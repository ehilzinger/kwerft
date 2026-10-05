package server

import (
	"context"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
)

// DNS records of remote clusters' hostnames (docs/phase5.md › DNS for remote
// clusters): the console's DNS reconciler reports them on each Cluster in
// the management cluster. The console reads them with its own identity (cached): they
// name only hostnames and addresses, shown next to Domains the user already
// sees.

// clusterDNS maps cluster → hostname → the record kept for it.
func (a *api) clusterDNS(ctx context.Context) map[string]map[string]kwerftv1.DNSRecordStatus {
	if a.cfg.System == nil {
		return nil
	}
	var list kwerftv1.ClusterList
	if err := a.cfg.System.List(ctx, &list); err != nil {
		return nil
	}
	out := map[string]map[string]kwerftv1.DNSRecordStatus{}
	for _, c := range list.Items {
		if c.Status.DNS == nil {
			continue
		}
		recs := map[string]kwerftv1.DNSRecordStatus{}
		for _, rec := range c.Status.DNS.Records {
			recs[rec.Hostname] = rec
		}
		out[c.Name] = recs
	}
	return out
}

// attachClusterDNS adds the record state to Domains of remote clusters.
func (a *api) attachClusterDNS(ctx context.Context, domains []domainJSON) {
	var byCluster map[string]map[string]kwerftv1.DNSRecordStatus
	for i := range domains {
		d := &domains[i]
		if d.Cluster == "" || d.Cluster == clusters.Local {
			continue
		}
		if byCluster == nil {
			if byCluster = a.clusterDNS(ctx); byCluster == nil {
				return
			}
		}
		if rec, ok := byCluster[d.Cluster][d.Hostname]; ok && (rec.Project == "" || rec.Project == d.Project) {
			v := dnsRecordView(rec)
			d.DNS = &v
		}
	}
}
