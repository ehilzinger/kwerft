package server

import (
	"cmp"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/install"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/version"
)

// Clusters (docs/phase5.md, W3): the clusters this console manages, and the
// endpoint their agents dial.
//
//   - Owners and admins only. Cluster objects live in the management
//     cluster; every write is made as the signed-in user (impersonated) and
//     audited, Kubernetes RBAC (kwerft.dev "*") being the authority.
//   - Agent tokens are shown once, in the install command adopt and rotate
//     return. Only their SHA-256 is kept, as an annotation on the Cluster
//     (clusters.TokenHashAnnotation), written as the user, so no user
//     request needs the console's own rights to Secrets.
//   - /api/v1/clusters/connect is for agents only: no session, no browser
//     (an Origin header is refused), the token in Authorization; failures
//     are rate-limited per client address and audited.

// latestInstallerURL is the latest stable installer: the install command of
// development builds, which have no published copy of their own.
const latestInstallerURL = "https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/install.sh"

type clustersAPI struct {
	*api
	connectFails *limiter // refused agents per client address
}

func (a *api) registerClusters(mux *http.ServeMux) {
	c := &clustersAPI{api: a, connectFails: newLimiter(20, 15*time.Minute, a.now)}
	mux.HandleFunc("GET "+clusters.ConnectPath, c.connect)
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireRole(h, store.RoleOwner, store.RoleAdmin)))
	}
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	mux.HandleFunc("GET /api/v1/clusters", read(c.list))
	mux.HandleFunc("POST /api/v1/clusters", write(c.create))
	mux.HandleFunc("GET /api/v1/clusters/{name}", read(c.get))
	mux.HandleFunc("DELETE /api/v1/clusters/{name}", write(c.remove))
	mux.HandleFunc("POST /api/v1/clusters/{name}/token", write(c.rotate))
}

// ---- the agents' endpoint -----------------------------------------------------

func (c *clustersAPI) connect(w http.ResponseWriter, r *http.Request) {
	if c.cfg.Tunnel == nil {
		writeError(w, http.StatusNotFound, "This console does not accept cluster agents.")
		return
	}
	ip := clientIP(r)
	if c.connectFails.exceeded(ip) {
		writeError(w, http.StatusTooManyRequests, "Too many refused attempts. Wait 15 minutes and try again.")
		return
	}
	if err := c.cfg.Tunnel.Connect(w, r); errors.Is(err, clusters.ErrRejected) {
		c.connectFails.allow(ip)
		name, _ := clusters.AgentTokenCluster(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		c.audit(r, "anonymous", "cluster.agent_rejected", name, "")
	}
}

// ---- views --------------------------------------------------------------------

type clusterJSON struct {
	Name         string           `json:"name"`
	DisplayName  string           `json:"displayName,omitempty"`
	Provider     string           `json:"provider"`
	HetznerCloud *hetznerSpecJSON `json:"hetznerCloud,omitempty"`
	// Cloud: a hetzner-cloud cluster's Cloud Firewall and Load Balancer
	// (api_hcloud.go).
	Cloud        *clusterCloudJSON `json:"cloud,omitempty"`
	Phase        string            `json:"phase"`
	Message      string            `json:"message,omitempty"`
	Connected    bool              `json:"connected"`
	Deleting     bool              `json:"deleting,omitempty"`
	HasToken     bool              `json:"hasToken"`
	Nodes        int32             `json:"nodes"`
	ReadyNodes   int32             `json:"readyNodes"`
	Kubernetes   string            `json:"kubernetesVersion,omitempty"`
	AgentVersion string            `json:"agentVersion,omitempty"`
	LastSeen     *time.Time        `json:"lastSeen,omitempty"`
	CreatedAt    time.Time         `json:"createdAt"`
	Agent        *clusterAgentJSON `json:"agent,omitempty"`
	Pools        []clusterPoolJSON `json:"pools,omitempty"`
	// PublicAddresses: where a remote cluster's ingress is reached.
	PublicAddresses []string `json:"publicAddresses,omitempty"`
	// DNS: the records the console keeps for a remote cluster's hostnames
	// under its apps domain.
	DNS        *clusterDNSJSON    `json:"dns,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type clusterDNSJSON struct {
	Records  []dnsRecordJSON `json:"records"`
	Message  string          `json:"message,omitempty"`
	SyncedAt *time.Time      `json:"syncedAt,omitempty"`
}

type hetznerSpecJSON struct {
	Location      string `json:"location"`
	ServerType    string `json:"serverType"`
	ControlPlanes int32  `json:"controlPlanes"`
}

type clusterAgentJSON struct {
	Remote string    `json:"remote"`
	Since  time.Time `json:"since"`
	Error  string    `json:"error,omitempty"`
}

type clusterPoolJSON struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	ServerType string `json:"serverType"`
	Location   string `json:"location"`
	Count      int32  `json:"count"`
	ReadyNodes int32  `json:"readyNodes"`
}

func (c *clustersAPI) view(cl *kwerftv1.Cluster) clusterJSON {
	out := clusterJSON{
		Name: cl.Name, DisplayName: cl.Spec.DisplayName, Provider: string(cl.Spec.Provider),
		Phase: string(cl.Status.Phase), Deleting: cl.DeletionTimestamp != nil,
		HasToken: cl.Annotations[clusters.TokenHashAnnotation] != "",
		Nodes:    cl.Status.Nodes, ReadyNodes: cl.Status.ReadyNodes,
		Kubernetes: cl.Status.KubernetesVersion, AgentVersion: cl.Status.AgentVersion,
		CreatedAt: cl.CreationTimestamp.Time,
	}
	if h := cl.Spec.HetznerCloud; h != nil {
		out.HetznerCloud = &hetznerSpecJSON{Location: h.Location, ServerType: h.ServerType, ControlPlanes: cmp.Or(h.ControlPlanes, 1)}
		out.Cloud = clusterCloudView(cl)
	}
	if cl.Status.LastSeen != nil {
		t := cl.Status.LastSeen.Time
		out.LastSeen = &t
	}
	out.PublicAddresses = cl.Status.PublicAddresses
	if d := cl.Status.DNS; d != nil {
		out.DNS = &clusterDNSJSON{Records: dnsRecordsView(d.Records), Message: d.Message, SyncedAt: timePtr(d.SyncedAt)}
	}
	if cond := meta.FindStatusCondition(cl.Status.Conditions, controllers.ConditionReady); cond != nil {
		out.Message = cond.Message
	}
	if cl.Name == clusters.Local || cl.Spec.Provider == kwerftv1.ClusterLocal {
		out.Connected = true
	} else if c.cfg.Tunnel != nil {
		if st, ok := c.cfg.Tunnel.Agent(cl.Name); ok {
			// Live, fresher than the status the reconciler writes.
			out.Connected, out.Phase = true, string(controllers.ClusterConnected)
			out.Agent = &clusterAgentJSON{Remote: st.Remote, Since: st.Since, Error: st.Info.Error}
			if !st.LastSeen.IsZero() {
				t := st.LastSeen
				out.LastSeen = &t
			}
		}
	}
	return out
}

// mgmtClient acts as the signed-in user in the management cluster, where
// Cluster objects live.
func (c *clustersAPI) mgmtClient(r *http.Request) (client.Client, *principal, error) {
	p := principalOf(r)
	kc, err := c.cfg.Kube.For(p.user.Email, p.user.Role)
	return kc, p, err
}

func (c *clustersAPI) list(w http.ResponseWriter, r *http.Request) {
	kc, p, err := c.mgmtClient(r)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	var list kwerftv1.ClusterList
	if err := kc.List(r.Context(), &list); err != nil {
		c.kubeError(w, r, p, "cluster.list", "", "Clusters not found.", err)
		return
	}
	out := make([]clusterJSON, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, c.view(&list.Items[i]))
	}
	// Local first, then by name.
	slices.SortFunc(out, func(a, b clusterJSON) int {
		if (a.Name == clusters.Local) != (b.Name == clusters.Local) {
			if a.Name == clusters.Local {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

func (c *clustersAPI) get(w http.ResponseWriter, r *http.Request) {
	kc, p, err := c.mgmtClient(r)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	name := r.PathValue("name")
	var cl kwerftv1.Cluster
	if err := kc.Get(r.Context(), client.ObjectKey{Name: name}, &cl); err != nil {
		c.kubeError(w, r, p, "cluster.get", name, "Cluster not found.", err)
		return
	}
	out := c.view(&cl)
	out.Conditions = cl.Status.Conditions
	var pools kwerftv1.NodePoolList
	if err := kc.List(r.Context(), &pools); err == nil {
		for _, np := range pools.Items {
			if np.Spec.Cluster == name {
				out.Pools = append(out.Pools, clusterPoolJSON{Name: np.Name, Role: string(np.Spec.Role), ServerType: np.Spec.ServerType,
					Location: np.Spec.Location, Count: np.Spec.Count, ReadyNodes: np.Status.ReadyNodes})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- writes -------------------------------------------------------------------

// reservedClusterNames cannot name a remote cluster: the management cluster,
// and the agents' endpoint under /api/v1/clusters/.
var reservedClusterNames = map[string]bool{clusters.Local: true, "connect": true}

var hetznerLocationRE = regexp.MustCompile(`^[a-z]{3}[0-9]?$`)
var serverTypeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)

func (c *clustersAPI) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		DisplayName  string `json:"displayName"`
		Provider     string `json:"provider"`
		HetznerCloud *struct {
			Location      string `json:"location"`
			ServerType    string `json:"serverType"`
			ControlPlanes int32  `json:"controlPlanes"`
		} `json:"hetznerCloud"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	switch {
	case !clusters.ValidName(req.Name):
		writeFieldError(w, "name", "Use lowercase letters, digits and dashes, starting with a letter (at most 40 characters).")
		return
	case reservedClusterNames[req.Name]:
		writeFieldError(w, "name", "This name is reserved. Choose another.")
		return
	case len(req.DisplayName) > 80:
		writeFieldError(w, "displayName", "At most 80 characters.")
		return
	}
	cl := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: req.Name}, Spec: kwerftv1.ClusterSpec{DisplayName: req.DisplayName}}
	var token string
	switch kwerftv1.ClusterProvider(req.Provider) {
	case kwerftv1.ClusterHetznerCloud:
		h := req.HetznerCloud
		if h == nil {
			writeFieldError(w, "hetznerCloud", "Choose a location and a server type.")
			return
		}
		h.Location, h.ServerType = strings.TrimSpace(h.Location), strings.TrimSpace(h.ServerType)
		if !hetznerLocationRE.MatchString(h.Location) {
			writeFieldError(w, "location", "Choose a Hetzner Cloud location, such as fsn1.")
			return
		}
		if !serverTypeRE.MatchString(h.ServerType) {
			writeFieldError(w, "serverType", "Enter a Hetzner Cloud server type, such as cx32.")
			return
		}
		if h.ControlPlanes == 0 {
			h.ControlPlanes = 1
		}
		if h.ControlPlanes != 1 && h.ControlPlanes != 3 {
			writeFieldError(w, "controlPlanes", "Choose 1 control-plane server, or 3 for high availability.")
			return
		}
		cl.Spec.Provider = kwerftv1.ClusterHetznerCloud
		cl.Spec.HetznerCloud = &kwerftv1.HetznerClusterSpec{Location: h.Location, ServerType: h.ServerType, ControlPlanes: h.ControlPlanes}
		// Its token is the reconciler's: the first server's cloud-init needs it.
	case kwerftv1.ClusterAdopted:
		cl.Spec.Provider = kwerftv1.ClusterAdopted
		token = clusters.NewAgentToken(req.Name)
		cl.Annotations = map[string]string{clusters.TokenHashAnnotation: auth.HashToken(token)}
	default:
		writeFieldError(w, "provider", "Choose Hetzner Cloud, or an existing cluster to adopt.")
		return
	}
	kc, p, err := c.mgmtClient(r)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := kc.Create(r.Context(), cl); err != nil {
		c.kubeError(w, r, p, "cluster.create", req.Name, "Cluster not found.", err)
		return
	}
	c.audit(r, p.user.Email, "cluster.create", req.Name, string(cl.Spec.Provider))
	out := map[string]any{"cluster": c.view(cl)}
	if token != "" {
		out["token"], out["installCommand"] = token, c.installCommand(token)
	}
	writeJSON(w, http.StatusCreated, out)
}

func (c *clustersAPI) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == clusters.Local {
		writeError(w, http.StatusBadRequest, "The cluster the console runs in cannot be deleted.")
		return
	}
	kc, p, err := c.mgmtClient(r)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	var cl kwerftv1.Cluster
	if err := kc.Get(r.Context(), client.ObjectKey{Name: name}, &cl); err != nil {
		c.kubeError(w, r, p, "cluster.delete", name, "Cluster not found.", err)
		return
	}
	if cl.Spec.Provider == kwerftv1.ClusterLocal {
		writeError(w, http.StatusBadRequest, "The cluster the console runs in cannot be deleted.")
		return
	}
	if err := kc.Delete(r.Context(), &cl, client.Preconditions{UID: &cl.UID}); err != nil {
		c.kubeError(w, r, p, "cluster.delete", name, "Cluster not found.", err)
		return
	}
	if c.cfg.Tunnel != nil {
		c.cfg.Tunnel.Disconnect(name)
	}
	c.audit(r, p.user.Email, "cluster.delete", name, string(cl.Spec.Provider))
	w.WriteHeader(http.StatusNoContent)
}

// rotate gives the cluster a new agent token: the old one stops working at
// once, and the agent connected with it is disconnected. The new token is
// shown once, in the install command, which updates the agent when run on
// one of the cluster's servers (`install.sh --agent` is idempotent).
func (c *clustersAPI) rotate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == clusters.Local {
		writeError(w, http.StatusBadRequest, "The cluster the console runs in has no agent.")
		return
	}
	kc, p, err := c.mgmtClient(r)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	var cl kwerftv1.Cluster
	if err := kc.Get(r.Context(), client.ObjectKey{Name: name}, &cl); err != nil {
		c.kubeError(w, r, p, "cluster.token_rotate", name, "Cluster not found.", err)
		return
	}
	if cl.Spec.Provider == kwerftv1.ClusterLocal {
		writeError(w, http.StatusBadRequest, "The cluster the console runs in has no agent.")
		return
	}
	if cl.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "This cluster is being deleted.")
		return
	}
	token := clusters.NewAgentToken(name)
	patch := client.MergeFromWithOptions(cl.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if cl.Annotations == nil {
		cl.Annotations = map[string]string{}
	}
	cl.Annotations[clusters.TokenHashAnnotation] = auth.HashToken(token)
	if err := kc.Patch(r.Context(), &cl, patch); err != nil {
		c.kubeError(w, r, p, "cluster.token_rotate", name, "Cluster not found.", err)
		return
	}
	if c.cfg.Tunnel != nil {
		c.cfg.Tunnel.DisconnectUnless(name, auth.HashToken(token))
	}
	c.audit(r, p.user.Email, "cluster.token_rotate", name, "")
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "installCommand": c.installCommand(token)})
}

// installCommand installs Kwerft in agent mode on a cluster's first server
// (or updates the token on an existing one).
func (c *clustersAPI) installCommand(token string) string {
	return agentInstallCommand(c.consoleDomain(), token, version.Version)
}

// agentInstallCommand fetches the installer of the console's own release,
// release candidates included (the latest stable one may predate --agent),
// and pins that version, so the agent matches the console. Development
// builds get the latest stable installer and its default version.
func agentInstallCommand(consoleDomain, token, ver string) string {
	url := install.ReleaseURL(ver)
	args := "--agent --console https://" + consoleDomain + " --cluster-token " + token
	if url == "" {
		url = latestInstallerURL
	} else {
		args += " --version " + strings.TrimPrefix(ver, "v")
	}
	return "curl -fsSL " + url + " | sudo bash -s -- " + args
}
