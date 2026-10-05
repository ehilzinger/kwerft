package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/install"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/jointoken"
	"github.com/ehilzinger/kwerft/internal/kube"
	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/version"
)

// Nodes and node pools (docs/phase5.md, W2).
//
// Who decides:
//   - Owners and admins only. NodePools live in the management cluster and
//     are written as the signed-in user (impersonated), so Kubernetes RBAC
//     (kwerft.dev "*") stays the authority; every write is audited.
//   - Nodes are read, drained and marked for removal as the user too, in the
//     cluster they belong to (roles.yaml gives owners and admins nodes get,
//     list and patch). The reconcilers do the draining with their own
//     identity because the user asked through an annotation:
//     kwerft.dev/drain or kwerft.dev/remove on the Node, and
//     kwerft.dev/remove-servers on a NodePool for servers that never joined.
//   - Join commands carry a signed join token (internal/jointoken), not a
//     Kubernetes credential; control-plane join commands are owners' only,
//     because the k3s server token they lead to is root on the cluster.
//   - GET /api/v1/join is public: the join token is the credential. It is
//     rate limited per address, audited, and answers with the k3s join
//     material only (a bootstrap token for workers).
//   - Coupling a vSwitch takes Robot credentials in the request, used once
//     and never stored (owners only).

type nodesAPI struct {
	*api
	signer *jointoken.Signer
	// hcloud reaches the Cloud API with the stored token; nil when the
	// console has no Kubernetes identity of its own.
	hcloud func(ctx context.Context) (*hetzner.Client, error)
	// robotBase overrides the Robot webservice (tests).
	robotBase string
	joinLim   *limiter
	// consoleURL is https://<console host> for join commands.
	consoleURL func(*http.Request) string
}

func (a *api) registerNodes(mux *http.ServeMux) {
	n := &nodesAPI{api: a, signer: jointoken.NewSigner(a.cfg.DataKey), joinLim: newLimiter(30, 15*time.Minute, a.now)}
	if a.cfg.SystemReader != nil {
		n.hcloud = controllers.HCloudFromSecret(a.cfg.SystemReader, controllers.GatewayNamespace)
	}
	n.consoleURL = func(r *http.Request) string {
		host := a.cfg.ConsoleDomain
		if a.cfg.ActiveConsoleDomain != nil {
			host = cmp.Or(a.cfg.ActiveConsoleDomain(), host)
		}
		if host == "" || a.cfg.InsecureCookies {
			scheme := "https"
			if r.TLS == nil && a.cfg.InsecureCookies {
				scheme = "http"
			}
			return scheme + "://" + cmp.Or(host, r.Host)
		}
		return "https://" + host
	}
	if a.cfg.nodesHook != nil {
		a.cfg.nodesHook(n)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireRole(h, store.RoleOwner, store.RoleAdmin)))
	}
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	owner := func(h http.HandlerFunc) http.HandlerFunc {
		return a.sameOrigin(a.requireUser(a.requireKube(a.requireRole(h, store.RoleOwner))))
	}
	mux.HandleFunc("GET /api/v1/clusters/{cluster}/nodes", read(n.list))
	mux.HandleFunc("POST /api/v1/clusters/{cluster}/pools", write(n.poolCreate))
	mux.HandleFunc("PATCH /api/v1/clusters/{cluster}/pools/{pool}", write(n.poolUpdate))
	mux.HandleFunc("DELETE /api/v1/clusters/{cluster}/pools/{pool}", write(n.poolDelete))
	mux.HandleFunc("POST /api/v1/clusters/{cluster}/pools/{pool}/servers/{server}/remove", write(n.serverRemove))
	mux.HandleFunc("POST /api/v1/clusters/{cluster}/nodes/{node}/drain", write(n.nodeDrain))
	mux.HandleFunc("POST /api/v1/clusters/{cluster}/nodes/{node}/uncordon", write(n.nodeUncordon))
	mux.HandleFunc("POST /api/v1/clusters/{cluster}/nodes/{node}/remove", write(n.nodeRemove))
	mux.HandleFunc("POST /api/v1/clusters/{cluster}/join-command", write(n.joinCommand))
	mux.HandleFunc("POST /api/v1/clusters/{cluster}/vswitch", owner(n.vswitch))
	mux.HandleFunc("GET /api/v1/hetzner/catalog", read(n.catalog))

	// For servers joining: no session, the join token is the credential.
	mux.HandleFunc("GET /api/v1/join", n.join)
	mux.HandleFunc("GET /join.sh", n.serveJoinScript)
	mux.HandleFunc("GET /install.sh", n.serveInstaller)
}

// ---- views ---------------------------------------------------------------------------

type poolNodeJSON struct {
	Name       string `json:"name"`
	ServerID   int64  `json:"serverId,omitempty"`
	PublicIP   string `json:"publicIp,omitempty"`
	PrivateIP  string `json:"privateIp,omitempty"`
	ServerType string `json:"serverType,omitempty"`
	Phase      string `json:"phase"`
	Message    string `json:"message,omitempty"`
}

type poolJSON struct {
	Name           string            `json:"name"` // the NodePool object
	Pool           string            `json:"pool"` // shown: without the "<cluster>-" prefix
	Role           string            `json:"role"`
	ServerType     string            `json:"serverType"`
	Location       string            `json:"location"`
	Count          int32             `json:"count"`
	Desired        int32             `json:"desired"`
	Ready          int32             `json:"ready"`
	Labels         map[string]string `json:"labels"`
	ScaleDownAfter int               `json:"scaleDownAfterMinutes,omitempty"`
	Deleting       bool              `json:"deleting"`
	State          string            `json:"state"` // ready | progressing | failed
	Reason         string            `json:"reason,omitempty"`
	Message        string            `json:"message,omitempty"`
	Servers        []poolNodeJSON    `json:"servers"`
}

type nodeJSON struct {
	Name          string     `json:"name"`
	Roles         []string   `json:"roles"`
	Pool          string     `json:"pool,omitempty"`
	Ready         bool       `json:"ready"`
	Status        string     `json:"status"` // Ready | NotReady | Draining | Drained | Removing
	Message       string     `json:"message,omitempty"`
	InternalIP    string     `json:"internalIp,omitempty"`
	ExternalIP    string     `json:"externalIp,omitempty"`
	Kubelet       string     `json:"kubeletVersion,omitempty"`
	OS            string     `json:"os,omitempty"`
	CPU           string     `json:"cpu,omitempty"`
	Memory        string     `json:"memory,omitempty"`
	Platform      string     `json:"platform,omitempty"` // cloud | dedicated
	Unschedulable bool       `json:"unschedulable"`
	Created       *time.Time `json:"created,omitempty"`
}

type nodesJSON struct {
	Cluster  string `json:"cluster"`
	Provider string `json:"provider"`
	// Reachable: the cluster's API answered (nodes are listed).
	Reachable bool       `json:"reachable"`
	Problem   string     `json:"problem,omitempty"`
	Pools     []poolJSON `json:"pools"`
	Nodes     []nodeJSON `json:"nodes"`
	// Joinable: the cluster has join material, so join commands work.
	Joinable bool `json:"joinable"`
	// Cloud: a Hetzner Cloud token is stored (pools can create servers).
	Cloud bool `json:"cloud"`
	// ControlPlanes is the number of control-plane nodes now.
	ControlPlanes int `json:"controlPlanes"`
}

func shortPool(cluster, name string) string { return strings.TrimPrefix(name, cluster+"-") }

func toPoolJSON(p *kwerftv1.NodePool) poolJSON {
	out := poolJSON{
		Name: p.Name, Pool: shortPool(p.Spec.Cluster, p.Name), Role: string(cmp.Or(p.Spec.Role, kwerftv1.NodeWorker)),
		ServerType: p.Spec.ServerType, Location: p.Spec.Location, Count: p.Spec.Count, Desired: p.Status.Desired,
		Ready: p.Status.ReadyNodes, Labels: p.Spec.Labels, Deleting: p.DeletionTimestamp != nil, State: "progressing",
		Servers: []poolNodeJSON{},
	}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	if p.Spec.ScaleDownAfter != nil {
		out.ScaleDownAfter = int(p.Spec.ScaleDownAfter.Minutes())
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, controllers.ConditionReady); c != nil && c.ObservedGeneration == p.Generation {
		out.Reason, out.Message = c.Reason, c.Message
		switch {
		case c.Status == metav1.ConditionTrue:
			out.State = "ready"
		case c.Reason != "Progressing" && c.Reason != "ReconcileFailed":
			out.State = "failed"
		}
	}
	for _, n := range p.Status.Nodes {
		out.Servers = append(out.Servers, poolNodeJSON{Name: n.Name, ServerID: n.ServerID, PublicIP: n.PublicIP, PrivateIP: n.PrivateIP,
			ServerType: n.ServerType, Phase: n.Phase, Message: n.Message})
	}
	return out
}

func toNodeJSON(n *corev1.Node) nodeJSON {
	out := nodeJSON{Name: n.Name, Ready: controllers.NodeReady(n), Pool: n.Labels[hetzner.LabelPool], Kubelet: n.Status.NodeInfo.KubeletVersion,
		OS: n.Status.NodeInfo.OSImage, Platform: n.Labels["kwerft.dev/platform"], Unschedulable: n.Spec.Unschedulable, Roles: []string{}}
	if !n.CreationTimestamp.IsZero() {
		t := n.CreationTimestamp.UTC()
		out.Created = &t
	}
	if controllers.IsControlPlane(n) {
		out.Roles = append(out.Roles, "control-plane")
	}
	if n.Labels[controllers.LabelBuildNode] == "true" {
		out.Roles = append(out.Roles, "builds")
	}
	if len(out.Roles) == 0 {
		out.Roles = append(out.Roles, "worker")
	}
	for _, a := range n.Status.Addresses {
		switch a.Type {
		case corev1.NodeInternalIP:
			out.InternalIP = cmp.Or(out.InternalIP, a.Address)
		case corev1.NodeExternalIP:
			out.ExternalIP = cmp.Or(out.ExternalIP, a.Address)
		}
	}
	if q, ok := n.Status.Capacity[corev1.ResourceCPU]; ok {
		out.CPU = q.String()
	}
	if q, ok := n.Status.Capacity[corev1.ResourceMemory]; ok {
		out.Memory = fmt.Sprintf("%.1f GiB", float64(q.Value())/(1<<30))
	}
	out.Status = "NotReady"
	if out.Ready {
		out.Status = "Ready"
	}
	out.Message = n.Annotations[controllers.AnnotationDrainStatus]
	switch {
	case n.Annotations[controllers.AnnotationNodeRemove] != "":
		out.Status = "Removing"
	case n.Annotations[controllers.AnnotationNodeDrain] != "" && out.Message == "Drained.":
		out.Status = "Drained"
	case n.Annotations[controllers.AnnotationNodeDrain] != "" || n.Annotations[controllers.AnnotationDrainStarted] != "":
		out.Status = "Draining"
	case n.Spec.Unschedulable:
		out.Status = "Cordoned"
	}
	return out
}

// ---- clients per cluster -------------------------------------------------------------------

// clusterOf checks that the cluster exists (the user reads it) and returns
// its provider. "local" exists before its Cluster object does.
func (n *nodesAPI) clusterOf(ctx context.Context, c client.Client, name string) (kwerftv1.ClusterProvider, error) {
	var cl kwerftv1.Cluster
	err := c.Get(ctx, client.ObjectKey{Name: name}, &cl)
	switch {
	case err == nil:
		return cl.Spec.Provider, nil
	case apierrors.IsNotFound(err) && name == clusters.Local:
		return kwerftv1.ClusterLocal, nil
	}
	return "", err
}

// remoteConfig reaches a remote cluster with Kwerft's identity there.
func (n *nodesAPI) remoteConfig(cluster string) (*rest.Config, error) {
	if n.cfg.Clusters == nil {
		return nil, clusters.ErrUnavailable
	}
	return n.cfg.Clusters.RESTConfig(cluster)
}

// userIn returns a client acting as the signed-in user in the cluster.
func (n *nodesAPI) userIn(r *http.Request, cluster string) (client.Client, error) {
	p := principalOf(r)
	if cluster == clusters.Local {
		return n.cfg.Kube.For(p.user.Email, p.user.Role)
	}
	cfg, err := n.remoteConfig(cluster)
	if err != nil {
		return nil, err
	}
	cfg.Impersonate = rest.ImpersonationConfig{UserName: kube.UserName(p.user.Email), Groups: []string{kube.RoleGroup(p.user.Role), kube.Authenticated}}
	return client.New(cfg, client.Options{Scheme: controllers.NewScheme()})
}

// systemIn returns a client with Kwerft's own identity in the cluster.
func (n *nodesAPI) systemIn(cluster string) (client.Client, error) {
	if cluster == clusters.Local {
		if n.cfg.System == nil {
			return nil, clusters.ErrUnavailable
		}
		return n.cfg.System, nil
	}
	cfg, err := n.remoteConfig(cluster)
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: controllers.NewScheme()})
}

func (n *nodesAPI) unreachable(w http.ResponseWriter, cluster string) {
	writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("Cluster %s cannot be reached right now: its agent is not connected.", cluster))
}

// ---- list ---------------------------------------------------------------------------------

func (n *nodesAPI) list(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("cluster")
	c, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	provider, err := n.clusterOf(ctx, c, name)
	if err != nil {
		n.kubeError(w, r, p, "cluster.read", name, fmt.Sprintf("Cluster %q not found.", name), err)
		return
	}
	out := nodesJSON{Cluster: name, Provider: string(provider), Pools: []poolJSON{}, Nodes: []nodeJSON{}}
	var pools kwerftv1.NodePoolList
	if err := c.List(ctx, &pools); err != nil {
		n.kubeError(w, r, p, "nodepool.list", name, "", err)
		return
	}
	for i := range pools.Items {
		if pools.Items[i].Spec.Cluster == name {
			out.Pools = append(out.Pools, toPoolJSON(&pools.Items[i]))
		}
	}
	slices.SortFunc(out.Pools, func(a, b poolJSON) int { return strings.Compare(a.Pool, b.Pool) })

	if uc, err := n.userIn(r, name); err != nil {
		out.Problem = "The cluster cannot be reached right now."
	} else {
		var nodes corev1.NodeList
		if err := uc.List(ctx, &nodes); err != nil {
			if apierrors.IsForbidden(err) {
				n.kubeError(w, r, p, "node.list", name, "", err)
				return
			}
			out.Problem = "The cluster's Kubernetes API did not answer: " + truncate(err.Error(), 200)
		} else {
			out.Reachable = true
			for i := range nodes.Items {
				out.Nodes = append(out.Nodes, toNodeJSON(&nodes.Items[i]))
				if controllers.IsControlPlane(&nodes.Items[i]) {
					out.ControlPlanes++
				}
			}
			slices.SortFunc(out.Nodes, func(a, b nodeJSON) int { return strings.Compare(a.Name, b.Name) })
		}
	}
	if n.cfg.SystemReader != nil {
		var sec corev1.Secret
		err := n.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.JoinSecretName(name)}, &sec)
		out.Joinable = err == nil && len(sec.Data["token"]) > 0
		err = n.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: "kwerft-hcloud-token"}, &sec)
		out.Cloud = err == nil && len(sec.Data["token"]) > 0
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- pools --------------------------------------------------------------------------------

type poolInput struct {
	Name           string            `json:"name"`
	Role           string            `json:"role"`
	ServerType     string            `json:"serverType"`
	Location       string            `json:"location"`
	Count          *int32            `json:"count"`
	Labels         map[string]string `json:"labels"`
	ScaleDownAfter *int              `json:"scaleDownAfterMinutes"`
}

var poolNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func poolNotFound(name string) string { return fmt.Sprintf("Node pool %q not found.", name) }

// controlPlanes counts the cluster's control-plane nodes outside pool, or
// -1 when the cluster cannot be reached.
func (n *nodesAPI) controlPlanesOutside(r *http.Request, ctx context.Context, cluster, pool string) int {
	uc, err := n.userIn(r, cluster)
	if err != nil {
		return -1
	}
	var nodes corev1.NodeList
	if err := uc.List(ctx, &nodes); err != nil {
		return -1
	}
	count := 0
	for i := range nodes.Items {
		if controllers.IsControlPlane(&nodes.Items[i]) && (pool == "" || nodes.Items[i].Labels[hetzner.LabelPool] != pool) {
			count++
		}
	}
	return count
}

// checkControlPlaneCount refuses a control-plane pool size that would leave
// the cluster with an even number of control-plane nodes, or none.
func checkControlPlaneCount(w http.ResponseWriter, outside int, count int32) bool {
	total := int(count)
	if outside > 0 {
		total += outside
	}
	switch {
	case total == 0:
		writeFieldError(w, "count", "The cluster needs at least one control-plane node.")
		return false
	case total%2 == 0 && outside >= 0:
		writeFieldError(w, "count", fmt.Sprintf("That makes %d control-plane nodes. etcd needs an odd number (1, 3 or 5): with an even one, losing a single node can stop the cluster.", total))
		return false
	case outside < 0 && count%2 == 0:
		writeFieldError(w, "count", "Use an odd number of control-plane nodes (1, 3 or 5).")
		return false
	}
	return true
}

// checkCatalog validates the server type and location against the Cloud
// API when a token is stored.
func (n *nodesAPI) checkCatalog(w http.ResponseWriter, ctx context.Context, serverType, location string) bool {
	if n.hcloud == nil {
		return true
	}
	hc, err := n.hcloud(ctx)
	if err != nil {
		writeError(w, http.StatusConflict, "Add a Hetzner Cloud API token under Settings › Hetzner Cloud first: node pools create Cloud servers with it.")
		return false
	}
	types, err := hc.ServerTypes(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "The Hetzner Cloud API did not answer: "+truncate(err.Error(), 200))
		return false
	}
	t, ok := hetzner.FindServerType(types, serverType)
	switch {
	case !ok:
		writeFieldError(w, "serverType", fmt.Sprintf("Hetzner Cloud has no server type %q.", serverType))
		return false
	case !t.AvailableIn(location):
		writeFieldError(w, "serverType", fmt.Sprintf("%s cannot be ordered in %s right now.", serverType, location))
		return false
	case t.Memory < 4:
		writeFieldError(w, "serverType", "Nodes need at least 4 GB of memory.")
		return false
	}
	return true
}

func validLabels(w http.ResponseWriter, labels map[string]string) bool {
	if len(labels) > 20 {
		writeFieldError(w, "labels", "At most 20 labels.")
		return false
	}
	for k, v := range labels {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			writeFieldError(w, "labels", fmt.Sprintf("Label %q: %s", k, errs[0]))
			return false
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			writeFieldError(w, "labels", fmt.Sprintf("Label %q: %s", k, errs[0]))
			return false
		}
		if strings.HasPrefix(k, "kwerft.dev/") || strings.Contains(k, "kubernetes.io/") || strings.Contains(k, "k8s.io/") {
			writeFieldError(w, "labels", fmt.Sprintf("Label %q: kwerft.dev, kubernetes.io and k8s.io labels are reserved.", k))
			return false
		}
	}
	return true
}

func poolDetail(spec kwerftv1.NodePoolSpec) string {
	return fmt.Sprintf("%s pool: %d × %s in %s", cmp.Or(spec.Role, kwerftv1.NodeWorker), spec.Count, spec.ServerType, spec.Location)
}

func (n *nodesAPI) poolCreate(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	var in poolInput
	if !decodeStrict(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !poolNameRE.MatchString(in.Name) || len(cluster)+1+len(in.Name) > 50 {
		writeFieldError(w, "name", "Use lowercase letters, digits and dashes; cluster and pool name together at most 50 characters.")
		return
	}
	role := kwerftv1.NodeRole(cmp.Or(in.Role, string(kwerftv1.NodeWorker)))
	if role != kwerftv1.NodeWorker && role != kwerftv1.NodeControlPlane && role != kwerftv1.NodeBuilds {
		writeFieldError(w, "role", "Choose worker, control-plane or builds.")
		return
	}
	if in.Count == nil || *in.Count < 0 || *in.Count > 50 {
		writeFieldError(w, "count", "Between 0 and 50 servers.")
		return
	}
	if strings.TrimSpace(in.ServerType) == "" {
		writeFieldError(w, "serverType", "Choose a server type.")
		return
	}
	if strings.TrimSpace(in.Location) == "" {
		writeFieldError(w, "location", "Choose a location.")
		return
	}
	if !validLabels(w, in.Labels) {
		return
	}
	c, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	if _, err := n.clusterOf(ctx, c, cluster); err != nil {
		n.kubeError(w, r, p, "nodepool.create", cluster, fmt.Sprintf("Cluster %q not found.", cluster), err)
		return
	}
	name := cluster + "-" + in.Name
	if role == kwerftv1.NodeControlPlane && !checkControlPlaneCount(w, n.controlPlanesOutside(r, ctx, cluster, name), *in.Count) {
		return
	}
	if !n.checkCatalog(w, ctx, in.ServerType, in.Location) {
		return
	}
	pool := &kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: kwerftv1.NodePoolSpec{
		Cluster: cluster, Role: role, ServerType: in.ServerType, Location: in.Location, Count: *in.Count, Labels: in.Labels,
	}}
	if role == kwerftv1.NodeBuilds && in.ScaleDownAfter != nil {
		if *in.ScaleDownAfter < 5 || *in.ScaleDownAfter > 24*60 {
			writeFieldError(w, "scaleDownAfterMinutes", "Between 5 minutes and 24 hours.")
			return
		}
		pool.Spec.ScaleDownAfter = &metav1.Duration{Duration: time.Duration(*in.ScaleDownAfter) * time.Minute}
	}
	if err := c.Create(ctx, pool); err != nil {
		n.kubeError(w, r, p, "nodepool.create", name, poolNotFound(in.Name), err)
		return
	}
	n.audit(r, p.user.Email, "nodepool.create", name, poolDetail(pool.Spec))
	writeJSON(w, http.StatusCreated, toPoolJSON(pool))
}

func (n *nodesAPI) poolGet(w http.ResponseWriter, r *http.Request, ctx context.Context, c client.Client, p *principal, action string) (*kwerftv1.NodePool, bool) {
	cluster, name := r.PathValue("cluster"), r.PathValue("pool")
	var pool kwerftv1.NodePool
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &pool); err != nil {
		n.kubeError(w, r, p, action, name, poolNotFound(name), err)
		return nil, false
	}
	if pool.Spec.Cluster != cluster {
		writeError(w, http.StatusNotFound, poolNotFound(name))
		return nil, false
	}
	return &pool, true
}

func (n *nodesAPI) poolUpdate(w http.ResponseWriter, r *http.Request) {
	var in poolInput
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Name != "" || in.Role != "" || in.Location != "" {
		writeError(w, http.StatusBadRequest, "A pool's name, role and location cannot change. Create another pool instead.")
		return
	}
	if in.Count != nil && (*in.Count < 0 || *in.Count > 50) {
		writeFieldError(w, "count", "Between 0 and 50 servers.")
		return
	}
	if in.Labels != nil && !validLabels(w, in.Labels) {
		return
	}
	c, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	pool, ok := n.poolGet(w, r, ctx, c, p, "nodepool.update")
	if !ok {
		return
	}
	spec := pool.Spec
	if in.Count != nil {
		spec.Count = *in.Count
	}
	if in.ServerType != "" {
		spec.ServerType = in.ServerType
	}
	if in.Labels != nil {
		spec.Labels = in.Labels
	}
	if in.ScaleDownAfter != nil {
		if spec.Role != kwerftv1.NodeBuilds {
			writeFieldError(w, "scaleDownAfterMinutes", "Only builds pools scale down on their own.")
			return
		}
		if *in.ScaleDownAfter < 5 || *in.ScaleDownAfter > 24*60 {
			writeFieldError(w, "scaleDownAfterMinutes", "Between 5 minutes and 24 hours.")
			return
		}
		spec.ScaleDownAfter = &metav1.Duration{Duration: time.Duration(*in.ScaleDownAfter) * time.Minute}
	}
	if spec.Role == kwerftv1.NodeControlPlane && spec.Count != pool.Spec.Count &&
		!checkControlPlaneCount(w, n.controlPlanesOutside(r, ctx, spec.Cluster, pool.Name), spec.Count) {
		return
	}
	if spec.ServerType != pool.Spec.ServerType && !n.checkCatalog(w, ctx, spec.ServerType, spec.Location) {
		return
	}
	old := pool.Spec
	if err := updateSpec(ctx, c, pool, func(x *kwerftv1.NodePool) bool {
		if x.Spec.Count != old.Count || x.Spec.ServerType != old.ServerType {
			return false
		}
		x.Spec = spec
		return true
	}); err != nil {
		n.kubeError(w, r, p, "nodepool.update", pool.Name, poolNotFound(pool.Name), err)
		return
	}
	n.audit(r, p.user.Email, "nodepool.update", pool.Name, poolDetail(spec))
	writeJSON(w, http.StatusOK, toPoolJSON(pool))
}

func (n *nodesAPI) poolDelete(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	pool, ok := n.poolGet(w, r, ctx, c, p, "nodepool.delete")
	if !ok {
		return
	}
	if pool.Spec.Role == kwerftv1.NodeControlPlane && pool.Spec.Count > 0 && n.controlPlanesOutside(r, ctx, pool.Spec.Cluster, pool.Name) == 0 {
		writeError(w, http.StatusConflict, "This pool holds the cluster's only control-plane nodes. Delete the cluster instead.")
		return
	}
	if err := c.Delete(ctx, pool); err != nil {
		n.kubeError(w, r, p, "nodepool.delete", pool.Name, poolNotFound(pool.Name), err)
		return
	}
	n.audit(r, p.user.Email, "nodepool.delete", pool.Name, poolDetail(pool.Spec)+"; its servers are drained and deleted")
	w.WriteHeader(http.StatusNoContent)
}

// serverRemove removes a pool server that has no node (it never joined):
// the pool deletes it and creates another.
func (n *nodesAPI) serverRemove(w http.ResponseWriter, r *http.Request) {
	server := r.PathValue("server")
	c, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	pool, ok := n.poolGet(w, r, ctx, c, p, "nodepool.server_remove")
	if !ok {
		return
	}
	if !slices.ContainsFunc(pool.Status.Nodes, func(x kwerftv1.PoolNode) bool { return x.Name == server }) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Server %q is not in pool %s.", server, pool.Name))
		return
	}
	if err := updateSpec(ctx, c, pool, func(x *kwerftv1.NodePool) bool {
		list := []string{}
		if s := x.Annotations[controllers.AnnotationRemoveServers]; s != "" {
			list = strings.Split(s, ",")
		}
		if !slices.Contains(list, server) {
			list = append(list, server)
		}
		if x.Annotations == nil {
			x.Annotations = map[string]string{}
		}
		x.Annotations[controllers.AnnotationRemoveServers] = strings.Join(list, ",")
		return true
	}); err != nil {
		n.kubeError(w, r, p, "nodepool.server_remove", pool.Name, poolNotFound(pool.Name), err)
		return
	}
	n.audit(r, p.user.Email, "nodepool.server_remove", server, "from pool "+pool.Name+"; the pool replaces it")
	w.WriteHeader(http.StatusAccepted)
}

// ---- nodes --------------------------------------------------------------------------------

// nodeFor reads a node as the user in the cluster named in the path.
func (n *nodesAPI) nodeFor(w http.ResponseWriter, r *http.Request, ctx context.Context, p *principal, action string) (client.Client, *corev1.Node, bool) {
	cluster, name := r.PathValue("cluster"), r.PathValue("node")
	uc, err := n.userIn(r, cluster)
	if err != nil {
		if errors.Is(err, clusters.ErrUnknown) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("Cluster %q not found.", cluster))
		} else {
			n.unreachable(w, cluster)
		}
		return nil, nil, false
	}
	var node corev1.Node
	if err := uc.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
		n.kubeError(w, r, p, action, name, fmt.Sprintf("Node %q not found.", name), err)
		return nil, nil, false
	}
	return uc, &node, true
}

func annotateNode(ctx context.Context, c client.Client, node *corev1.Node, set map[string]string, drop []string, unschedulable *bool) error {
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	for k, v := range set {
		node.Annotations[k] = v
	}
	for _, k := range drop {
		delete(node.Annotations, k)
	}
	if unschedulable != nil {
		node.Spec.Unschedulable = *unschedulable
	}
	return c.Patch(ctx, node, patch)
}

func (n *nodesAPI) nodeDrain(w http.ResponseWriter, r *http.Request) {
	_, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	uc, node, ok := n.nodeFor(w, r, ctx, p, "node.drain")
	if !ok {
		return
	}
	if err := annotateNode(ctx, uc, node, map[string]string{controllers.AnnotationNodeDrain: "true"}, nil, nil); err != nil {
		n.kubeError(w, r, p, "node.drain", node.Name, "Node not found.", err)
		return
	}
	n.audit(r, p.user.Email, "node.drain", r.PathValue("cluster")+"/"+node.Name, "cordon and drain")
	writeJSON(w, http.StatusAccepted, toNodeJSON(node))
}

func (n *nodesAPI) nodeUncordon(w http.ResponseWriter, r *http.Request) {
	_, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	uc, node, ok := n.nodeFor(w, r, ctx, p, "node.uncordon")
	if !ok {
		return
	}
	if node.Annotations[controllers.AnnotationNodeRemove] != "" {
		writeError(w, http.StatusConflict, "This node is being removed.")
		return
	}
	no := false
	if err := annotateNode(ctx, uc, node, nil, []string{controllers.AnnotationNodeDrain, controllers.AnnotationDrainStarted, controllers.AnnotationDrainStatus}, &no); err != nil {
		n.kubeError(w, r, p, "node.uncordon", node.Name, "Node not found.", err)
		return
	}
	n.audit(r, p.user.Email, "node.uncordon", r.PathValue("cluster")+"/"+node.Name, "schedulable again")
	writeJSON(w, http.StatusOK, toNodeJSON(node))
}

func (n *nodesAPI) nodeRemove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Force bool `json:"force"`
	}
	if !decodeOptional(w, r, &in) {
		return
	}
	_, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	uc, node, ok := n.nodeFor(w, r, ctx, p, "node.remove")
	if !ok {
		return
	}
	if controllers.IsControlPlane(node) {
		var nodes corev1.NodeList
		if err := uc.List(ctx, &nodes); err != nil {
			n.kubeError(w, r, p, "node.remove", node.Name, "", err)
			return
		}
		if err := controllers.CheckControlPlaneRemoval(nodes.Items, node.Name); err != nil {
			writeError(w, http.StatusConflict, "Not now: "+err.Error()+".")
			return
		}
	}
	value := "true"
	if in.Force {
		value = "force"
	}
	if err := annotateNode(ctx, uc, node, map[string]string{controllers.AnnotationNodeRemove: value}, nil, nil); err != nil {
		n.kubeError(w, r, p, "node.remove", node.Name, "Node not found.", err)
		return
	}
	detail := "drain and remove"
	if pool := node.Labels[hetzner.LabelPool]; pool != "" {
		detail += "; pool " + pool + " deletes its server and replaces it"
	}
	if in.Force {
		detail += " (forced: local volumes are lost)"
	}
	n.audit(r, p.user.Email, "node.remove", r.PathValue("cluster")+"/"+node.Name, detail)
	writeJSON(w, http.StatusAccepted, toNodeJSON(node))
}

// ---- join ---------------------------------------------------------------------------------

type joinCommandJSON struct {
	Command   string    `json:"command"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (n *nodesAPI) joinCommand(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	var in struct {
		Role       string `json:"role"`
		TTLMinutes int    `json:"ttlMinutes"`
	}
	if !decodeOptional(w, r, &in) {
		return
	}
	in.Role = cmp.Or(in.Role, jointoken.RoleWorker)
	if in.Role != jointoken.RoleWorker && in.Role != jointoken.RoleControlPlane {
		writeFieldError(w, "role", "Choose worker or control-plane.")
		return
	}
	if in.TTLMinutes == 0 {
		in.TTLMinutes = 60
	}
	if in.TTLMinutes < 10 || in.TTLMinutes > 24*60 {
		writeFieldError(w, "ttlMinutes", "Between 10 minutes and 24 hours.")
		return
	}
	c, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	if in.Role == jointoken.RoleControlPlane && p.user.Role != store.RoleOwner {
		writeError(w, http.StatusForbidden, "Only owners can add control-plane nodes: they receive the cluster's k3s server token.")
		return
	}
	if _, err := n.clusterOf(ctx, c, cluster); err != nil {
		n.kubeError(w, r, p, "cluster.join_command", cluster, fmt.Sprintf("Cluster %q not found.", cluster), err)
		return
	}
	if n.signer == nil || n.cfg.SystemReader == nil {
		writeError(w, http.StatusServiceUnavailable, "Join commands need the console's data key and its own Kubernetes access.")
		return
	}
	var sec corev1.Secret
	if err := n.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.JoinSecretName(cluster)}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusConflict, "This cluster has no join material yet (its first server publishes it).")
			return
		}
		n.internalError(w, r, err)
		return
	}
	tok, claims, err := n.signer.Issue(jointoken.Claims{Cluster: cluster, Role: in.Role}, time.Duration(in.TTLMinutes)*time.Minute)
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	console := n.consoleURL(r)
	cmd := fmt.Sprintf("curl -fsSL %s/join.sh | sudo bash -s -- --token %s --role %s", console, tok, in.Role)
	n.audit(r, p.user.Email, "cluster.join_command", cluster, fmt.Sprintf("%s join token %s, valid %d min", in.Role, claims.ID, in.TTLMinutes))
	writeJSON(w, http.StatusCreated, joinCommandJSON{Command: cmd, Role: in.Role, ExpiresAt: claims.ExpiresAt().UTC()})
}

// bootstrapTokenTTL is how long a worker's k3s bootstrap token lives: the
// agent needs it to join; afterwards it uses its node password and
// certificates.
const bootstrapTokenTTL = time.Hour

// join trades a join token for the k3s join material (install.sh
// stage_join: GET /api/v1/join?role=…&node=<hostname>).
func (n *nodesAPI) join(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !n.joinLim.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "Too many join attempts. Try again later.")
		return
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := n.signer.Verify(strings.TrimSpace(raw))
	if !ok || err != nil {
		msg := "The join token is not valid. Copy a fresh join command from the console."
		if errors.Is(err, jointoken.ErrExpired) {
			msg = "The join token has expired. Copy a fresh join command from the console."
		}
		writeError(w, http.StatusUnauthorized, msg)
		return
	}
	actor := "join token " + claims.ID
	if role := r.URL.Query().Get("role"); role != "" && role != claims.Role {
		writeError(w, http.StatusForbidden, fmt.Sprintf("This join token is for %s nodes; run the join command with --role %s.", claims.Role, claims.Role))
		return
	}
	node := r.URL.Query().Get("node")
	if claims.Node != "" && node != claims.Node {
		n.audit(r, actor, "cluster.node_join.denied", claims.Cluster, fmt.Sprintf("token for %s used by %q from %s", claims.Node, node, ip))
		writeError(w, http.StatusForbidden, "This join token belongs to another server.")
		return
	}
	if n.cfg.SystemReader == nil {
		writeError(w, http.StatusServiceUnavailable, "The console cannot read the join material.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	var sec corev1.Secret
	if err := n.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.JoinSecretName(claims.Cluster)}, &sec); err != nil {
		writeError(w, http.StatusConflict, "The cluster has no join material yet.")
		return
	}
	server, serverToken := strings.TrimSpace(string(sec.Data["server"])), strings.TrimSpace(string(sec.Data["token"]))
	if server == "" || jointoken.ValidServerToken(serverToken) != nil {
		writeError(w, http.StatusConflict, "The cluster's join material is incomplete.")
		return
	}
	target, err := n.systemIn(claims.Cluster)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "The cluster cannot be reached right now; try again in a minute.")
		return
	}
	// A token bound to a server is spent once that server is a node.
	if node != "" && validation.IsDNS1123Subdomain(node) == nil {
		var existing corev1.Node
		if err := target.Get(ctx, client.ObjectKey{Name: node}, &existing); err == nil && claims.Node != "" {
			writeError(w, http.StatusConflict, "A node named "+node+" has joined already; this join token is spent.")
			return
		} else if err != nil && !apierrors.IsNotFound(err) {
			writeError(w, http.StatusServiceUnavailable, "The cluster cannot be reached right now; try again in a minute.")
			return
		}
	}
	token := serverToken
	if claims.Role == jointoken.RoleWorker {
		bt, err := jointoken.NewBootstrapToken(serverToken, "kwerft join "+claims.ID+" "+truncate(node, 63), bootstrapTokenTTL, n.now())
		if err != nil {
			n.internalError(w, r, err)
			return
		}
		if err := target.Create(ctx, bt.Secret); err != nil {
			n.cfg.Logger.Error("create k3s bootstrap token", "cluster", claims.Cluster, "err", err)
			writeError(w, http.StatusServiceUnavailable, "The console could not create a join token in the cluster; try again in a minute.")
			return
		}
		token = bt.AgentToken
	}
	n.audit(r, actor, "cluster.node_join", claims.Cluster, fmt.Sprintf("%s %q from %s", claims.Role, node, ip))
	out := map[string]string{"server": server, "token": token}
	if n.externalCloudProvider(ctx, claims.Cluster, target) {
		out["cloudProvider"] = "external"
	}
	writeJSON(w, http.StatusOK, out)
}

// externalCloudProvider reports whether the cluster runs the hcloud
// cloud-controller-manager: joining kubelets must then register with
// --cloud-provider=external (install.sh reads cloudProvider). The local
// cluster knows from its install; others show it in their nodes' provider IDs.
func (n *nodesAPI) externalCloudProvider(ctx context.Context, cluster string, target client.Client) bool {
	if cluster == clusters.Local && n.cfg.HCloudCCM {
		return true
	}
	var nodes corev1.NodeList
	if err := target.List(ctx, &nodes, client.Limit(20)); err != nil {
		return false
	}
	return slices.ContainsFunc(nodes.Items, func(nd corev1.Node) bool {
		return strings.HasPrefix(nd.Spec.ProviderID, "hcloud://")
	})
}

func (n *nodesAPI) serveJoinScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(install.Join(n.consoleURL(r), install.ReleaseURL(version.Version))))
}

func (n *nodesAPI) serveInstaller(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(install.Installer(version.Version)))
}

// ---- Hetzner Cloud catalogue -------------------------------------------------------------------

type serverTypeJSON struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Cores        int      `json:"cores"`
	Memory       float64  `json:"memory"`
	Disk         float64  `json:"disk"`
	CPUType      string   `json:"cpuType"`
	Architecture string   `json:"architecture"`
	Locations    []string `json:"locations"` // where it can be ordered now
	// Monthly prices (gross) by location.
	Prices map[string]string `json:"prices"`
}

type locationJSON struct {
	Name        string `json:"name"`
	City        string `json:"city"`
	Country     string `json:"country"`
	NetworkZone string `json:"networkZone"`
}

func (n *nodesAPI) catalog(w http.ResponseWriter, r *http.Request) {
	if n.hcloud == nil {
		writeError(w, http.StatusServiceUnavailable, "The console cannot read the Hetzner Cloud token.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	hc, err := n.hcloud(ctx)
	if err != nil {
		writeError(w, http.StatusConflict, "Add a Hetzner Cloud API token under Settings › Hetzner Cloud first.")
		return
	}
	types, err := hc.ServerTypes(ctx)
	if err == nil {
		var locs []hetzner.Location
		if locs, err = hc.Locations(ctx); err == nil {
			out := struct {
				Locations   []locationJSON   `json:"locations"`
				ServerTypes []serverTypeJSON `json:"serverTypes"`
			}{Locations: []locationJSON{}, ServerTypes: []serverTypeJSON{}}
			for _, l := range locs {
				out.Locations = append(out.Locations, locationJSON{Name: l.Name, City: l.City, Country: l.Country, NetworkZone: l.NetworkZone})
			}
			for _, t := range types {
				if t.Deprecation != nil || t.Memory < 4 {
					continue
				}
				st := serverTypeJSON{Name: t.Name, Description: t.Description, Cores: t.Cores, Memory: t.Memory, Disk: t.Disk,
					CPUType: t.CPUType, Architecture: t.Architecture, Locations: []string{}, Prices: map[string]string{}}
				for _, l := range locs {
					if t.AvailableIn(l.Name) {
						st.Locations = append(st.Locations, l.Name)
						st.Prices[l.Name] = t.MonthlyGross(l.Name)
					}
				}
				if len(st.Locations) > 0 {
					out.ServerTypes = append(out.ServerTypes, st)
				}
			}
			slices.SortFunc(out.ServerTypes, func(a, b serverTypeJSON) int {
				return cmp.Or(cmp.Compare(a.Memory, b.Memory), cmp.Compare(a.Cores, b.Cores), strings.Compare(a.Name, b.Name))
			})
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	if errors.Is(err, hetzner.ErrTokenRejected) {
		writeError(w, http.StatusConflict, "Hetzner rejected the stored Cloud API token. Replace it under Settings › Hetzner Cloud.")
		return
	}
	writeError(w, http.StatusBadGateway, "The Hetzner Cloud API did not answer: "+truncate(err.Error(), 200))
}

// ---- vSwitch ----------------------------------------------------------------------------------

type vswitchInput struct {
	RobotUser     string   `json:"robotUser"`
	RobotPassword string   `json:"robotPassword"`
	VSwitchID     int64    `json:"vswitchId"`
	VLAN          int      `json:"vlan"`
	IPRange       string   `json:"ipRange"`
	NetworkZone   string   `json:"networkZone"`
	Servers       []string `json:"servers"`
}

// vswitch couples dedicated servers to the cluster's Cloud Network: a Robot
// vSwitch (new, or an existing one), the dedicated servers on it, and a
// vswitch subnet in the Cloud Network. The dedicated servers then need a
// VLAN interface with an address in ipRange and a route to the network
// (docs/phase5.md › As built (W2)); they join with the join command.
func (n *nodesAPI) vswitch(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	var in vswitchInput
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.RobotUser == "" || in.RobotPassword == "" {
		writeFieldError(w, "robotUser", "The Robot webservice user and password are needed (Robot › Settings › Web service and app settings). They are used once and not stored.")
		return
	}
	prefix, err := netip.ParsePrefix(in.IPRange)
	if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() || prefix.Bits() < 16 || prefix.Bits() > 29 || prefix.Masked() != prefix {
		writeFieldError(w, "ipRange", "A private IPv4 range inside the cluster's network, such as 10.0.64.0/24.")
		return
	}
	if in.VSwitchID == 0 && (in.VLAN < 4000 || in.VLAN > 4091) {
		writeFieldError(w, "vlan", "A new vSwitch needs a VLAN ID between 4000 and 4091.")
		return
	}
	for _, s := range in.Servers {
		if _, err := netip.ParseAddr(s); err != nil {
			if _, err := strconv.ParseInt(s, 10, 64); err != nil {
				writeFieldError(w, "servers", fmt.Sprintf("%q is neither a server's main IPv4 address nor its server number.", s))
				return
			}
		}
	}
	in.NetworkZone = cmp.Or(in.NetworkZone, "eu-central")
	c, p, ctx, cancel, err := n.userClient(r)
	defer cancel()
	if err != nil {
		n.internalError(w, r, err)
		return
	}
	if _, err := n.clusterOf(ctx, c, cluster); err != nil {
		n.kubeError(w, r, p, "cluster.vswitch", cluster, fmt.Sprintf("Cluster %q not found.", cluster), err)
		return
	}
	if n.hcloud == nil {
		writeError(w, http.StatusServiceUnavailable, "The console cannot read the Hetzner Cloud token.")
		return
	}
	hc, err := n.hcloud(ctx)
	if err != nil {
		writeError(w, http.StatusConflict, "Add a Hetzner Cloud API token under Settings › Hetzner Cloud first.")
		return
	}
	network, err := hc.ClusterNetwork(ctx, cluster)
	if err != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("The cluster has no Cloud Network (labelled %s=%s or named %s).", hetzner.LabelCluster, cluster, hetzner.NetworkName(cluster)))
		return
	}
	if netRange, err := netip.ParsePrefix(network.IPRange); err == nil && (!netRange.Contains(prefix.Addr()) || prefix.Bits() < netRange.Bits()) {
		writeFieldError(w, "ipRange", fmt.Sprintf("%s is not inside the network's range %s.", in.IPRange, network.IPRange))
		return
	}
	robot := &hetzner.RobotClient{User: in.RobotUser, Password: in.RobotPassword, Base: n.robotBase}
	robotErr := func(err error) {
		if errors.Is(err, hetzner.ErrTokenRejected) {
			writeFieldError(w, "robotUser", "Robot rejected the webservice user or password.")
			return
		}
		writeError(w, http.StatusBadGateway, "Hetzner Robot: "+truncate(err.Error(), 200))
	}
	vs := hetzner.VSwitch{ID: in.VSwitchID}
	if in.VSwitchID == 0 {
		if vs, err = robot.CreateVSwitch(ctx, "kwerft-"+cluster, in.VLAN); err != nil {
			robotErr(err)
			return
		}
	} else if vs, err = robot.VSwitch(ctx, in.VSwitchID); err != nil {
		robotErr(err)
		return
	}
	if len(in.Servers) > 0 {
		if err := robot.AddVSwitchServers(ctx, vs.ID, in.Servers); err != nil {
			robotErr(err)
			return
		}
	}
	if !slices.ContainsFunc(vs.CloudNetwork, func(cn hetzner.VSwitchNetwork) bool { return cn.ID == network.ID }) {
		if _, err := hc.AddVSwitchSubnet(ctx, network.ID, in.IPRange, in.NetworkZone, vs.ID); err != nil {
			writeError(w, http.StatusBadGateway, "Hetzner Cloud: "+truncate(err.Error(), 200))
			return
		}
	}
	n.audit(r, p.user.Email, "cluster.vswitch", cluster, fmt.Sprintf("vSwitch %d (VLAN %d) coupled to network %s as %s; servers %s",
		vs.ID, vs.VLAN, network.Name, in.IPRange, strings.Join(in.Servers, ", ")))
	writeJSON(w, http.StatusOK, map[string]any{"vswitchId": vs.ID, "vlan": vs.VLAN, "network": network.Name, "ipRange": in.IPRange})
}
