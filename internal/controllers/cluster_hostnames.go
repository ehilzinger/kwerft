package controllers

import (
	"cmp"
	"context"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// App hostnames of remote clusters (docs/phase5.md › DNS for remote
// clusters). A remote cluster has no DNS token, so the console keeps the
// records of its hostnames under the console's apps domain: the Cluster
// reconciler, which reaches the cluster through the tunnel anyway, records
// where its ingress is reached and which of those hostnames its Domains
// hold; the DNS reconciler, which has the token and never uses the tunnel,
// turns that into records (dns_clusters.go). The cluster issues each
// hostname's certificate itself, through HTTP-01, once the record points
// there.

// observeIngress records the remote cluster's public addresses and the
// hostnames under the console's apps domain its Domains hold. Values stay as
// they were when the cluster cannot be read, so records survive a short
// outage.
func (r *ClusterReconciler) observeIngress(ctx context.Context, remote client.Client, c *kwerftv1.Cluster) error {
	// Without a ConsoleSettings its Domain reconciler reports no addresses;
	// an adopted cluster's installer writes none.
	var rs kwerftv1.ConsoleSettings
	err := remote.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &rs)
	if apierrors.IsNotFound(err) {
		err = remote.Create(ctx, &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}})
		if apierrors.IsAlreadyExists(err) {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	addrs := slices.Clone(rs.Status.PublicAddresses)
	slices.Sort(addrs)

	var hosts []kwerftv1.ClusterHostname
	if apps := r.appsDomain(ctx); apps != "" {
		var list kwerftv1.DomainList
		if err := remote.List(ctx, &list); err != nil {
			return err
		}
		hosts = heldHostnames(list.Items, apps)
	}
	if len(addrs) > 0 || len(c.Status.PublicAddresses) == 0 {
		c.Status.PublicAddresses = addrs
	}
	c.Status.Hostnames = hosts
	return nil
}

// appsDomain is the console's (the management cluster's) apps domain.
func (r *ClusterReconciler) appsDomain(ctx context.Context) string {
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		return ""
	}
	return s.Spec.AppsDomain
}

// heldHostnames are the hostnames below apps that won their claim in the
// cluster (the Domain reconciler gave them a listener for the current
// generation), one entry per hostname, the oldest claim's.
func heldHostnames(domains []kwerftv1.Domain, apps string) []kwerftv1.ClusterHostname {
	byHost := map[string]kwerftv1.ClusterHostname{}
	for _, d := range domains {
		host := d.Spec.Hostname
		if !strings.HasSuffix(host, "."+apps) || d.Status.Listener == "" ||
			d.Status.ObservedGeneration != d.Generation || !d.DeletionTimestamp.IsZero() {
			continue
		}
		h := kwerftv1.ClusterHostname{Hostname: host, Project: d.Namespace, Since: metav1.Time{Time: d.CreationTimestamp.Time.UTC()}}
		if cur, ok := byHost[host]; !ok || h.Since.Before(&cur.Since) {
			byHost[host] = h
		}
	}
	out := make([]kwerftv1.ClusterHostname, 0, len(byHost))
	for _, h := range byHost {
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b kwerftv1.ClusterHostname) int { return cmp.Compare(a.Hostname, b.Hostname) })
	if len(out) == 0 {
		return nil
	}
	if len(out) > 500 { // the CRD's limit
		out = out[:500]
	}
	return out
}
