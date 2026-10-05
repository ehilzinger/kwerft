package controllers

import (
	"context"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
)

// Records for remote clusters' hostnames (docs/phase5.md › DNS for remote
// clusters). Only the console holds the DNS token, so its DNS reconciler
// also keeps the records of the hostnames remote clusters serve under the
// console's apps domain: one A/AAAA pair per hostname, pointing at that
// cluster's public addresses. Being more specific than *.<appsDomain>, it
// wins over the wildcard, which keeps pointing at the console's cluster.
// The Cluster reconciler reports each cluster's hostnames and addresses in
// its status (cluster_hostnames.go); this side never uses the tunnel, so a
// slow cluster cannot hold up the console's own records.
//
// Rules:
//   - A hostname the console's own cluster holds stays its own (the
//     wildcard serves it); a remote cluster wanting it is told so.
//   - Of two remote clusters wanting a hostname, the older claim gets it.
//   - Only names one label below the apps domain get records: a record for
//     a.b.<apps> would make b.<apps> an empty non-terminal, which the
//     wildcard no longer answers for.
//   - A disconnected cluster keeps its last reported hostnames and
//     addresses, so its records stay; a deleted cluster's go.
//   - Records are labelled and owned as the console's own; anything else is
//     left alone and reported, as for the console's hostnames.

// purposeApp marks the records of a remote cluster's hostname.
const purposeApp = "app"

// remoteClusters are the Clusters other than the management cluster, when
// this reconciler keeps their records (the console's process).
func (r *DNSReconciler) remoteClusters(ctx context.Context) ([]kwerftv1.Cluster, error) {
	if !r.RemoteClusters {
		return nil, nil
	}
	var list kwerftv1.ClusterList
	if err := r.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []kwerftv1.Cluster
	for _, c := range list.Items {
		if c.Name != clusters.Local && c.Spec.Provider != kwerftv1.ClusterLocal {
			out = append(out, c)
		}
	}
	return out, nil
}

// remoteHosts are the remote clusters' hostnames that get records, and the
// statuses of those settled without the provider, per cluster.
func (r *DNSReconciler) remoteHosts(ctx context.Context, s *kwerftv1.ConsoleSettings, console []dnsHost, remotes []kwerftv1.Cluster) ([]dnsHost, map[string][]kwerftv1.DNSRecordStatus, error) {
	settled := map[string][]kwerftv1.DNSRecordStatus{}
	apps := s.Spec.AppsDomain
	if apps == "" || len(remotes) == 0 {
		return nil, settled, nil
	}
	reserved := map[string]bool{}
	for _, h := range console {
		reserved[h.host] = true
	}
	// Hostnames the console's own cluster holds: the wildcard serves them.
	var domains kwerftv1.DomainList
	if err := r.List(ctx, &domains); err != nil {
		return nil, nil, err
	}
	local := map[string]string{}
	for _, d := range domains.Items {
		if d.Status.Listener != "" && d.DeletionTimestamp.IsZero() {
			local[d.Spec.Hostname] = d.Namespace
		}
	}

	type claim struct {
		cluster *kwerftv1.Cluster
		host    kwerftv1.ClusterHostname
	}
	claims := map[string][]claim{}
	settle := func(cluster string, h kwerftv1.ClusterHostname, state kwerftv1.DNSRecordState, msg string) {
		settled[cluster] = append(settled[cluster], kwerftv1.DNSRecordStatus{Hostname: h.Hostname, Purpose: purposeApp,
			Project: h.Project, State: state, Message: msg})
	}
	for i := range remotes {
		c := &remotes[i]
		if !c.DeletionTimestamp.IsZero() {
			continue // its records go with it
		}
		for _, h := range c.Status.Hostnames {
			label, ok := strings.CutSuffix(h.Hostname, "."+apps)
			switch {
			case !ok || label == "":
				// From before the apps domain changed; the cluster's next
				// report drops it.
			case strings.Contains(label, "."):
				parent := label[strings.Index(label, ".")+1:] + "." + apps
				settle(c.Name, h, kwerftv1.DNSUnsupported, "Kwerft keeps records only for names one label below "+apps+
					": one for "+h.Hostname+" would stop *."+apps+" from answering for "+parent+
					". Create its record by hand, or use a name like "+strings.ReplaceAll(label, ".", "-")+"."+apps+".")
			case reserved[h.Hostname]:
				settle(c.Name, h, kwerftv1.DNSConflict, h.Hostname+" is one of the console's own hostnames.")
			case local[h.Hostname] != "":
				settle(c.Name, h, kwerftv1.DNSConflict, h.Hostname+" is used by project "+local[h.Hostname]+
					" in the console's cluster, which *."+apps+" points to; Kwerft does not point it at this cluster.")
			default:
				claims[h.Hostname] = append(claims[h.Hostname], claim{c, h})
			}
		}
	}

	var out []dnsHost
	for host, cs := range claims {
		slices.SortFunc(cs, func(a, b claim) int {
			if n := a.host.Since.Compare(b.host.Since.Time); n != 0 {
				return n
			}
			return strings.Compare(a.cluster.Name, b.cluster.Name)
		})
		win := cs[0]
		for _, lost := range cs[1:] {
			settle(lost.cluster.Name, lost.host, kwerftv1.DNSConflict, host+" is also used by project "+win.host.Project+
				" in cluster "+win.cluster.Name+", which claimed it first; the record points there.")
		}
		h := dnsHost{host: host, purpose: purposeApp, cluster: win.cluster.Name, project: win.host.Project}
		h.v4, h.v6 = publicDNSAddresses(win.cluster.Status.PublicAddresses)
		if len(h.v4)+len(h.v6) == 0 {
			h.hold = true
			h.note = "Waiting for the cluster's public addresses; records it already has stay as they are."
		}
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b dnsHost) int { return strings.Compare(a.host, b.host) })
	return out, settled, nil
}

// stalledClusters keeps each cluster's last records when the sync could not
// run, with the reason.
func stalledClusters(remotes []kwerftv1.Cluster, msg string) map[string]*kwerftv1.DNSStatus {
	out := map[string]*kwerftv1.DNSStatus{}
	for _, c := range remotes {
		if len(c.Status.Hostnames) == 0 {
			continue
		}
		st := &kwerftv1.DNSStatus{Message: msg}
		if c.Status.DNS != nil {
			st.Records, st.Zones, st.SyncedAt = c.Status.DNS.Records, c.Status.DNS.Zones, c.Status.DNS.SyncedAt
		}
		out[c.Name] = st
	}
	return out
}

// unmanagedClusters: with managed records off, clusters with hostnames are
// told to create them by hand.
func unmanagedClusters(remotes []kwerftv1.Cluster, apps string) map[string]*kwerftv1.DNSStatus {
	out := map[string]*kwerftv1.DNSStatus{}
	for _, c := range remotes {
		if len(c.Status.Hostnames) > 0 {
			out[c.Name] = &kwerftv1.DNSStatus{Message: "Kwerft keeps no DNS records under " + apps +
				" (Settings › Let Kwerft create the DNS records). Point each of this cluster's hostnames at its addresses by hand."}
		}
	}
	return out
}

// reportClusters writes status.dns of each remote cluster. A merge patch of
// that field alone: the Cluster reconciler writes the rest of the status, and
// a conflict on its fields must not repeat the whole sync.
func (r *DNSReconciler) reportClusters(ctx context.Context, res map[string]*kwerftv1.DNSStatus, remotes []kwerftv1.Cluster) error {
	for i := range remotes {
		c := &remotes[i]
		next := res[c.Name]
		if !c.DeletionTimestamp.IsZero() || r.sameDNS(c.Status.DNS, next) {
			continue
		}
		orig := c.DeepCopy()
		c.Status.DNS = next
		if err := r.Status().Patch(ctx, c, client.MergeFrom(orig)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// clusterDNSInputs is what a Cluster contributes to the records.
func clusterDNSInputs(c *kwerftv1.Cluster) []any {
	return []any{c.Status.PublicAddresses, c.Status.Hostnames, c.DeletionTimestamp != nil}
}

// clusterChanged lets through Cluster events that change records.
var clusterChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return !equality.Semantic.DeepEqual(clusterDNSInputs(e.ObjectOld.(*kwerftv1.Cluster)), clusterDNSInputs(e.ObjectNew.(*kwerftv1.Cluster)))
	},
}

// domainClaimedRemotely queues the sync when a Domain of the console's
// cluster is about a hostname a remote cluster wants: the local claim wins.
func (r *DNSReconciler) domainClaimedRemotely(reader client.Reader) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		d, ok := obj.(*kwerftv1.Domain)
		if !ok {
			return nil
		}
		var list kwerftv1.ClusterList
		if err := reader.List(ctx, &list); err != nil {
			return nil
		}
		for _, c := range list.Items {
			for _, h := range c.Status.Hostnames {
				if h.Hostname == d.Spec.Hostname {
					return []reconcile.Request{dnsRequest}
				}
			}
		}
		return nil
	}
}
