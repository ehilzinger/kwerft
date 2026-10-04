package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// DNS records for the console and the apps domain (ConsoleSettings
// spec.dns.manageRecords): A and AAAA records for the console hostname (and,
// during a move, the new and the previous one) and *.<appsDomain>, pointing
// at the nodes' public addresses. App hostnames need nothing more: the
// wildcard record covers every name under the apps domain, whatever the
// certificate method, so developers never cause DNS writes.
//
// Ownership lives in the provider: Kwerft labels the RRsets it creates with
// its instance (the kube-system namespace UID) and only ever changes or
// deletes RRsets with that label. Records someone else created are left
// alone; if they point elsewhere the hostname is reported as a conflict. A
// reinstalled server (a new instance) takes over the records of the old one;
// the old one, should it still run, sees its records taken and stops.

const (
	// Labels on the RRsets Kwerft creates.
	DNSLabelManagedBy = "kwerft.dev/managed-by"
	DNSLabelInstance  = "kwerft.dev/instance"

	dnsTTL = 300
	// dnsResync catches records changed by hand in the Hetzner Console.
	dnsResync = 10 * time.Minute
	dnsRetry  = time.Minute
)

var dnsRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: kwerftv1.ConsoleSettingsName}}

// DNSReconciler keeps the managed records in step with ConsoleSettings.
type DNSReconciler struct {
	client.Client
	// APIReader reads the token Secret; nil falls back to the client (tests).
	APIReader client.Reader

	ConsoleDomain string // console hostname from the flag, like DomainReconciler's
	HetznerAPI    string // Cloud API base URL; empty is production

	Now func() time.Time

	instance string
}

func (r *DNSReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// dnsHost is one hostname Kwerft wants records for.
type dnsHost struct {
	host, purpose string
}

// wantedHosts are the console's hostnames and the apps wildcard, in the
// order the Settings page lists them.
func (r *DNSReconciler) wantedHosts(s *kwerftv1.ConsoleSettings) []dnsHost {
	active := s.Status.ConsoleDomain
	if active == "" {
		active = r.ConsoleDomain
	}
	if active == "" {
		active = s.Spec.ConsoleDomain
	}
	var out []dnsHost
	seen := map[string]bool{}
	add := func(h, purpose string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, dnsHost{h, purpose})
		}
	}
	add(active, "console")
	if s.Spec.ConsoleDomain != active {
		add(s.Spec.ConsoleDomain, "console-next")
	}
	add(s.Status.PreviousConsoleDomain, "console-previous")
	if s.Spec.AppsDomain != "" {
		add("*."+s.Spec.AppsDomain, "apps")
	}
	return out
}

// publicDNSAddresses keeps the addresses DNS may point at: global unicast,
// not private (the node status falls back to internal addresses).
func publicDNSAddresses(addrs []string) (v4, v6 []string) {
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			continue
		}
		if ip.Is4() || ip.Is4In6() {
			v4 = append(v4, ip.Unmap().String())
		} else {
			v6 = append(v6, ip.String())
		}
	}
	slices.Sort(v4)
	slices.Sort(v6)
	return slices.Compact(v4), slices.Compact(v6)
}

func (r *DNSReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if s.Spec.DNS == nil || !s.Spec.DNS.ManageRecords {
		// Records stay where they are; only the report goes.
		return ctrl.Result{}, r.report(ctx, &s, nil)
	}
	st, after, err := r.sync(ctx, &s)
	if rerr := r.report(ctx, &s, st); rerr != nil {
		return ctrl.Result{}, rerr
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// sync brings the provider in line and returns the status to report and when
// to look again. An error means retry with backoff.
func (r *DNSReconciler) sync(ctx context.Context, s *kwerftv1.ConsoleSettings) (*kwerftv1.DNSStatus, time.Duration, error) {
	logger := log.FromContext(ctx)
	prev := map[string]kwerftv1.DNSRecordStatus{}
	if s.Status.DNS != nil {
		for _, rec := range s.Status.DNS.Records {
			prev[rec.Hostname] = rec
		}
	}
	// Problems before any record is looked at keep the last known records.
	stalled := func(msg string) *kwerftv1.DNSStatus {
		st := &kwerftv1.DNSStatus{Message: msg}
		if s.Status.DNS != nil {
			st.Records, st.Zones, st.SyncedAt = s.Status.DNS.Records, s.Status.DNS.Zones, s.Status.DNS.SyncedAt
		}
		return st
	}

	v4, v6 := publicDNSAddresses(s.Status.PublicAddresses)
	if len(v4)+len(v6) == 0 {
		return stalled("Waiting for the nodes' public addresses; no records are changed until they are known."), dnsRetry, nil
	}
	token, err := r.token(ctx)
	if err != nil {
		return nil, 0, err
	}
	if token == "" {
		return stalled("No DNS API token is stored. Enter one under Settings."), 0, nil
	}
	instance, err := r.instanceID(ctx)
	if err != nil {
		return nil, 0, err
	}
	hz := &hetzner.Client{Token: token, Base: r.HetznerAPI}
	zones, err := hz.Zones(ctx)
	if msg, after, ok := providerTrouble(err); ok {
		return stalled(msg), after, nil
	} else if err != nil {
		return stalled("Could not list the DNS zones: " + err.Error()), 0, err
	}

	// Which zone each wanted host lives in, and which zones to read: those,
	// plus zones that held managed records last time (to clean them up).
	wanted := r.wantedHosts(s)
	type placed struct {
		dnsHost
		zone hetzner.Zone
		name string
	}
	var hosts []placed
	var out []kwerftv1.DNSRecordStatus
	read := map[string]bool{}
	for _, h := range wanted {
		z, name, ok := hetzner.MatchZone(zones, h.host)
		switch {
		case !ok:
			out = append(out, kwerftv1.DNSRecordStatus{Hostname: h.host, Purpose: h.purpose, State: kwerftv1.DNSNoZone,
				Message: "No zone in the token's Hetzner project contains " + h.host + "; create its records by hand."})
		case z.Mode != "" && z.Mode != "primary":
			out = append(out, kwerftv1.DNSRecordStatus{Hostname: h.host, Purpose: h.purpose, Zone: z.Name, State: kwerftv1.DNSNoZone,
				Message: "The zone " + z.Name + " is a secondary zone; its records are managed at its primary."})
		default:
			hosts = append(hosts, placed{h, z, name})
			read[z.Name] = true
		}
	}
	for _, rec := range prev {
		if rec.Zone != "" && rec.State == kwerftv1.DNSManaged {
			if z, _, ok := hetzner.MatchZone(zones, rec.Zone); ok && z.Name == rec.Zone && (z.Mode == "" || z.Mode == "primary") {
				read[rec.Zone] = true
			}
		}
	}

	existing := map[string]map[string]hetzner.RRSet{} // zone → "name/TYPE"
	for zone := range read {
		sets, err := hz.RRSets(ctx, zone, "A", "AAAA", "CNAME")
		if msg, after, ok := providerTrouble(err); ok {
			return stalled(msg), after, nil
		} else if err != nil {
			return stalled("Could not read the zone " + zone + ": " + err.Error()), 0, err
		}
		existing[zone] = map[string]hetzner.RRSet{}
		for _, set := range sets {
			existing[zone][set.Name+"/"+set.Type] = set
		}
	}

	labels := map[string]string{DNSLabelManagedBy: ManagedByKwerft, DNSLabelInstance: instance}
	ours := func(set hetzner.RRSet) bool {
		return set.Labels[DNSLabelManagedBy] == ManagedByKwerft && set.Labels[DNSLabelInstance] == instance
	}
	kwerfts := func(set hetzner.RRSet) bool { return set.Labels[DNSLabelManagedBy] == ManagedByKwerft }
	want := map[string][]string{"A": v4, "AAAA": v6}
	all := append(slices.Clone(v4), v6...)
	keep := map[string]bool{} // zone/name/TYPE of managed sets to keep
	failed := false
	var rateLimited *hetzner.RateLimitError

	for _, h := range hosts {
		rec := kwerftv1.DNSRecordStatus{Hostname: h.host, Purpose: h.purpose, Zone: h.zone.Name}
		sets := existing[h.zone.Name]
		cname, hasCNAME := sets[h.name+"/CNAME"]
		var foreign, others []hetzner.RRSet
		for _, t := range []string{"A", "AAAA"} {
			if set, ok := sets[h.name+"/"+t]; ok {
				switch {
				case ours(set):
				case kwerfts(set):
					others = append(others, set)
				default:
					foreign = append(foreign, set)
				}
			}
		}

		switch {
		case hasCNAME:
			rec.State, rec.Values = kwerftv1.DNSConflict, cname.Values()
			rec.Message = h.host + " has a CNAME record (" + strings.Join(cname.Values(), ", ") + ") that Kwerft did not create. Delete it in the Hetzner Console to let Kwerft manage the name."
		case len(foreign) > 0:
			vals := valuesOf(foreign)
			rec.Values = vals
			if subset(vals, all) {
				rec.State = kwerftv1.DNSExternal
				rec.Message = "Records Kwerft did not create already point here; Kwerft leaves them alone."
			} else {
				rec.State = kwerftv1.DNSConflict
				rec.Message = h.host + " has records Kwerft did not create, pointing to " + strings.Join(vals, ", ") +
					" instead of " + strings.Join(all, ", ") + ". Change or delete them in the Hetzner Console; Kwerft does not overwrite them."
			}
		case len(others) > 0 && (prev[h.host].State == kwerftv1.DNSManaged || prev[h.host].State == kwerftv1.DNSTakenOver):
			rec.State, rec.Values = kwerftv1.DNSTakenOver, valuesOf(others)
			rec.Message = "Another Kwerft installation took over these records (" + strings.Join(rec.Values, ", ") +
				"). This one no longer changes them; delete them in the Hetzner Console to manage them from here again."
		default:
			rec.State, rec.Values = kwerftv1.DNSManaged, all
			for _, t := range []string{"A", "AAAA"} {
				set, has := sets[h.name+"/"+t]
				err := r.ensureSet(ctx, hz, h.zone.Name, h.name, t, want[t], set, has, labels, ours)
				if err != nil {
					if errors.As(err, &rateLimited) {
						break
					}
					failed = true
					rec.State = kwerftv1.DNSError
					rec.Message = fmt.Sprintf("Could not write the %s record: %v", t, err)
					logger.Error(err, "DNS record", "host", h.host, "type", t)
				}
				if len(want[t]) > 0 {
					keep[h.zone.Name+"/"+h.name+"/"+t] = true
				}
			}
			if rec.State == kwerftv1.DNSManaged && len(others) > 0 {
				rec.Message = "Taken over from an earlier Kwerft installation."
			}
		}
		if rateLimited != nil {
			break
		}
		out = append(out, rec)
	}
	if rateLimited != nil {
		return stalled(rateLimited.Error()), time.Until(rateLimited.Reset) + time.Second, nil
	}

	// Remove this instance's records for names it no longer wants.
	for zone, sets := range existing {
		for _, set := range sets {
			if (set.Type != "A" && set.Type != "AAAA") || !ours(set) || keep[zone+"/"+set.Name+"/"+set.Type] {
				continue
			}
			if err := hz.DeleteRRSet(ctx, zone, set.Name, set.Type); err != nil {
				if msg, after, ok := providerTrouble(err); ok {
					return stalled(msg), after, nil
				}
				failed = true
				logger.Error(err, "delete DNS record", "zone", zone, "name", set.Name, "type", set.Type)
			}
		}
	}

	// List the hostnames in the order they were wanted.
	order := map[string]int{}
	for i, h := range wanted {
		order[h.host] = i
	}
	slices.SortStableFunc(out, func(a, b kwerftv1.DNSRecordStatus) int { return order[a.Hostname] - order[b.Hostname] })
	var primary []string
	for _, z := range zones {
		if z.Mode == "" || z.Mode == "primary" {
			primary = append(primary, strings.ToLower(strings.TrimSuffix(z.Name, ".")))
		}
	}
	slices.Sort(primary)
	st := &kwerftv1.DNSStatus{Records: out, Zones: primary, SyncedAt: &metav1.Time{Time: r.now()}}
	if failed {
		return st, dnsRetry, nil
	}
	return st, dnsResync, nil
}

// ensureSet makes the RRset of one name and type hold values, as this
// instance's: created, updated, relabelled (taken over) or deleted.
func (r *DNSReconciler) ensureSet(ctx context.Context, hz *hetzner.Client, zone, name, typ string, values []string,
	set hetzner.RRSet, has bool, labels map[string]string, ours func(hetzner.RRSet) bool) error {
	records := make([]hetzner.Record, 0, len(values))
	for _, v := range values {
		records = append(records, hetzner.Record{Value: v, Comment: "managed by Kwerft"})
	}
	switch {
	case !has && len(values) == 0:
		return nil
	case !has:
		ttl := dnsTTL
		return hz.CreateRRSet(ctx, zone, hetzner.RRSet{Name: name, Type: typ, TTL: &ttl, Labels: labels, Records: records})
	case len(values) == 0:
		return hz.DeleteRRSet(ctx, zone, name, typ)
	}
	if !ours(set) {
		if err := hz.SetLabels(ctx, zone, name, typ, labels); err != nil {
			return err
		}
	}
	have := set.Values()
	slices.Sort(have)
	if slices.Equal(have, values) {
		return nil
	}
	return hz.SetRecords(ctx, zone, name, typ, records)
}

// providerTrouble turns errors that concern the whole sync into a message:
// a rejected token or the rate limit. Retrying soon helps with neither.
func providerTrouble(err error) (string, time.Duration, bool) {
	var rl *hetzner.RateLimitError
	switch {
	case err == nil:
		return "", 0, false
	case errors.Is(err, hetzner.ErrTokenRejected):
		return "Hetzner rejected the DNS API token. Enter a Read & Write token under Settings.", 0, true
	case errors.As(err, &rl):
		return rl.Error(), time.Until(rl.Reset) + time.Second, true
	}
	return "", 0, false
}

func valuesOf(sets []hetzner.RRSet) []string {
	var out []string
	for _, s := range sets {
		out = append(out, s.Values()...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func subset(vals, of []string) bool {
	for _, v := range vals {
		if !slices.Contains(of, v) {
			return false
		}
	}
	return len(vals) > 0
}

func (r *DNSReconciler) token(ctx context.Context) (string, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var sec corev1.Secret
	err := reader.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: DNSTokenSecret}, &sec)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(sec.Data[DNSTokenKey])), nil
}

// instanceID names this installation in record labels: the kube-system
// namespace's UID, which lives as long as the cluster.
func (r *DNSReconciler) instanceID(ctx context.Context) (string, error) {
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

// report writes status.dns, and nothing else of the status, which the
// Domain reconciler owns.
func (r *DNSReconciler) report(ctx context.Context, s *kwerftv1.ConsoleSettings, st *kwerftv1.DNSStatus) error {
	if equality.Semantic.DeepEqual(s.Status.DNS, st) {
		return nil
	}
	// A new sync time alone is not worth a write every ten minutes unless
	// the last one is old.
	if s.Status.DNS != nil && st != nil && st.SyncedAt != nil && s.Status.DNS.SyncedAt != nil &&
		r.now().Sub(s.Status.DNS.SyncedAt.Time) < time.Hour {
		same := st.DeepCopy()
		same.SyncedAt = s.Status.DNS.SyncedAt
		if equality.Semantic.DeepEqual(s.Status.DNS, same) {
			return nil
		}
	}
	orig := s.DeepCopy()
	s.Status.DNS = st
	if err := patchStatus(ctx, r.Client, s, orig); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// dnsInputs is what the records depend on, so the reconciler's own status
// writes (and the Domain reconciler's certificate updates) do not trigger it.
func dnsInputs(s *kwerftv1.ConsoleSettings) string {
	raw, _ := json.Marshal([]any{s.Spec, s.Annotations[AnnotationDNSTokenUpdated],
		s.Status.ConsoleDomain, s.Status.PreviousConsoleDomain, s.Status.PublicAddresses})
	return string(raw)
}

func (r *DNSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toSettings := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{dnsRequest}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("dns").
		Watches(&kwerftv1.ConsoleSettings{}, toSettings, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return dnsInputs(e.ObjectOld.(*kwerftv1.ConsoleSettings)) != dnsInputs(e.ObjectNew.(*kwerftv1.ConsoleSettings))
			},
		})).
		Complete(r)
}
