package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"path"
	"slices"
	"strings"
)

// The Kubernetes proxy at /k8s/: kubectl with a downloaded kubeconfig
// (api_tokens.go) talks to the API server through the console, as the
// token's user. The API server itself stays private.
//
//   - Only API tokens authenticate here, never cookies: no browser can be
//     tricked into sending a request, so no CSRF check is needed.
//   - Requests go to the API server with the console's credentials and the
//     user's identity, as for the console: user "kwerft:<email>", groups
//     "kwerft:role:<role>" (the token's effective role) and
//     "system:authenticated". Kubernetes RBAC decides, as everywhere.
//   - Client-supplied Authorization and Impersonate-* headers are removed
//     before anything else; client-go's round trippers would otherwise pass
//     a request that already carries them through untouched.
//   - exec, attach, port-forward and the proxy subresources are refused, as
//     is every protocol upgrade: shells go through the console, where they
//     are recorded. Reads, writes, watches and log streams (kubectl logs -f)
//     work.
//   - A project-restricted token reaches only its projects' namespaces (and
//     its Project objects), plus discovery and self-reviews ("kubectl auth
//     can-i"). Cluster-wide lists are refused even where RBAC would allow
//     them, so a restriction holds for every role.
//   - Secrets are refused altogether. No role reads them, and some are
//     write-only (the DNS token, the single sign-on client secret, Git and
//     notification credentials: "patch", never "get") — but a patch answers
//     with the whole object, so through kubectl a patch would read them.
//   - Writes are audited (kube.write), refusals too (kube.denied).
//   - /k8s/... is the management cluster; /k8s/clusters/<name>/... another
//     cluster the console manages (docs/phase5.md), reached through its
//     agent with the same rules. A project-restricted token reaches a
//     project's namespace only in the cluster the project lives in.

const kubeProxyPrefix = "/k8s"

// kubeClusterPrefix starts a path to another cluster: /k8s/clusters/<name>.
// No Kubernetes API path starts with /clusters.
const kubeClusterPrefix = "/clusters/"

// maxKubeBody bounds a request body; the API server's own limit is 3 MiB
// per object, and a List of them is a little more.
const maxKubeBody = 16 << 20

// kubeDeniedSubresources reach into containers or nodes without recording.
var kubeDeniedSubresources = []string{"exec", "attach", "portforward", "proxy"}

// kubeSelfReviews may be created by any token: they only ask what the
// caller may do (kubectl auth can-i, kubectl auth whoami).
var kubeSelfReviews = []string{
	"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews",
	"/apis/authorization.k8s.io/v1/selfsubjectrulesreviews",
	"/apis/authentication.k8s.io/v1/selfsubjectreviews",
}

// kubeStatus answers in the API server's own error format, which kubectl
// prints as a sentence.
func kubeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
		"status": "Failure", "message": msg, "reason": reason, "code": code,
	})
}

// kubeRequest is what the proxy needs to know about a request path.
type kubeRequest struct {
	path        string // without the /k8s prefix, cleaned
	namespace   string // set for namespaced resources and for a namespace itself
	resource    string // e.g. pods, or namespaces for a namespace itself
	name        string
	subresource string
	group       string // "" for the core group
	discovery   bool   // /api, /apis, /version, /openapi, group discovery
}

// parseKubePath reads a Kubernetes API path. ok is false for paths the proxy
// refuses to interpret (encoded, unclean, legacy /watch/).
func parseKubePath(raw string) (kubeRequest, bool) {
	k := kubeRequest{path: raw}
	if raw == "" || raw[0] != '/' || path.Clean(raw) != raw || strings.Contains(raw, "//") {
		return k, false
	}
	segs := strings.Split(raw[1:], "/")
	switch {
	case raw == "/api" || raw == "/apis" || raw == "/version" || raw == "/openapi/v2" || raw == "/openapi/v3" || strings.HasPrefix(raw, "/openapi/v3/"):
		k.discovery = true
		return k, true
	case segs[0] == "api" && len(segs) >= 2:
		segs = segs[2:] // api/v1
	case segs[0] == "apis" && len(segs) <= 3:
		k.discovery = true // apis/<group> and apis/<group>/<version>
		return k, true
	case segs[0] == "apis":
		k.group = segs[1]
		segs = segs[3:] // apis/<group>/<version>
	default:
		return k, false
	}
	if len(segs) == 0 {
		k.discovery = true // api/v1
		return k, true
	}
	if segs[0] == "watch" {
		return k, false // the deprecated /watch/ prefix; kubectl uses ?watch=true
	}
	if segs[0] == "namespaces" && len(segs) >= 2 {
		k.namespace = segs[1]
		if len(segs) == 2 || (len(segs) == 3 && (segs[2] == "status" || segs[2] == "finalize")) {
			k.resource, k.name = "namespaces", segs[1]
			if len(segs) == 3 {
				k.subresource = segs[2]
			}
			return k, true
		}
		segs = segs[2:]
	}
	k.resource = segs[0]
	if len(segs) >= 2 {
		k.name = segs[1]
	}
	if len(segs) >= 3 {
		k.subresource = segs[2]
	}
	return k, true
}

// allowedFor reports whether a project-restricted token may make this
// request.
func (k kubeRequest) allowedFor(method string, projects []string) bool {
	read := method == http.MethodGet || method == http.MethodHead
	switch {
	case k.discovery:
		return read
	case method == http.MethodPost && slices.Contains(kubeSelfReviews, k.path):
		return true
	case k.resource == "namespaces" && k.namespace != "":
		return read && slices.Contains(projects, k.namespace)
	case k.namespace != "":
		return slices.Contains(projects, k.namespace)
	case k.group == "kwerft.dev" && k.resource == "projects" && k.name != "" && k.subresource == "":
		return read && slices.Contains(projects, k.name)
	}
	return false
}

func (a *api) registerKubeProxy(mux *http.ServeMux) {
	mux.HandleFunc(kubeProxyPrefix+"/", a.kubeProxy)
}

func (a *api) kubeProxy(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Kube == nil {
		kubeStatus(w, http.StatusServiceUnavailable, "ServiceUnavailable", "This Kwerft console is not connected to a Kubernetes cluster.")
		return
	}
	if !hasAuthorization(r) {
		kubeStatus(w, http.StatusUnauthorized, "Unauthorized",
			"Kwerft's Kubernetes proxy needs an API token. Download a kubeconfig under Account › API tokens.")
		return
	}
	p, ok := a.tokenPrincipal(w, r)
	if !ok {
		return
	}
	deny := func(reason, msg string) {
		a.audit(r, p.user.Email, "kube.denied", r.Method+" "+r.URL.Path, p.token.Name+": "+reason)
		kubeStatus(w, http.StatusForbidden, "Forbidden", msg)
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "impersonate-") {
			// kubectl --as: the token's identity is all a request gets.
			deny("impersonation headers", "Kwerft's Kubernetes proxy acts as you; impersonating someone else (--as, --as-group) is not allowed.")
			return
		}
	}
	if r.URL.RawPath != "" {
		deny("encoded path", "Kwerft's Kubernetes proxy does not accept encoded paths.")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, kubeProxyPrefix)
	conn := a.clusters.local
	if after, ok := strings.CutPrefix(rest, kubeClusterPrefix); ok {
		name, path, _ := strings.Cut(after, "/")
		c, err := a.clusters.byName(name)
		if err != nil {
			code, reason := http.StatusServiceUnavailable, "ServiceUnavailable"
			var unknown *clusterUnknownError
			if errors.As(err, &unknown) {
				code, reason = http.StatusNotFound, "NotFound"
			}
			kubeStatus(w, code, reason, err.Error())
			return
		}
		conn, rest = c, "/"+path
	}
	k, ok := parseKubePath(rest)
	if !ok {
		deny("unsupported path", "Kwerft's Kubernetes proxy does not serve this path.")
		return
	}
	if slices.Contains(kubeDeniedSubresources, k.subresource) || r.Header.Get("Upgrade") != "" ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		deny(k.resource+"/"+k.subresource+" or upgrade",
			"exec, attach, port-forward and proxying are not available through Kwerft's kubeconfig. Open a shell in the console instead: it is recorded.")
		return
	}
	if k.group == "" && k.resource == "secrets" {
		deny("secrets", "Secrets are not available through Kwerft's kubeconfig: the console keeps credentials write-only.")
		return
	}
	if p.token.Projects != nil && (!k.allowedFor(r.Method, p.token.Projects) || !a.inItsCluster(r.Context(), k, conn)) {
		deny("outside the token's projects", "This token is limited to the projects "+strings.Join(p.token.Projects, ", ")+
			"; it reaches only their namespaces.")
		return
	}
	target, rt, err := conn.kube.Upstream(p.user.Email, p.user.Role)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxKubeBody)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + k.path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			stripClientCredentials(pr.Out.Header)
		},
		Transport:     rt,
		FlushInterval: -1, // watches and log streams
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			a.cfg.Logger.Error("kubernetes proxy", "path", k.path, "err", err)
			kubeStatus(w, http.StatusBadGateway, "ServiceUnavailable", "Kwerft could not reach the Kubernetes API server.")
		},
	}
	write := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
		!slices.Contains(kubeSelfReviews, k.path)
	if !write {
		proxy.ServeHTTP(w, r)
		return
	}
	// Recorded as the status goes out, before kubectl sees any of the
	// answer (as every other write is audited before it is answered), and
	// even when kubectl has hung up by now.
	ctx := context.WithoutCancel(r.Context())
	aw := &auditOnStatus{ResponseWriter: w, audit: func(status int) {
		a.audit(r.WithContext(ctx), p.user.Email, "kube.write", r.Method+" "+k.path, p.token.Name+", "+http.StatusText(status))
	}}
	proxy.ServeHTTP(aw, r)
	aw.record(http.StatusOK) // the proxy wrote nothing at all
}

// auditOnStatus calls audit once, with the final status, before that status
// reaches the client.
type auditOnStatus struct {
	http.ResponseWriter
	audit func(status int)
	done  bool
}

func (w *auditOnStatus) record(status int) {
	if !w.done {
		w.done = true
		w.audit(status)
	}
}

func (w *auditOnStatus) WriteHeader(code int) {
	if code >= 200 { // 1xx (100 Continue, 103 Early Hints) are not the answer
		w.record(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditOnStatus) Write(b []byte) (int, error) {
	if !w.done {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets the proxy flush watches through http.ResponseController.
func (w *auditOnStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// inItsCluster: a request a project-restricted token may make (allowedFor)
// goes to the cluster its project lives in, so a namespace of the same name
// elsewhere stays out of reach. Discovery and self-reviews go anywhere.
func (a *api) inItsCluster(ctx context.Context, k kubeRequest, conn *clusterConn) bool {
	project := k.namespace
	if project == "" && k.group == "kwerft.dev" && k.resource == "projects" {
		project = k.name
	}
	if project == "" {
		return true
	}
	home, err := a.clusters.forProject(ctx, project)
	return err == nil && home == conn
}

// stripClientCredentials removes what a client must never pass to the API
// server: its own credentials (the console adds its service account's) and
// any identity it claims (the console adds the user's).
func stripClientCredentials(h http.Header) {
	for name := range h {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "cookie" || lower == "proxy-authorization" ||
			strings.HasPrefix(lower, "impersonate-") || strings.HasPrefix(lower, "x-remote-") ||
			strings.HasPrefix(lower, "x-forwarded-") || lower == "x-real-ip" {
			h.Del(name)
		}
	}
}
