package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/firewall"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// The Hetzner Cloud reconciler does what the Cloud API token (Settings ›
// Hetzner Cloud API) makes possible, in one pass per change:
//
//   - finds the cluster's nodes among the token project's servers;
//   - keeps one Cloud Firewall per cluster with the public part of the
//     confirmed FirewallRules (firewall_cloud.go);
//   - keeps the optional Load Balancer in front of the ingress
//     (hcloud_loadbalancer.go);
//   - keeps the token of the cloud-controller-manager and the CSI driver
//     (Secret kube-system/hcloud, which install.sh creates) equal to the
//     console's (hcloud_token.go).
//
// Cloud resources it creates carry kwerft.dev/managed-by=kwerft, this
// installation's kwerft.dev/instance (the kube-system UID) and
// kwerft.dev/cluster=<cluster>, and it only ever changes or deletes
// resources with all three. Servers are never changed: the firewall and
// the Load Balancer select the cluster's servers by the label
// kwerft.dev/cluster=<cluster> (servers Kwerft created) and name the
// other nodes (the installer's first server) one by one. Dedicated servers
// (label kwerft.dev/platform=dedicated) are never touched.

const (
	// HCloudTokenSecret holds the Cloud API token (key "token") in
	// kwerft-system: created empty by the Domain reconciler, patched by
	// owners and admins (or install.sh --config hcloud.tokenFile), read only
	// by Kwerft's own identity.
	HCloudTokenSecret = "kwerft-hcloud-token"
	HCloudTokenKey    = "token"
	// AnnotationHCloudTokenUpdated on ConsoleSettings: when the token was
	// last written (Settings shows whether one is set; a change starts a
	// sync).
	AnnotationHCloudTokenUpdated = "kwerft.dev/hcloud-token-updated-at"

	// CloudLabelCluster is the label of a cluster's Cloud servers (set by
	// the node pools) and of Kwerft's Cloud resources.
	CloudLabelCluster = "kwerft.dev/cluster"
	// LabelPlatform is the installer's node label: cloud or dedicated.
	LabelPlatform = "kwerft.dev/platform"

	hcloudResync = 5 * time.Minute
	hcloudRetry  = 30 * time.Second
)

var hcloudRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "hetzner-cloud"}}

// HetznerCloudReconciler: see above.
type HetznerCloudReconciler struct {
	client.Client
	// APIReader reads Secrets and the firewall ConfigMap uncached; nil falls
	// back to the client (tests).
	APIReader client.Reader

	// HetznerAPI is the Cloud API base URL; empty is production.
	HetznerAPI string
	// ClusterName names this cluster in Cloud labels ("local" for the
	// management cluster).
	ClusterName string
	// ProxyNetwork is the private network Traefik accepts the PROXY
	// protocol from (install.sh); the Load Balancer needs it.
	ProxyNetwork string

	Now func() time.Time

	instance string
}

func (r *HetznerCloudReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *HetznerCloudReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *HetznerCloudReconciler) cluster() string {
	if r.ClusterName != "" {
		return r.ClusterName
	}
	return "local"
}

// CloudSelector selects a cluster's Cloud servers.
func CloudSelector(cluster string) string { return CloudLabelCluster + "=" + cluster }

// ownedSelector selects this installation's Cloud resources of the cluster.
func (r *HetznerCloudReconciler) ownedSelector(instance string) string {
	return DNSLabelManagedBy + "=" + ManagedByKwerft + "," + DNSLabelInstance + "=" + instance + "," + CloudSelector(r.cluster())
}

func (r *HetznerCloudReconciler) ownedLabels(instance string) map[string]string {
	return map[string]string{DNSLabelManagedBy: ManagedByKwerft, DNSLabelInstance: instance, CloudLabelCluster: r.cluster()}
}

// resourceName names a Cloud resource of this installation and cluster:
// kwerft-<cluster>-<instance prefix> (unique per project, readable).
func (r *HetznerCloudReconciler) resourceName(instance string) string {
	short := strings.ReplaceAll(instance, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return "kwerft-" + r.cluster() + "-" + short
}

func (r *HetznerCloudReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	token, err := HCloudToken(ctx, r.reader())
	if err != nil {
		return ctrl.Result{}, err
	}
	if token == "" && r.cluster() != "local" {
		// A remote Cloud cluster: the console hands it only the CCM's and
		// CSI driver's Secret (docs/phase5.md › Security), which serves its
		// own Cloud Firewall and Load Balancer as well.
		if token, err = systemHCloudToken(ctx, r.reader()); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.mirrorToken(ctx, token); err != nil {
		log.FromContext(ctx).Error(err, "keeping the hcloud token of the CCM and CSI driver")
	}
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if token == "" {
		st := &kwerftv1.HetznerCloudStatus{Message: "No Hetzner Cloud API token is stored. Enter one under Settings › Hetzner Cloud API."}
		if prev := s.Status.HetznerCloud; prev != nil {
			st.Firewall, st.LoadBalancer = prev.Firewall, prev.LoadBalancer
		}
		if err := r.report(ctx, &s, st); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.ruleStates(ctx, nil)
	}
	instance, err := r.instanceID(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	hz := &hetzner.Client{Token: token, Base: r.HetznerAPI}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}
	servers, err := hz.ServerSummaries(ctx, "")
	if msg, after, ok := cloudTrouble(err); ok {
		st := s.Status.HetznerCloud.DeepCopy()
		if st == nil {
			st = &kwerftv1.HetznerCloudStatus{}
		}
		st.Message = msg
		return ctrl.Result{RequeueAfter: after}, r.report(ctx, &s, st)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	cloud := MatchCloudNodes(nodes.Items, servers, r.cluster())

	st := &kwerftv1.HetznerCloudStatus{SyncedAt: &metav1.Time{Time: r.now()}}
	for _, n := range cloud {
		st.Servers = append(st.Servers, kwerftv1.CloudServerStatus{Node: n.Node, ID: n.Server.ID, Name: n.Server.Name,
			Location: n.Server.Location.Name, Labelled: n.Labelled})
	}
	after := hcloudResync

	fw, states, fwErr := r.syncFirewall(ctx, hz, &s, instance, cloud)
	st.Firewall = fw
	if fwErr != nil || (fw != nil && fw.State == "Error") {
		after = hcloudRetry
	}
	lb, lbErr := r.syncLoadBalancer(ctx, hz, &s, instance, cloud)
	st.LoadBalancer = lb
	if lbErr != nil || (lb != nil && lb.State != "Active") {
		after = hcloudRetry
	}
	for _, e := range []error{fwErr, lbErr} {
		if msg, wait, ok := cloudTrouble(e); ok {
			st.Message = msg
			after = max(wait, hcloudRetry)
		} else if e != nil {
			log.FromContext(ctx).Error(e, "Hetzner Cloud sync")
		}
	}
	if err := r.report(ctx, &s, st); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ruleStates(ctx, states); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// cloudTrouble turns errors that concern the whole sync into a message: a
// rejected token or the rate limit.
func cloudTrouble(err error) (string, time.Duration, bool) {
	var rl *hetzner.RateLimitError
	switch {
	case err == nil:
		return "", 0, false
	case errors.Is(err, hetzner.ErrForbidden):
		return "The Hetzner Cloud API token may only read. Enter a Read & Write token under Settings › Hetzner Cloud API.", hcloudResync, true
	case errors.Is(err, hetzner.ErrTokenRejected):
		return "Hetzner rejected the Cloud API token. Enter a Read & Write token under Settings › Hetzner Cloud API.", hcloudResync, true
	case errors.As(err, &rl):
		return rl.Error(), time.Until(rl.Reset) + time.Second, true
	}
	return "", 0, false
}

// HCloudToken reads the stored Cloud API token ("" when none is set).
func HCloudToken(ctx context.Context, c client.Reader) (string, error) {
	var sec corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: HCloudTokenSecret}, &sec)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(sec.Data[HCloudTokenKey])), nil
}

func (r *HetznerCloudReconciler) instanceID(ctx context.Context) (string, error) {
	if r.instance != "" {
		return r.instance, nil
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		return "", fmt.Errorf("read kube-system for the instance ID: %w", err)
	}
	r.instance = string(ns.UID)
	return r.instance, nil
}

// CloudNode is a node that is a Hetzner Cloud server of the token's project.
type CloudNode struct {
	Node   string
	Server hetzner.ServerSummary
	// Labelled: the server carries kwerft.dev/cluster=<cluster>.
	Labelled bool
	// PrivateIP is the node's address in a private network, if any.
	PrivateIP netip.Addr
}

// MatchCloudNodes finds the nodes among the servers: by provider ID
// (hcloud://<id>, set by the cloud-controller-manager), else by address.
// Dedicated nodes (kwerft.dev/platform=dedicated) are left out.
func MatchCloudNodes(nodes []corev1.Node, servers []hetzner.ServerSummary, cluster string) []CloudNode {
	var out []CloudNode
	for _, n := range nodes {
		if n.Labels[LabelPlatform] == "dedicated" {
			continue
		}
		var addrs []netip.Addr
		for _, a := range n.Status.Addresses {
			if ip, err := netip.ParseAddr(a.Address); err == nil {
				addrs = append(addrs, ip.Unmap())
			}
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(n.Spec.ProviderID, "hcloud://"), 10, 64)
		if !strings.HasPrefix(n.Spec.ProviderID, "hcloud://") {
			id = 0
		}
		i := slices.IndexFunc(servers, func(s hetzner.ServerSummary) bool {
			if id != 0 {
				return s.ID == id
			}
			return slices.ContainsFunc(addrs, s.HasAddress)
		})
		if i < 0 {
			continue
		}
		srv := servers[i]
		cn := CloudNode{Node: n.Name, Server: srv, Labelled: srv.Labels[CloudLabelCluster] == cluster}
		for _, p := range srv.PrivateNet {
			if ip, err := netip.ParseAddr(p.IP); err == nil {
				cn.PrivateIP = ip
				break
			}
		}
		out = append(out, cn)
	}
	slices.SortFunc(out, func(a, b CloudNode) int { return strings.Compare(a.Node, b.Node) })
	return out
}

// report writes status.hetznerCloud and nothing else.
func (r *HetznerCloudReconciler) report(ctx context.Context, s *kwerftv1.ConsoleSettings, st *kwerftv1.HetznerCloudStatus) error {
	if prev := s.Status.HetznerCloud; prev != nil && st != nil && st.SyncedAt != nil && prev.SyncedAt != nil &&
		r.now().Sub(prev.SyncedAt.Time) < time.Hour {
		same := st.DeepCopy()
		same.SyncedAt = prev.SyncedAt
		if equality.Semantic.DeepEqual(prev, same) {
			return nil
		}
	}
	if equality.Semantic.DeepEqual(s.Status.HetznerCloud, st) {
		return nil
	}
	orig := s.DeepCopy()
	s.Status.HetznerCloud = st
	if err := patchStatus(ctx, r.Client, s, orig); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ruleStates writes each FirewallRule's status.cloudFirewall (nil states:
// no token, so the field is cleared).
func (r *HetznerCloudReconciler) ruleStates(ctx context.Context, states map[string]string) error {
	var rules kwerftv1.FirewallRuleList
	if err := r.List(ctx, &rules); err != nil {
		return err
	}
	for i := range rules.Items {
		rule := &rules.Items[i]
		if !rule.DeletionTimestamp.IsZero() || rule.Status.CloudFirewall == states[rule.Name] {
			continue
		}
		orig := rule.DeepCopy()
		rule.Status.CloudFirewall = states[rule.Name]
		if err := patchStatus(ctx, r.Client, rule, orig); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}

// hcloudInputs is what a sync depends on in ConsoleSettings: its spec and
// the token's change marker, not the status other reconcilers write.
func hcloudInputs(s *kwerftv1.ConsoleSettings) string {
	raw, _ := json.Marshal([]any{s.Spec.HetznerCloud, s.Annotations[AnnotationHCloudTokenUpdated], s.Annotations[AnnotationHCloudTokenSum]})
	return string(raw)
}

func (r *HetznerCloudReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{hcloudRequest}
	})
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}}}
	// The firewall's desired rules and confirmed snapshot: an informer of
	// its own for exactly that ConfigMap.
	desired, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(), HTTPClient: mgr.GetHTTPClient(),
		ByObject: map[client.Object]cache.ByObject{&corev1.ConfigMap{}: {
			Namespaces: map[string]cache.Config{firewall.Namespace: {}},
			Field:      fields.OneTermEqualSelector("metadata.name", firewall.DesiredConfigMap),
		}},
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(desired); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("hetzner-cloud").
		WatchesRawSource(source.Channel(start, all)).
		WatchesRawSource(source.Kind(desired, client.Object(&corev1.ConfigMap{}), all)).
		Watches(&kwerftv1.ConsoleSettings{}, all, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return hcloudInputs(e.ObjectOld.(*kwerftv1.ConsoleSettings)) != hcloudInputs(e.ObjectNew.(*kwerftv1.ConsoleSettings))
			},
		})).
		// Nodes joining or leaving, or changing addresses or provider IDs.
		Watches(&corev1.Node{}, all, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				o, n := e.ObjectOld.(*corev1.Node), e.ObjectNew.(*corev1.Node)
				return o.Spec.ProviderID != n.Spec.ProviderID || o.Labels[LabelPlatform] != n.Labels[LabelPlatform] ||
					!equality.Semantic.DeepEqual(o.Status.Addresses, n.Status.Addresses)
			},
		})).
		Complete(r)
}
