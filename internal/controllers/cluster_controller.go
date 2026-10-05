package controllers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kwerftac "github.com/ehilzinger/kwerft/api/applyconfiguration/api/v1alpha1"
	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Cluster phases (kwerftv1.ClusterPhase).
const (
	ClusterPending      kwerftv1.ClusterPhase = "Pending"
	ClusterProvisioning kwerftv1.ClusterPhase = "Provisioning"
	ClusterConnected    kwerftv1.ClusterPhase = "Connected"
	ClusterDisconnected kwerftv1.ClusterPhase = "Disconnected"
	ClusterFailed       kwerftv1.ClusterPhase = "Failed"
)

// ClusterFinalizer holds a remote Cluster until its agent is disconnected,
// its node pools (and so its Cloud servers) are gone and its Secrets are
// removed.
const ClusterFinalizer = "kwerft.dev/cluster-cleanup"

// ControlPlanePoolName is the NodePool of a hetzner-cloud cluster's control
// plane; its first server bootstraps the cluster with `install.sh --agent`
// (docs/phase5.md).
func ControlPlanePoolName(cluster string) string { return cluster + "-control-plane" }

// ClusterTunnel is what the Cluster reconciler needs of the console's agent
// tunnel (clusters.Hub).
type ClusterTunnel interface {
	Agent(name string) (clusters.AgentStatus, bool)
	Disconnect(name string)
	DisconnectUnless(name, tokenHash string)
	Changed() <-chan struct{}
}

// ClusterReconciler runs in the management cluster only (the console's
// process). It keeps the Cluster "local", creates the control-plane
// NodePool and agent token of a hetzner-cloud cluster, records each
// cluster's status from its agent, publishes the agents' join material as
// Secrets, and on deletion disconnects the agent, deletes the cluster's
// node pools (whose reconciler deletes the servers by label) and its
// Secrets.
type ClusterReconciler struct {
	client.Client
	// APIReader reads Secrets uncached (the manager does not cache them).
	APIReader client.Reader
	// Tunnel reports connected agents; nil leaves remote clusters waiting.
	Tunnel ClusterTunnel
	// Namespace holds the clusters' Secrets (kwerft-system).
	Namespace string
	// ConsoleDomain is the --console-domain flag, for the console URL agents
	// dial when ConsoleSettings has none.
	ConsoleDomain string
	// Remote returns a client for a connected cluster with Kwerft's own
	// identity there (clusters.Clients.For); nil turns off copying
	// notification channels and Git connections into remote clusters
	// (cluster_mirror.go).
	Remote func(name string) (client.Client, error)
	// HCloud reaches the Cloud API with the console's token (finalize
	// deletes a hetzner-cloud cluster's network); nil skips that.
	HCloud func(context.Context) (*hetzner.Client, error)
	// Local reports the management cluster's own health; nil leaves the
	// versions empty.
	Local func(ctx context.Context) clusters.AgentInfo
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Resync is how often a cluster's status is refreshed (1 minute).
	Resync time.Duration
}

func (r *ClusterReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ClusterReconciler) resync() time.Duration { return cmp.Or(r.Resync, time.Minute) }

func (r *ClusterReconciler) namespace() string { return cmp.Or(r.Namespace, GatewayNamespace) }

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var c kwerftv1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &c); err != nil {
		if apierrors.IsNotFound(err) && req.Name == clusters.Local {
			return ctrl.Result{}, r.ensureLocal(ctx)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if c.Name == clusters.Local || c.Spec.Provider == kwerftv1.ClusterLocal {
		return r.reconcileLocal(ctx, &c)
	}
	if c.DeletionTimestamp != nil {
		return r.finalize(ctx, &c)
	}
	if controllerutil.AddFinalizer(&c, ClusterFinalizer) {
		if err := r.Update(ctx, &c); err != nil {
			return ctrl.Result{}, err
		}
	}
	orig := c.DeepCopy()
	if !clusters.ValidName(c.Name) {
		c.Status.Phase = ClusterFailed
		setReady(&c.Status.Conditions, c.Generation, metav1.ConditionFalse, "InvalidName",
			fmt.Sprintf("Cluster names must be DNS labels of at most %d characters starting with a letter; create it again with another name.", clusters.MaxNameLength))
		return ctrl.Result{}, r.writeStatus(ctx, &c, orig)
	}

	hash := c.Annotations[clusters.TokenHashAnnotation]
	if r.Tunnel != nil {
		// A token rotated or removed (also with kubectl) ends the session
		// that used the old one.
		r.Tunnel.DisconnectUnless(c.Name, hash)
	}
	var agent clusters.AgentStatus
	connected := false
	if r.Tunnel != nil {
		agent, connected = r.Tunnel.Agent(c.Name)
	}

	problem := ""
	if c.Spec.Provider == kwerftv1.ClusterHetznerCloud {
		var patched bool
		var err error
		if problem, patched, err = r.reconcileCloud(ctx, &c, connected); err != nil || patched {
			// A patched Cluster comes back as a watch event; its status
			// is written then, from fresh data.
			return ctrl.Result{}, err
		}
	}
	if connected && agent.Info.Join != nil {
		if err := r.publishJoin(ctx, &c, agent.Info.Join); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.setRemoteStatus(&c, agent, connected, problem)
	if connected && r.Remote != nil {
		remote, connErr := r.Remote(c.Name)
		err := connErr
		if err == nil {
			err = r.mirror(ctx, remote)
		}
		if err != nil {
			log.FromContext(ctx).Info("copying channels and Git connections into the cluster failed", "cluster", c.Name, "err", err.Error())
		}
		setMirrored(&c, err)
		if c.Spec.Provider == kwerftv1.ClusterHetznerCloud && c.Spec.HetznerCloud != nil {
			note, err := "", connErr
			if err == nil {
				note, err = r.syncCloud(ctx, remote, &c)
			}
			if err != nil {
				log.FromContext(ctx).Info("handing the Hetzner Cloud settings to the cluster failed", "cluster", c.Name, "err", err.Error())
			}
			setCloudSynced(&c, note, err)
		}
	}
	if err := r.writeStatus(ctx, &c, orig); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// reconcileCloud keeps a hetzner-cloud cluster's control-plane pool and,
// until the first agent connected, the token its first server bootstraps
// with. It returns a problem to show when something blocks progress, and
// whether it patched the Cluster.
func (r *ClusterReconciler) reconcileCloud(ctx context.Context, c *kwerftv1.Cluster, connected bool) (string, bool, error) {
	hc := c.Spec.HetznerCloud
	if hc == nil { // the CRD refuses this; be safe
		return "spec.hetznerCloud is missing.", false, nil
	}
	pool := kwerftac.NodePool(ControlPlanePoolName(c.Name), "").
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelClusterName: c.Name}).
		WithOwnerReferences(controllerRef(c, kwerftv1.GroupVersion.WithKind("Cluster"))).
		WithSpec(kwerftac.NodePoolSpec().
			WithCluster(c.Name).
			WithRole(kwerftv1.NodeControlPlane).
			WithServerType(hc.ServerType).
			WithLocation(hc.Location).
			WithCount(cmp.Or(hc.ControlPlanes, 1)))
	// Applied only when it differs: an apply that changes nothing still
	// bumps the pool's resourceVersion on some API servers, and the pool
	// is watched (Owns), which would make this a busy loop.
	var cur kwerftv1.NodePool
	err := r.Get(ctx, client.ObjectKey{Name: ControlPlanePoolName(c.Name)}, &cur)
	want := kwerftv1.NodePoolSpec{Cluster: c.Name, Role: kwerftv1.NodeControlPlane, ServerType: hc.ServerType, Location: hc.Location, Count: cmp.Or(hc.ControlPlanes, 1)}
	upToDate := err == nil && metav1.IsControlledBy(&cur, c) && cur.Labels[LabelClusterName] == c.Name &&
		cur.Spec.Cluster == want.Cluster && cur.Spec.Role == want.Role && cur.Spec.ServerType == want.ServerType &&
		cur.Spec.Location == want.Location && cur.Spec.Count == want.Count
	if err != nil && !apierrors.IsNotFound(err) {
		return "", false, err
	}
	if !upToDate {
		if err := apply(ctx, r.Client, pool); err != nil {
			return "", false, fmt.Errorf("control-plane pool: %w", err)
		}
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: r.namespace(), Name: clusters.AgentSecretName(c.Name)}
	err = r.APIReader.Get(ctx, key, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", false, err
	}
	exists := err == nil
	everConnected := connected || c.Status.LastSeen != nil
	// The token hash from the API server, not the cache: a cached Cluster
	// may not show the hash just written, and comparing it with the fresh
	// Secret would revoke a token that is valid, or mint a second one.
	fresh := &kwerftv1.Cluster{}
	if !everConnected {
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(c), fresh); err != nil {
			return "", false, err
		}
	}
	hash := fresh.Annotations[clusters.TokenHashAnnotation]
	switch {
	case everConnected:
		// The cluster bootstrapped; nothing needs the plain token any more.
		if exists {
			if err := client.IgnoreNotFound(r.Delete(ctx, secret)); err != nil {
				return "", false, err
			}
		}
		return "", false, nil
	case hash == "" && exists && len(secret.Data[clusters.SecretToken]) > 0 && metav1.IsControlledBy(secret, c):
		// The Secret was written but the hash was not (a failure between
		// the two): record the hash of the token already handed out.
		patch := client.MergeFrom(fresh.DeepCopy())
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations[clusters.TokenHashAnnotation] = auth.HashToken(string(secret.Data[clusters.SecretToken]))
		return "", true, r.Patch(ctx, fresh, patch)
	case hash == "":
		// A new cluster: a token for its first server's cloud-init. The
		// Secret first, then the hash (see above for a failure between).
		consoleURL := r.consoleURL(ctx)
		if consoleURL == "" {
			return "The console has no hostname yet, so the cluster's agent would not know where to connect.", false, nil
		}
		token := clusters.NewAgentToken(c.Name)
		s := corev1ac.Secret(key.Name, key.Namespace).
			WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelClusterName: c.Name}).
			WithOwnerReferences(controllerRef(c, kwerftv1.GroupVersion.WithKind("Cluster"))).
			WithType(corev1.SecretTypeOpaque).
			WithData(map[string][]byte{
				clusters.SecretToken:      []byte(token),
				clusters.SecretConsoleURL: []byte(consoleURL),
			})
		if err := apply(ctx, r.Client, s); err != nil {
			return "", false, fmt.Errorf("agent secret: %w", err)
		}
		// Guarded by the fresh resourceVersion: a concurrent rotation wins.
		patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations[clusters.TokenHashAnnotation] = auth.HashToken(token)
		return "", true, r.Patch(ctx, fresh, patch)
	case exists && !auth.TokenMatches(string(secret.Data[clusters.SecretToken]), hash):
		// Rotated before the first connection: the stored token is void, and
		// the new one exists only in the install command shown then.
		if err := client.IgnoreNotFound(r.Delete(ctx, secret)); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	return "", false, nil
}

// LabelClusterName marks objects Kwerft keeps for a Cluster.
const LabelClusterName = "kwerft.dev/cluster"

// consoleURL is where agents dial: the console's active hostname.
func (r *ClusterReconciler) consoleURL(ctx context.Context) string {
	var s kwerftv1.ConsoleSettings
	host := r.ConsoleDomain
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err == nil {
		host = cmp.Or(s.Status.ConsoleDomain, s.Spec.ConsoleDomain, host)
	}
	if host == "" {
		return ""
	}
	return "https://" + host
}

// publishJoin writes the agent's join material as cluster-<name>-join.
func (r *ClusterReconciler) publishJoin(ctx context.Context, c *kwerftv1.Cluster, j *clusters.JoinMaterial) error {
	s := corev1ac.Secret(clusters.JoinSecretName(c.Name), r.namespace()).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelClusterName: c.Name}).
		WithOwnerReferences(controllerRef(c, kwerftv1.GroupVersion.WithKind("Cluster"))).
		WithType(corev1.SecretTypeOpaque).
		WithData(map[string][]byte{clusters.SecretJoinServer: []byte(j.Server), clusters.SecretJoinToken: []byte(j.Token)})
	if err := apply(ctx, r.Client, s); err != nil {
		return fmt.Errorf("join secret: %w", err)
	}
	return nil
}

func (r *ClusterReconciler) setRemoteStatus(c *kwerftv1.Cluster, agent clusters.AgentStatus, connected bool, problem string) {
	st := &c.Status
	st.ObservedGeneration = c.Generation
	switch {
	case connected:
		st.Phase = ClusterConnected
		seen := agent.LastSeen.Truncate(time.Second)
		// LastSeen moves at most once a minute: a status write per poll
		// would be churn for nothing.
		if st.LastSeen == nil || seen.Sub(st.LastSeen.Time) >= time.Minute || seen.Before(st.LastSeen.Time) {
			st.LastSeen = &metav1.Time{Time: seen}
		}
		st.AgentVersion = agent.Info.AgentVersion
		st.KubernetesVersion = agent.Info.KubernetesVersion
		st.Nodes, st.ReadyNodes = agent.Info.Nodes, agent.Info.ReadyNodes
		msg := fmt.Sprintf("The agent is connected (from %s).", agent.Remote)
		if agent.Info.Error != "" {
			msg += " It reports: " + agent.Info.Error
		}
		setReady(&st.Conditions, c.Generation, metav1.ConditionTrue, "Connected", msg)
	case st.LastSeen != nil:
		st.Phase = ClusterDisconnected
		setReady(&st.Conditions, c.Generation, metav1.ConditionFalse, "Disconnected",
			"The agent is not connected. Projects in this cluster cannot be reached until it is back.")
	case c.Annotations[clusters.TokenHashAnnotation] == "" && c.Spec.Provider == kwerftv1.ClusterAdopted:
		st.Phase = ClusterPending
		setReady(&st.Conditions, c.Generation, metav1.ConditionFalse, "NoToken",
			"The cluster has no agent token. Rotate the token to get an install command.")
	case c.Spec.Provider == kwerftv1.ClusterHetznerCloud:
		st.Phase = ClusterProvisioning
		msg := "Creating the control plane; the cluster appears once its agent connects."
		if problem != "" {
			msg = problem
		}
		setReady(&st.Conditions, c.Generation, metav1.ConditionFalse, "Provisioning", msg)
	default:
		st.Phase = ClusterPending
		setReady(&st.Conditions, c.Generation, metav1.ConditionFalse, "WaitingForAgent",
			"Waiting for the agent: run the install command on the cluster's first server.")
	}
}

func (r *ClusterReconciler) writeStatus(ctx context.Context, c, orig *kwerftv1.Cluster) error {
	if equality.Semantic.DeepEqual(orig.Status, c.Status) {
		return nil
	}
	return patchStatus(ctx, r.Client, c, orig)
}

// finalize disconnects the agent, deletes the cluster's node pools (their
// reconciler deletes the servers, by label) and waits for them, then
// removes its Secrets: the token is revoked with the Cluster.
func (r *ClusterReconciler) finalize(ctx context.Context, c *kwerftv1.Cluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(c, ClusterFinalizer) {
		return ctrl.Result{}, nil
	}
	if r.Tunnel != nil {
		r.Tunnel.Disconnect(c.Name)
	}
	var pools kwerftv1.NodePoolList
	if err := r.List(ctx, &pools); err != nil {
		return ctrl.Result{}, err
	}
	remaining := 0
	for i := range pools.Items {
		p := &pools.Items[i]
		if p.Spec.Cluster != c.Name {
			continue
		}
		remaining++
		if p.DeletionTimestamp == nil {
			if err := client.IgnoreNotFound(r.Delete(ctx, p)); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if remaining > 0 {
		orig := c.DeepCopy()
		setReady(&c.Status.Conditions, c.Generation, metav1.ConditionFalse, "Deleting",
			fmt.Sprintf("Deleting the cluster's servers (%d node pools left).", remaining))
		if err := r.writeStatus(ctx, c, orig); err != nil && !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("cluster status not written while deleting", "err", err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if c.Spec.Provider == kwerftv1.ClusterHetznerCloud {
		if wait, err := r.deleteCloudNetwork(ctx, c); err != nil || wait {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
	}
	for _, name := range []string{clusters.AgentSecretName(c.Name), clusters.JoinSecretName(c.Name)} {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace()}}
		if err := client.IgnoreNotFound(r.Delete(ctx, s)); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(c, ClusterFinalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, c))
}

// ensureLocal creates the Cluster "local": the management cluster itself.
func (r *ClusterReconciler) ensureLocal(ctx context.Context) error {
	c := &kwerftv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusters.Local, Labels: map[string]string{LabelManagedBy: ManagedByKwerft}},
		Spec:       kwerftv1.ClusterSpec{DisplayName: "Local", Provider: kwerftv1.ClusterLocal},
	}
	if err := r.Create(ctx, c); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// reconcileLocal records the management cluster's own health.
func (r *ClusterReconciler) reconcileLocal(ctx context.Context, c *kwerftv1.Cluster) (ctrl.Result, error) {
	orig := c.DeepCopy()
	st := &c.Status
	st.ObservedGeneration = c.Generation
	st.Phase = ClusterConnected
	now := r.now().Truncate(time.Second)
	if st.LastSeen == nil || now.Sub(st.LastSeen.Time) >= time.Minute || now.Before(st.LastSeen.Time) {
		st.LastSeen = &metav1.Time{Time: now}
	}
	msg := "The console runs in this cluster."
	if r.Local != nil {
		info := r.Local(ctx)
		st.AgentVersion, st.KubernetesVersion = info.AgentVersion, info.KubernetesVersion
		st.Nodes, st.ReadyNodes = info.Nodes, info.ReadyNodes
		if info.Error != "" {
			msg += " Health check: " + info.Error
		}
	}
	setReady(&st.Conditions, c.Generation, metav1.ConditionTrue, "Local", msg)
	return ctrl.Result{RequeueAfter: r.resync()}, r.writeStatus(ctx, c, orig)
}

func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(manager.RunnableFunc(r.ensureLocal)); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("cluster").
		For(&kwerftv1.Cluster{}).
		// Spec changes and deletions of its pools; not their status.
		Owns(&kwerftv1.NodePool{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	if r.Remote != nil {
		// A channel or Git connection changed (credentials: the console's
		// annotation): copy it into every cluster at once.
		all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			var list kwerftv1.ClusterList
			if err := mgr.GetCache().List(ctx, &list); err != nil {
				return nil
			}
			out := make([]reconcile.Request, 0, len(list.Items))
			for _, c := range list.Items {
				if c.Name != clusters.Local {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: c.Name}})
				}
			}
			return out
		})
		changed := builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))
		b = b.Watches(&kwerftv1.NotificationChannel{}, all, changed).Watches(&kwerftv1.GitConnection{}, all, changed).
			// A new Cloud API token (Settings marks it on ConsoleSettings):
			// hand it to the Cloud clusters at once.
			Watches(&kwerftv1.ConsoleSettings{}, all, builder.WithPredicates(predicate.AnnotationChangedPredicate{}))
	}
	if r.Tunnel != nil {
		events := make(chan event.GenericEvent)
		if err := mgr.Add(&tunnelEvents{tunnel: r.Tunnel, reader: mgr.GetCache(), out: events}); err != nil {
			return err
		}
		b = b.WatchesRawSource(source.Channel(events, &handler.EnqueueRequestForObject{}))
	}
	return b.Complete(r)
}

// tunnelEvents queues every Cluster whenever an agent connects or leaves.
type tunnelEvents struct {
	tunnel ClusterTunnel
	reader client.Reader
	out    chan<- event.GenericEvent
}

func (t *tunnelEvents) Start(ctx context.Context) error {
	for {
		changed := t.tunnel.Changed()
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
		var list kwerftv1.ClusterList
		if err := t.reader.List(ctx, &list); err != nil {
			log.FromContext(ctx).Error(err, "listing clusters after an agent change")
			continue
		}
		for i := range list.Items {
			select {
			case t.out <- event.GenericEvent{Object: &list.Items[i]}:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// ClusterReady reports a Cluster's Ready condition, for tests and callers
// that only need a yes or no.
func ClusterReady(c *kwerftv1.Cluster) bool {
	return meta.IsStatusConditionTrue(c.Status.Conditions, ConditionReady)
}

// deleteCloudNetwork deletes the private network Kwerft created for a
// hetzner-cloud cluster (node pools' clusterNetwork), once its servers are
// gone; wait while Hetzner still counts something attached.
func (r *ClusterReconciler) deleteCloudNetwork(ctx context.Context, c *kwerftv1.Cluster) (wait bool, err error) {
	if r.HCloud == nil {
		return false, nil
	}
	hc, err := r.HCloud(ctx)
	if errors.Is(err, errNoCloudToken) {
		log.FromContext(ctx).Info("no Cloud API token: the cluster's network is left in the project", "cluster", c.Name)
		return false, nil
	} else if err != nil {
		return false, err
	}
	nets, err := hc.Networks(ctx, hetzner.LabelCluster+"="+c.Name+","+DNSLabelManagedBy+"="+ManagedByKwerft)
	if err != nil {
		return false, err
	}
	for _, n := range nets {
		var apiErr *hetzner.APIError
		if err := hc.DeleteNetwork(ctx, n.ID); errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			return true, nil
		} else if err != nil {
			return false, err
		}
	}
	return false, nil
}
