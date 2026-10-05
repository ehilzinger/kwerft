package controllers

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/jointoken"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// NodePoolReconciler keeps each NodePool's Hetzner Cloud servers: it
// creates servers that join the pool's cluster through cloud-init (or
// bootstrap a new hetzner-cloud cluster), waits for their nodes to become
// Ready, replaces servers of another type one at a time, and drains,
// removes and deletes servers when the pool shrinks. It runs in the
// management cluster and reaches the pool's cluster through ClusterClients.
//
// Rules (docs/phase5.md › As built (W2)):
//   - Only servers labelled kwerft.dev/cluster=<cluster> and
//     kwerft.dev/pool=<pool> are ever deleted.
//   - Control plane: the cluster keeps an odd number of control-plane nodes
//     (k3s servers with embedded etcd), never zero. Servers join one at a
//     time; one is removed at a time, only while every other control-plane
//     node is Ready, after k3s took it out of etcd.
//   - Worker and builds pools replace nodes that stay NotReady for
//     RepairAfter; control-plane nodes are only replaced by hand.
//   - Builds pools are tainted kwerft.dev/builds=true:NoSchedule and scale
//     between zero and spec.count with the build Jobs in their cluster.
type NodePoolReconciler struct {
	client.Client
	// APIReader reads Secrets (join material, the Cloud token) uncached.
	APIReader client.Reader
	// Namespace holds the join material and the Cloud token (kwerft-system).
	Namespace string

	// HCloud returns a Cloud API client; nil reads the token from the
	// Secret kwerft-hcloud-token (key "token").
	HCloud func(ctx context.Context) (*hetzner.Client, error)
	// Clusters reaches the pools' clusters.
	Clusters ClusterClients
	// JoinTokens signs the join tokens cloud-init uses.
	JoinTokens *jointoken.Signer
	// ConsoleURL is where new servers download the installer and trade
	// their join token (https://<console>).
	ConsoleURL func() string
	// InstallerURL is the release's install.sh, used when the console does
	// not serve one; may be empty.
	InstallerURL string
	// Version is the console's release, which a new cluster's first server
	// installs (install.sh --version); empty for development builds.
	Version string

	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// Suffix makes server names unique; nil uses 5 random characters.
	Suffix func() string
	// Timeouts; zero means the defaults.
	DrainTimeout, EtcdRemoveTimeout, JoinTimeout, RepairAfter time.Duration

	catalogMu sync.Mutex
	catalog   *cloudCatalog
}

const (
	// HCloudTokenSecret holds the Hetzner Cloud API token (key "token"),
	// written by Settings › Hetzner Cloud API (W1).
	nodePoolTokenSecret = "kwerft-hcloud-token"
	nodePoolTokenKey    = "token"

	// AnnotationRemoveServers lists servers of the pool to remove (comma
	// separated), for servers whose node never joined; the console writes it
	// as the signed-in user.
	AnnotationRemoveServers = "kwerft.dev/remove-servers"

	// LabelBuildNode and TaintBuilds mark build pool nodes.
	LabelBuildNode = "kwerft.dev/builds"
	// BuildNodesConfigMap in kwerft-system tells the cluster's Build
	// reconciler that build nodes exist (or are on their way).
	BuildNodesConfigMap = "kwerft-build-nodes"

	nodePoolFinalizer = "kwerft.dev/nodepool"

	// DefaultJoinTimeout is how long a new server may take to become a
	// Ready node before it is reported Failed.
	DefaultJoinTimeout = 20 * time.Minute
	// DefaultRepairAfter is how long a worker stays NotReady before it is
	// replaced.
	DefaultRepairAfter = 15 * time.Minute
	// DefaultScaleDownAfter idles a builds pool to zero.
	DefaultScaleDownAfter = 15 * time.Minute
	// joinTokenTTL bounds the cloud-init token: boot, packages, k3s.
	joinTokenTTL = 2 * time.Hour
)

// Pool node phases.
const (
	PoolNodeCreating = "Creating"
	PoolNodeJoining  = "Joining"
	PoolNodeReady    = "Ready"
	PoolNodeDraining = "Draining"
	PoolNodeDeleting = "Deleting"
	PoolNodeFailed   = "Failed"
)

// TaintBuilds keeps everything but builds off build nodes.
var TaintBuilds = corev1.Taint{Key: LabelBuildNode, Value: "true", Effect: corev1.TaintEffectNoSchedule}

// JoinSecretName and AgentSecretName are a cluster's join material in the
// management cluster (docs/phase5.md › Join material).
func JoinSecretName(cluster string) string  { return clusters.JoinSecretName(cluster) }
func AgentSecretName(cluster string) string { return clusters.AgentSecretName(cluster) }

func (r *NodePoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *NodePoolReconciler) ns() string { return cmp.Or(r.Namespace, GatewayNamespace) }

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kwerftv1.NodePool{}).
		Named("nodepool").
		Complete(r)
}

// HCloudFromSecret reads the Cloud token Secret and returns a client.
func HCloudFromSecret(reader client.Reader, namespace string) func(context.Context) (*hetzner.Client, error) {
	return func(ctx context.Context) (*hetzner.Client, error) {
		var sec corev1.Secret
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: nodePoolTokenSecret}, &sec); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, errNoCloudToken
			}
			return nil, err
		}
		tok := strings.TrimSpace(string(sec.Data[nodePoolTokenKey]))
		if tok == "" {
			return nil, errNoCloudToken
		}
		return &hetzner.Client{Token: tok}, nil
	}
}

var errNoCloudToken = errors.New("no Hetzner Cloud API token: add one under Settings › Hetzner Cloud")

// cloudCatalog caches server types, locations and images for a while: they
// change rarely and every pool reconcile needs them.
type cloudCatalog struct {
	at        time.Time
	types     []hetzner.ServerType
	locations []hetzner.Location
	images    map[string][]hetzner.Image // by architecture
}

func (r *NodePoolReconciler) cloudCatalog(ctx context.Context, hc *hetzner.Client) (*cloudCatalog, error) {
	r.catalogMu.Lock()
	defer r.catalogMu.Unlock()
	if r.catalog != nil && r.now().Sub(r.catalog.at) < 10*time.Minute {
		return r.catalog, nil
	}
	types, err := hc.ServerTypes(ctx)
	if err != nil {
		return nil, err
	}
	locs, err := hc.Locations(ctx)
	if err != nil {
		return nil, err
	}
	c := &cloudCatalog{at: r.now(), types: types, locations: locs, images: map[string][]hetzner.Image{}}
	for _, arch := range []string{"x86", "arm"} {
		if c.images[arch], err = hc.SystemImages(ctx, arch); err != nil {
			return nil, err
		}
	}
	r.catalog = c
	return c, nil
}

// poolRun is one reconcile pass over a pool.
type poolRun struct {
	r       *NodePoolReconciler
	pool    *kwerftv1.NodePool
	hc      *hetzner.Client
	target  client.Client // nil while the cluster cannot be reached
	nodes   []corev1.Node // every node of the cluster
	servers []hetzner.Server
	now     time.Time
	// busy: something is in flight; reconcile again soon.
	busy bool
	// removing: servers being removed (before this pass chose more).
	removing int
	status   []kwerftv1.PoolNode
	notes    []string
}

func (p *poolRun) node(name string) *corev1.Node {
	for i := range p.nodes {
		if p.nodes[i].Name == name {
			return &p.nodes[i]
		}
	}
	return nil
}

func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool kwerftv1.NodePool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := pool.Status.DeepCopy()
	run, err := r.reconcile(ctx, &pool)
	if run == nil || pool.DeletionTimestamp != nil && !controllerutil.ContainsFinalizer(&pool, nodePoolFinalizer) {
		return ctrl.Result{}, err
	}
	// Nothing in another cluster or at Hetzner is watched: look again soon
	// while something moves, now and then otherwise.
	res := ctrl.Result{RequeueAfter: 2 * time.Minute}
	switch {
	case run.busy || len(run.notes) > 0:
		res.RequeueAfter = 15 * time.Second
	case roleOf(&pool) == kwerftv1.NodeBuilds && pool.Spec.Count > 0:
		res.RequeueAfter = 20 * time.Second
	}
	st := &pool.Status
	st.ObservedGeneration = pool.Generation
	if run.status != nil {
		st.Nodes = run.status
	}
	st.ReadyNodes = 0
	for _, n := range st.Nodes {
		if n.Phase == PoolNodeReady {
			st.ReadyNodes++
		}
	}
	switch {
	case err != nil && isTerminal(err):
		setReady(&st.Conditions, pool.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
		err = nil
		res = ctrl.Result{RequeueAfter: time.Minute}
	case err != nil:
		setReady(&st.Conditions, pool.Generation, metav1.ConditionFalse, "ReconcileFailed", oneLine(err.Error()))
	case len(run.notes) > 0:
		setReady(&st.Conditions, pool.Generation, metav1.ConditionFalse, "Progressing", strings.Join(run.notes, " "))
	case st.ReadyNodes == st.Desired && len(st.Nodes) == int(st.Desired):
		setReady(&st.Conditions, pool.Generation, metav1.ConditionTrue, "Ready", fmt.Sprintf("%d of %d nodes ready.", st.ReadyNodes, st.Desired))
	default:
		setReady(&st.Conditions, pool.Generation, metav1.ConditionFalse, "Progressing", fmt.Sprintf("%d of %d nodes ready.", st.ReadyNodes, st.Desired))
	}
	if !equalStatus(before, st) {
		if uerr := r.Status().Update(ctx, &pool); uerr != nil && !apierrors.IsNotFound(uerr) && !apierrors.IsConflict(uerr) {
			return ctrl.Result{}, uerr
		} else if apierrors.IsConflict(uerr) {
			return ctrl.Result{Requeue: true}, nil
		}
	}
	return res, err
}

func equalStatus(a, b *kwerftv1.NodePoolStatus) bool {
	x, y := a.DeepCopy(), b.DeepCopy()
	for _, s := range []*kwerftv1.NodePoolStatus{x, y} {
		for i := range s.Conditions {
			s.Conditions[i].LastTransitionTime = metav1.Time{}
		}
	}
	return equality.Semantic.DeepEqual(x, y)
}

func (r *NodePoolReconciler) reconcile(ctx context.Context, pool *kwerftv1.NodePool) (*poolRun, error) {
	run := &poolRun{r: r, pool: pool, now: r.now()}
	if pool.DeletionTimestamp == nil && !controllerutil.ContainsFinalizer(pool, nodePoolFinalizer) {
		controllerutil.AddFinalizer(pool, nodePoolFinalizer)
		if err := r.Update(ctx, pool); err != nil {
			return nil, err
		}
	}
	if pool.DeletionTimestamp == nil {
		if err := validatePool(pool); err != nil {
			return run, err
		}
	}
	hcloud := r.HCloud
	if hcloud == nil {
		hcloud = HCloudFromSecret(r.APIReader, r.ns())
	}
	hc, err := hcloud(ctx)
	if err != nil {
		if errors.Is(err, errNoCloudToken) {
			// A pool that never had a server can go without the token.
			if pool.DeletionTimestamp != nil && len(pool.Status.Nodes) == 0 && controllerutil.ContainsFinalizer(pool, nodePoolFinalizer) {
				controllerutil.RemoveFinalizer(pool, nodePoolFinalizer)
				return run, r.Update(ctx, pool)
			}
			return run, terminalf("NoCloudToken", "%s", err.Error())
		}
		return run, err
	}
	run.hc = hc
	sel := hetzner.LabelCluster + "=" + pool.Spec.Cluster + "," + hetzner.LabelPool + "=" + pool.Name
	all, err := hc.Servers(ctx, sel)
	if err != nil {
		return run, fmt.Errorf("list servers: %w", err)
	}
	for _, s := range all {
		// Belt and braces: the selector already says so.
		if s.Labels[hetzner.LabelCluster] == pool.Spec.Cluster && s.Labels[hetzner.LabelPool] == pool.Name && s.Status != hetzner.ServerDeleting {
			run.servers = append(run.servers, s)
		}
	}
	slices.SortFunc(run.servers, func(a, b hetzner.Server) int { return cmp.Compare(a.ID, b.ID) })

	if run.target, err = r.Clusters.For(ctx, pool.Spec.Cluster); err != nil {
		run.target = nil
		// A pool being deleted must not hang on its cluster: deleting a
		// Cluster waits for its pools.
		if !errors.Is(err, clusters.ErrUnavailable) && !errors.Is(err, clusters.ErrUnknown) && pool.DeletionTimestamp == nil {
			return run, err
		}
	} else {
		var nodes corev1.NodeList
		if err := run.target.List(ctx, &nodes); err != nil {
			run.target = nil
			run.notes = append(run.notes, "The cluster's Kubernetes API is not reachable: "+oneLine(err.Error()))
		} else {
			run.nodes = nodes.Items
		}
	}

	if pool.DeletionTimestamp != nil {
		return run, r.finalize(ctx, run)
	}
	// An upgrade of the pool's cluster holds node changes until it is done
	// (docs/phase6-upgrades.md).
	if run.target != nil {
		name, err := activeUpgrade(ctx, run.target)
		if err != nil {
			return run, err
		}
		if name != "" {
			run.notes = append(run.notes, "Waiting for upgrade "+name+" to finish.")
			run.busy = true
			return run, nil
		}
	}
	return run, r.scale(ctx, run)
}

// activeUpgrade names the cluster's active Upgrade, if any. A cluster
// without the Upgrade CRD (an older agent) has none.
func activeUpgrade(ctx context.Context, c client.Reader) (string, error) {
	var list kwerftv1.UpgradeList
	if err := c.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) || apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("list upgrades: %w", err)
	}
	for _, u := range list.Items {
		if upgrades.Active(u.Status.Phase) {
			return u.Name, nil
		}
	}
	return "", nil
}

// validatePool checks what the CRD cannot.
func validatePool(pool *kwerftv1.NodePool) error {
	for k, v := range pool.Spec.Labels {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return terminalf("InvalidLabels", "label %q: %s", k, errs[0])
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			return terminalf("InvalidLabels", "label %q: %s", k, errs[0])
		}
		if strings.HasPrefix(k, "kwerft.dev/") || strings.Contains(k, "kubernetes.io/") || strings.Contains(k, "k8s.io/") {
			return terminalf("InvalidLabels", "label %q: kwerft.dev, kubernetes.io and k8s.io labels are Kwerft's and Kubernetes' own", k)
		}
	}
	if !hetzner.ValidServerName(serverPrefix(pool.Spec.Cluster, pool.Name) + "-abcde") {
		return terminalf("InvalidName", "cluster and pool names make no valid server name")
	}
	return nil
}

// serverPrefix is <cluster>-<pool>, made a hostname and short enough for
// a 6-character suffix.
func serverPrefix(cluster, pool string) string {
	p := strings.ToLower(strings.ReplaceAll(cluster+"-"+pool, ".", "-"))
	if len(p) > 56 {
		p = p[:56]
	}
	return strings.TrimRight(p, "-")
}

func (r *NodePoolReconciler) suffix() string {
	if r.Suffix != nil {
		return r.Suffix()
	}
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// desired is how many servers the pool aims for now.
func (r *NodePoolReconciler) desired(ctx context.Context, run *poolRun) (int32, error) {
	pool := run.pool
	if pool.Spec.Role != kwerftv1.NodeBuilds {
		return pool.Spec.Count, nil
	}
	current := int32(len(run.servers))
	if run.target == nil {
		// Unknown load: keep what is there.
		return min(current, pool.Spec.Count), nil
	}
	active, err := activeBuilds(ctx, run.target)
	if err != nil {
		return 0, err
	}
	if active > 0 {
		pool.Status.LastBuildAt = &metav1.Time{Time: run.now}
		return min(pool.Spec.Count, max(current, int32(active))), nil
	}
	idle := DefaultScaleDownAfter
	if pool.Spec.ScaleDownAfter != nil && pool.Spec.ScaleDownAfter.Duration > 0 {
		idle = pool.Spec.ScaleDownAfter.Duration
	}
	if pool.Status.LastBuildAt == nil || run.now.Sub(pool.Status.LastBuildAt.Time) >= idle {
		return 0, nil
	}
	return min(current, pool.Spec.Count), nil
}

// activeBuilds counts build Jobs that have not finished (running or about
// to run); queued builds wait behind them.
func activeBuilds(ctx context.Context, c client.Reader) (int, error) {
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(builds.Namespace), client.HasLabels{builds.LabelBuild}); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for i := range jobs.Items {
		if !jobFinished(&jobs.Items[i]) {
			n++
		}
	}
	return n, nil
}

// entry is one server of the pool with what Kwerft knows about it.
type entry struct {
	srv      hetzner.Server
	node     *corev1.Node
	phase    string
	message  string
	outdated bool
	// removing: a drain/removal is under way or was asked for.
	removing bool
	force    bool
}

func (e *entry) ready() bool { return e.node != nil && NodeReady(e.node) }

func (r *NodePoolReconciler) classify(run *poolRun) []*entry {
	pool := run.pool
	toRemove := map[string]bool{}
	for _, n := range strings.Split(pool.Annotations[AnnotationRemoveServers], ",") {
		if n = strings.TrimSpace(n); n != "" {
			toRemove[n] = true
		}
	}
	var out []*entry
	for _, s := range run.servers {
		e := &entry{srv: s, node: run.node(s.Name)}
		e.outdated = s.ServerType.Name != pool.Spec.ServerType || s.Location.Name != pool.Spec.Location
		if toRemove[s.Name] {
			e.removing, e.force = true, true
		}
		if e.node != nil {
			if v := e.node.Annotations[AnnotationNodeRemove]; v == "true" || v == "force" {
				e.removing, e.force = true, e.force || v == "force"
			}
			if !drainStarted(e.node).IsZero() && e.node.Annotations[AnnotationNodeDrain] == "" {
				e.removing = true
			}
		}
		switch {
		case s.Status != hetzner.ServerRunning:
			e.phase, e.message = PoolNodeCreating, "Server "+s.Status+"."
		case e.ready():
			e.phase = PoolNodeReady
		case e.node != nil && run.now.Sub(nodeReadyChanged(e.node)) > orDuration(r.RepairAfter, DefaultRepairAfter):
			e.phase, e.message = PoolNodeFailed, "Kubernetes has reported the node NotReady since "+nodeReadyChanged(e.node).UTC().Format(time.RFC3339)+"."
			if pool.Spec.Role != kwerftv1.NodeControlPlane {
				e.removing, e.message = true, e.message+" Replacing it."
			}
		case e.node != nil:
			e.phase, e.message = PoolNodeJoining, "Node registered, not Ready yet."
		case run.target != nil && run.now.Sub(s.Created) > orDuration(r.JoinTimeout, DefaultJoinTimeout):
			e.phase, e.message = PoolNodeFailed, "Did not join within "+humanDuration(orDuration(r.JoinTimeout, DefaultJoinTimeout))+
				"; see /var/log/kwerft-join.log on the server. Remove it to try again."
		default:
			e.phase, e.message = PoolNodeJoining, "Installing and joining."
		}
		out = append(out, e)
	}
	return out
}

func (r *NodePoolReconciler) scale(ctx context.Context, run *poolRun) error {
	pool := run.pool
	desired, err := r.desired(ctx, run)
	if err != nil {
		return err
	}
	pool.Status.Desired = desired
	entries := r.classify(run)
	defer func() { run.status = poolStatus(entries) }()

	isCP := pool.Spec.Role == kwerftv1.NodeControlPlane
	if isCP && run.target != nil {
		if err := r.checkControlPlaneCount(run); err != nil {
			// Finish what is under way, start nothing new.
			return errors.Join(r.progressRemovals(ctx, run, entries), err)
		}
	}

	if err := r.progressRemovals(ctx, run, entries); err != nil {
		return err
	}
	active := slices.DeleteFunc(slices.Clone(entries), func(e *entry) bool { return e.removing })
	run.removing = len(entries) - len(active)

	// Replace outdated servers one at a time: one extra server first, the
	// old one goes once the new one is Ready.
	target := int(desired)
	settled := !slices.ContainsFunc(entries, func(e *entry) bool { return e.phase != PoolNodeReady })
	replacing := slices.ContainsFunc(active, func(e *entry) bool { return e.outdated })
	newJoining := slices.ContainsFunc(active, func(e *entry) bool {
		return !e.outdated && (e.phase == PoolNodeCreating || e.phase == PoolNodeJoining)
	})
	switch {
	case replacing && newJoining && len(active) >= target && target > 0:
		// The replacement is on its way; the old server goes once it is Ready.
		target = len(active)
		run.notes = append(run.notes, "Replacing servers of another type or location, one at a time.")
	case replacing && settled && len(active) == target && target > 0:
		target++
		run.notes = append(run.notes, "Replacing servers of another type or location, one at a time.")
	}

	switch {
	case len(active) < target:
		return r.grow(ctx, run, &entries, target-len(active))
	case len(active) > target:
		return r.shrink(ctx, run, active, len(active)-target)
	}
	if err := r.syncNodes(ctx, run, entries); err != nil {
		return err
	}
	return r.syncBuildMarker(ctx, run)
}

// checkControlPlaneCount refuses a control-plane pool size that leaves the
// cluster with an even number of control-plane nodes, or none.
func (r *NodePoolReconciler) checkControlPlaneCount(run *poolRun) error {
	outside := 0
	for i := range run.nodes {
		n := &run.nodes[i]
		if IsControlPlane(n) && !slices.ContainsFunc(run.servers, func(s hetzner.Server) bool { return s.Name == n.Name }) {
			outside++
		}
	}
	total := outside + int(run.pool.Spec.Count)
	switch {
	case total == 0:
		return terminalf("LastControlPlane", "the cluster would have no control-plane node left")
	case total%2 == 0:
		return terminalf("EvenControlPlane", "the cluster would have %d control-plane nodes; etcd needs an odd number (1, 3 or 5)", total)
	}
	return nil
}

// progressRemovals moves every server being removed one step on: drain,
// etcd member removal, node deletion, server deletion.
func (r *NodePoolReconciler) progressRemovals(ctx context.Context, run *poolRun, entries []*entry) error {
	isCP := run.pool.Spec.Role == kwerftv1.NodeControlPlane
	cpBusy := false
	for _, e := range entries {
		if !e.removing {
			continue
		}
		run.busy = true
		if e.node == nil && run.target == nil && e.srv.Status == hetzner.ServerRunning {
			// It may well be a node: never delete one undrained because its
			// cluster cannot be asked right now.
			e.phase, e.message = PoolNodeDraining, "Waiting for the cluster to be reachable."
			continue
		}
		if e.node == nil {
			// Never joined (or its node is gone): nothing to drain.
			if err := r.deleteServer(ctx, run, e); err != nil {
				return err
			}
			continue
		}
		if run.target == nil {
			e.phase, e.message = PoolNodeDraining, "Waiting for the cluster to be reachable."
			continue
		}
		if isCP {
			if cpBusy {
				e.phase, e.message = PoolNodeDraining, "Waiting: control-plane nodes are removed one at a time."
				continue
			}
			cpBusy = true
			if err := CheckControlPlaneRemoval(run.nodes, e.node.Name); err != nil {
				e.phase, e.message = PoolNodeDraining, "Waiting: "+err.Error()+"."
				continue
			}
		}
		if err := startDrain(ctx, run.target, e.node, run.now); err != nil {
			return client.IgnoreNotFound(err)
		}
		e.phase = PoolNodeDraining
		// A NotReady node's pods cannot be evicted gracefully; its
		// replacement matters more than a long drain.
		res, err := drain(ctx, run.target, run.target, e.node, run.now, orDuration(r.DrainTimeout, DefaultDrainTimeout), e.force || !NodeReady(e.node))
		if err != nil {
			return err
		}
		e.message = res.Message
		_ = setDrainStatus(ctx, run.target, e.node, res.Message)
		if !res.Done {
			continue
		}
		if out, err := removeEtcdMember(ctx, run.target, e.node, run.now, orDuration(r.EtcdRemoveTimeout, DefaultEtcdRemoveTimeout)); err != nil || !out {
			e.message = "Removing the etcd member."
			if err != nil {
				return client.IgnoreNotFound(err)
			}
			continue
		}
		if err := run.target.Delete(ctx, e.node); client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.deleteServer(ctx, run, e); err != nil {
			return err
		}
	}
	return nil
}

func (r *NodePoolReconciler) deleteServer(ctx context.Context, run *poolRun, e *entry) error {
	if e.srv.Labels[hetzner.LabelCluster] != run.pool.Spec.Cluster || e.srv.Labels[hetzner.LabelPool] != run.pool.Name {
		return fmt.Errorf("refusing to delete server %s: it does not carry this pool's labels", e.srv.Name)
	}
	if err := run.hc.DeleteServer(ctx, e.srv.ID); err != nil {
		return fmt.Errorf("delete server %s: %w", e.srv.Name, err)
	}
	e.phase, e.message = PoolNodeDeleting, "Server deleted."
	return r.forgetRemoval(ctx, run.pool, e.srv.Name)
}

// forgetRemoval drops a deleted server from the pool's removal list.
func (r *NodePoolReconciler) forgetRemoval(ctx context.Context, pool *kwerftv1.NodePool, name string) error {
	list := pool.Annotations[AnnotationRemoveServers]
	if list == "" {
		return nil
	}
	names := slices.DeleteFunc(strings.Split(list, ","), func(n string) bool { return strings.TrimSpace(n) == name || strings.TrimSpace(n) == "" })
	patch := client.MergeFrom(pool.DeepCopy())
	if len(names) == 0 {
		delete(pool.Annotations, AnnotationRemoveServers)
	} else {
		pool.Annotations[AnnotationRemoveServers] = strings.Join(names, ",")
	}
	return client.IgnoreNotFound(r.Patch(ctx, pool, patch))
}

// shrink starts removing n servers: broken ones first, then servers of
// another type, then those without local volumes, newest first.
func (r *NodePoolReconciler) shrink(ctx context.Context, run *poolRun, active []*entry, n int) error {
	if run.pool.Spec.Role == kwerftv1.NodeControlPlane {
		n = min(n, 1)
		if run.removing > 0 {
			run.notes = append(run.notes, "Control-plane nodes are removed one at a time.")
			return nil
		}
		if slices.ContainsFunc(active, func(e *entry) bool { return e.phase != PoolNodeReady }) &&
			!slices.ContainsFunc(active, func(e *entry) bool { return e.phase == PoolNodeFailed }) {
			run.notes = append(run.notes, "Waiting for control-plane nodes to be Ready before removing one.")
			return nil
		}
	}
	rank := func(e *entry) int {
		switch {
		case e.phase == PoolNodeFailed || e.node == nil:
			return 0
		case e.outdated:
			return 1
		case !e.ready():
			return 2
		}
		return 3
	}
	victims := slices.Clone(active)
	slices.SortStableFunc(victims, func(a, b *entry) int {
		if c := cmp.Compare(rank(a), rank(b)); c != 0 {
			return c
		}
		return -cmp.Compare(a.srv.ID, b.srv.ID) // newest first
	})
	for _, e := range victims[:n] {
		e.removing = true
	}
	run.notes = append(run.notes, fmt.Sprintf("Removing %d server(s).", n))
	return r.progressRemovals(ctx, run, victims[:n])
}

// grow creates up to n servers. Without join material only a new
// hetzner-cloud cluster's first control-plane server can be created.
func (r *NodePoolReconciler) grow(ctx context.Context, run *poolRun, entries *[]*entry, n int) error {
	pool := run.pool
	run.busy = true
	if pool.Spec.Role == kwerftv1.NodeControlPlane {
		if slices.ContainsFunc(*entries, func(e *entry) bool { return e.phase == PoolNodeCreating || e.phase == PoolNodeJoining }) {
			run.notes = append(run.notes, "Control-plane servers join one at a time.")
			return nil
		}
		n = 1
	}
	mode, err := r.joinMode(ctx, run)
	if err != nil {
		return err
	}
	if mode == "" {
		return nil
	}
	if mode == "agent" {
		n = 1
	}
	cat, err := r.cloudCatalog(ctx, run.hc)
	if err != nil {
		return fmt.Errorf("read the Cloud catalogue: %w", err)
	}
	typ, ok := hetzner.FindServerType(cat.types, pool.Spec.ServerType)
	if !ok {
		return terminalf("UnknownServerType", "Hetzner Cloud has no server type %q", pool.Spec.ServerType)
	}
	if !typ.AvailableIn(pool.Spec.Location) {
		return terminalf("ServerTypeUnavailable", "server type %s cannot be ordered in %s now", pool.Spec.ServerType, pool.Spec.Location)
	}
	img, ok := hetzner.NewestUbuntu(cat.images[typ.Architecture], typ.Architecture)
	if !ok {
		return terminalf("NoImage", "no Ubuntu LTS image for %s servers", typ.Architecture)
	}
	network, err := r.clusterNetwork(ctx, run, mode, cat)
	if err != nil {
		return err
	}
	if network == nil {
		run.notes = append(run.notes, fmt.Sprintf("Waiting for the cluster's private network (a Cloud Network labelled %s=%s or named %s).",
			hetzner.LabelCluster, pool.Spec.Cluster, hetzner.NetworkName(pool.Spec.Cluster)))
		return nil
	}
	keys, err := run.hc.ServerSSHKeys(ctx)
	if err != nil {
		return fmt.Errorf("list SSH keys: %w", err)
	}
	group, err := r.placementGroup(ctx, run)
	if err != nil {
		return err
	}
	for range n {
		name := serverPrefix(pool.Spec.Cluster, pool.Name) + "-" + r.suffix()
		userData, err := r.userData(ctx, run, mode, name)
		if err != nil {
			return err
		}
		labels := map[string]string{hetzner.LabelCluster: pool.Spec.Cluster, hetzner.LabelPool: pool.Name, hetzner.LabelRole: string(roleOf(pool))}
		if mode == "agent" {
			labels[hetzner.LabelBootstrap] = "true"
		}
		opts := hetzner.CreateServerOpts{
			Name: name, ServerType: pool.Spec.ServerType, Location: pool.Spec.Location, Image: img.Name,
			SSHKeys: keys, Networks: []int64{network.ID}, UserData: userData, Labels: labels,
		}
		if group != nil && len(group.Servers) < hetzner.SpreadGroupMax {
			opts.PlacementGroup = group.ID
			group.Servers = append(group.Servers, 0)
		}
		srv, _, err := run.hc.CreateServer(ctx, opts)
		if err != nil {
			var apiErr *hetzner.APIError
			if errors.As(err, &apiErr) && (apiErr.Code == "resource_unavailable" || apiErr.Code == "resource_limit_exceeded" || apiErr.Code == "invalid_input") {
				return terminalf("CreateFailed", "creating a server failed: %s", apiErr.Error())
			}
			return fmt.Errorf("create server: %w", err)
		}
		*entries = append(*entries, &entry{srv: srv, phase: PoolNodeCreating, message: "Server ordered."})
	}
	return nil
}

func roleOf(pool *kwerftv1.NodePool) kwerftv1.NodeRole {
	return cmp.Or(pool.Spec.Role, kwerftv1.NodeWorker)
}

// joinMode is "join" when the cluster's join material exists, "agent" for
// the bootstrap server of a new hetzner-cloud cluster, and "" (wait) else.
func (r *NodePoolReconciler) joinMode(ctx context.Context, run *poolRun) (string, error) {
	pool := run.pool
	var sec corev1.Secret
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.ns(), Name: JoinSecretName(pool.Spec.Cluster)}, &sec)
	if err == nil && len(sec.Data["server"]) > 0 && len(sec.Data["token"]) > 0 {
		return "join", nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	// No join material: only a new hetzner-cloud cluster's first
	// control-plane server may be created, by the first control-plane pool.
	var cl kwerftv1.Cluster
	if err := r.Get(ctx, client.ObjectKey{Name: pool.Spec.Cluster}, &cl); err != nil || cl.Spec.Provider != kwerftv1.ClusterHetznerCloud || roleOf(pool) != kwerftv1.NodeControlPlane {
		run.notes = append(run.notes, "Waiting for the cluster's join material (Secret "+JoinSecretName(pool.Spec.Cluster)+").")
		return "", client.IgnoreNotFound(err)
	}
	others, err := run.hc.Servers(ctx, hetzner.LabelCluster+"="+pool.Spec.Cluster)
	if err != nil {
		return "", err
	}
	if len(others) > 0 {
		run.notes = append(run.notes, "Waiting for the cluster's first server to publish its join material.")
		return "", nil
	}
	var pools kwerftv1.NodePoolList
	if err := r.List(ctx, &pools); err != nil {
		return "", err
	}
	for _, p := range pools.Items {
		if p.Spec.Cluster == pool.Spec.Cluster && roleOf(&p) == kwerftv1.NodeControlPlane && p.Spec.Count > 0 && p.Name < pool.Name {
			run.notes = append(run.notes, "Pool "+p.Name+" bootstraps the cluster.")
			return "", nil
		}
	}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.ns(), Name: AgentSecretName(pool.Spec.Cluster)}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			run.notes = append(run.notes, "Waiting for the cluster's agent token (Secret "+AgentSecretName(pool.Spec.Cluster)+").")
			return "", nil
		}
		return "", err
	}
	if len(sec.Data["token"]) == 0 {
		run.notes = append(run.notes, "The cluster's agent token is empty.")
		return "", nil
	}
	return "agent", nil
}

// userData renders cloud-init for a new server.
func (r *NodePoolReconciler) userData(ctx context.Context, run *poolRun, mode, name string) (string, error) {
	pool := run.pool
	console := ""
	if r.ConsoleURL != nil {
		console = strings.TrimRight(r.ConsoleURL(), "/")
	}
	if console == "" {
		return "", errors.New("the console's address is not known yet")
	}
	j := joinScript{Console: console, FallbackURL: r.InstallerURL, Mode: mode, Cluster: pool.Spec.Cluster, Pool: pool.Name,
		Labels: nodeLabels(pool)}
	if roleOf(pool) == kwerftv1.NodeBuilds {
		j.Taints = []string{TaintBuilds.ToString()}
	}
	switch mode {
	case "agent":
		var sec corev1.Secret
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.ns(), Name: AgentSecretName(pool.Spec.Cluster)}, &sec); err != nil {
			return "", err
		}
		j.Token = strings.TrimSpace(string(sec.Data["token"]))
		if u := strings.TrimSpace(string(sec.Data["consoleURL"])); u != "" {
			j.Console = strings.TrimRight(u, "/")
		}
		j.Version = r.Version
	default:
		role := jointoken.RoleWorker
		if roleOf(pool) == kwerftv1.NodeControlPlane {
			role = jointoken.RoleControlPlane
		}
		tok, _, err := r.JoinTokens.Issue(jointoken.Claims{Cluster: pool.Spec.Cluster, Role: role, Node: name}, joinTokenTTL)
		if err != nil {
			return "", err
		}
		j.Token, j.Role = tok, role
	}
	return j.userData(), nil
}

// nodeLabels are the labels a pool's nodes carry.
func nodeLabels(pool *kwerftv1.NodePool) map[string]string {
	out := map[string]string{hetzner.LabelPool: pool.Name, hetzner.LabelRole: string(roleOf(pool))}
	for k, v := range pool.Spec.Labels {
		out[k] = v
	}
	if roleOf(pool) == kwerftv1.NodeBuilds {
		out[LabelBuildNode] = "true"
	}
	return out
}

// placementGroup finds or creates the pool's spread group.
// The private network of a new hetzner-cloud cluster (ClusterNetworkRange,
// one Cloud subnet in the location's network zone; pod and service
// networks are 10.42/16 and 10.43/16).
const (
	ClusterNetworkRange = "10.0.0.0/16"
	clusterSubnetRange  = "10.0.0.0/24"
)

// clusterNetwork finds the cluster's private network: labelled or named as
// the cluster's (hetzner.ClusterNetwork), else the one the cluster's
// installer recorded in kube-system/hcloud (key network: the Cloud Network
// of its first server's private address, so a hand-made network of the
// local cluster needs no label). A new hetzner-cloud cluster's first server
// (mode agent) gets a new one, labelled as the cluster's and Kwerft's; the
// Cluster's finalizer deletes it. Nil: none yet.
func (r *NodePoolReconciler) clusterNetwork(ctx context.Context, run *poolRun, mode string, cat *cloudCatalog) (*hetzner.NetworkRef, error) {
	cluster := run.pool.Spec.Cluster
	n, err := run.hc.ClusterNetwork(ctx, cluster)
	switch {
	case err == nil:
		return &n, nil
	case !errors.Is(err, hetzner.ErrNotFound):
		return nil, fmt.Errorf("find the cluster network: %w", err)
	}
	if run.target != nil {
		var sec corev1.Secret
		if err := run.target.Get(ctx, client.ObjectKey{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}, &sec); err == nil {
			if id, err := strconv.ParseInt(strings.TrimSpace(string(sec.Data["network"])), 10, 64); err == nil && id > 0 {
				nw, err := run.hc.GetNetwork(ctx, id)
				switch {
				case err == nil:
					return &hetzner.NetworkRef{ID: nw.ID, Name: nw.Name, IPRange: nw.IPRange}, nil
				case !errors.Is(err, hetzner.ErrNotFound):
					return nil, fmt.Errorf("read the cluster network: %w", err)
				}
			}
		}
	}
	if mode != "agent" {
		return nil, nil
	}
	i := slices.IndexFunc(cat.locations, func(l hetzner.Location) bool { return l.Name == run.pool.Spec.Location })
	if i < 0 || cat.locations[i].NetworkZone == "" {
		return nil, terminalf("UnknownLocation", "Hetzner Cloud has no location %q", run.pool.Spec.Location)
	}
	nw, err := run.hc.CreateNetwork(ctx, hetzner.NetworkOpts{
		Name: hetzner.NetworkName(cluster), IPRange: ClusterNetworkRange,
		Labels:  map[string]string{hetzner.LabelCluster: cluster, DNSLabelManagedBy: ManagedByKwerft},
		Subnets: []hetzner.Subnet{{Type: "cloud", IPRange: clusterSubnetRange, NetworkZone: cat.locations[i].NetworkZone}},
	})
	if err != nil {
		return nil, fmt.Errorf("create the cluster network: %w", err)
	}
	log.FromContext(ctx).Info("created the cluster network", "cluster", cluster, "network", nw.ID)
	return &hetzner.NetworkRef{ID: nw.ID, Name: nw.Name, IPRange: nw.IPRange}, nil
}

func (r *NodePoolReconciler) placementGroup(ctx context.Context, run *poolRun) (*hetzner.PlacementGroup, error) {
	sel := hetzner.LabelCluster + "=" + run.pool.Spec.Cluster + "," + hetzner.LabelPool + "=" + run.pool.Name
	groups, err := run.hc.PlacementGroups(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("list placement groups: %w", err)
	}
	if len(groups) > 0 {
		return &groups[0], nil
	}
	g, err := run.hc.CreatePlacementGroup(ctx, hetzner.PlacementGroupName(run.pool.Spec.Cluster, run.pool.Name),
		map[string]string{hetzner.LabelCluster: run.pool.Spec.Cluster, hetzner.LabelPool: run.pool.Name})
	if err != nil {
		return nil, fmt.Errorf("create placement group: %w", err)
	}
	return &g, nil
}

// syncNodes keeps the pool's labels (and the builds taint) on its nodes.
func (r *NodePoolReconciler) syncNodes(ctx context.Context, run *poolRun, entries []*entry) error {
	if run.target == nil {
		return nil
	}
	want := nodeLabels(run.pool)
	for _, e := range entries {
		if e.node == nil || e.removing {
			continue
		}
		n := e.node
		patch := client.MergeFrom(n.DeepCopy())
		changed := false
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		for k, v := range want {
			if n.Labels[k] != v {
				n.Labels[k], changed = v, true
			}
		}
		if roleOf(run.pool) == kwerftv1.NodeBuilds && !slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.MatchTaint(&TaintBuilds) }) {
			n.Spec.Taints, changed = append(n.Spec.Taints, TaintBuilds), true
		}
		if changed {
			if err := run.target.Patch(ctx, n, patch); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// syncBuildMarker tells the cluster's Build reconciler whether builds go to
// build nodes: the ConfigMap exists while a builds pool with count > 0 does.
func (r *NodePoolReconciler) syncBuildMarker(ctx context.Context, run *poolRun) error {
	if run.target == nil || roleOf(run.pool) != kwerftv1.NodeBuilds {
		return nil
	}
	if run.pool.Spec.Count == 0 || run.pool.DeletionTimestamp != nil {
		return r.dropBuildMarker(ctx, run)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: BuildNodesConfigMap, Namespace: r.ns()}}
	err := run.target.Get(ctx, client.ObjectKeyFromObject(cm), cm)
	if apierrors.IsNotFound(err) {
		cm.Labels = map[string]string{LabelManagedBy: ManagedByKwerft}
		cm.Data = map[string]string{"pool": run.pool.Name}
		return client.IgnoreAlreadyExists(run.target.Create(ctx, cm))
	}
	return err
}

func (r *NodePoolReconciler) dropBuildMarker(ctx context.Context, run *poolRun) error {
	if run.target == nil {
		return nil
	}
	cm := &corev1.ConfigMap{}
	if err := run.target.Get(ctx, client.ObjectKey{Namespace: r.ns(), Name: BuildNodesConfigMap}, cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	if cm.Data["pool"] != run.pool.Name {
		return nil
	}
	return client.IgnoreNotFound(run.target.Delete(ctx, cm))
}

// finalize removes every server of a deleted pool, then the placement
// group and the finalizer. A control-plane pool holding the cluster's last
// control-plane nodes is only torn down with its cluster.
func (r *NodePoolReconciler) finalize(ctx context.Context, run *poolRun) error {
	pool := run.pool
	if !controllerutil.ContainsFinalizer(pool, nodePoolFinalizer) {
		return nil
	}
	clusterGone := false
	if pool.Spec.Cluster != clusters.Local {
		var cl kwerftv1.Cluster
		err := r.Get(ctx, client.ObjectKey{Name: pool.Spec.Cluster}, &cl)
		clusterGone = apierrors.IsNotFound(err) || err == nil && cl.DeletionTimestamp != nil
	}
	pool.Status.Desired = 0
	entries := r.classify(run)
	defer func() { run.status = poolStatus(entries) }()
	if len(entries) > 0 {
		run.busy = true
		if clusterGone {
			// The cluster goes with its servers: nothing to drain.
			for _, e := range entries {
				if err := r.deleteServer(ctx, run, e); err != nil {
					return err
				}
			}
			return nil
		}
		if roleOf(pool) == kwerftv1.NodeControlPlane && run.target != nil {
			others := 0
			for i := range run.nodes {
				if IsControlPlane(&run.nodes[i]) && !slices.ContainsFunc(entries, func(e *entry) bool { return e.srv.Name == run.nodes[i].Name }) {
					others++
				}
			}
			if others == 0 {
				return terminalf("LastControlPlane", "this pool holds the cluster's last control-plane nodes; delete the cluster instead")
			}
		}
		for _, e := range entries {
			e.removing = true
		}
		return r.progressRemovals(ctx, run, entries)
	}
	if err := r.dropBuildMarker(ctx, run); err != nil {
		return err
	}
	groups, err := run.hc.PlacementGroups(ctx, hetzner.LabelCluster+"="+pool.Spec.Cluster+","+hetzner.LabelPool+"="+pool.Name)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if err := run.hc.DeletePlacementGroup(ctx, g.ID); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(pool, nodePoolFinalizer)
	return r.Update(ctx, pool)
}

func poolStatus(entries []*entry) []kwerftv1.PoolNode {
	out := make([]kwerftv1.PoolNode, 0, len(entries))
	for _, e := range entries {
		phase := e.phase
		if e.removing && phase != PoolNodeDeleting {
			phase = PoolNodeDraining
		}
		out = append(out, kwerftv1.PoolNode{
			Name: e.srv.Name, ServerID: e.srv.ID, PublicIP: e.srv.PublicIPv4(), PrivateIP: e.srv.PrivateIP(0),
			ServerType: e.srv.ServerType.Name, Phase: phase, Message: e.message,
		})
	}
	return out
}
