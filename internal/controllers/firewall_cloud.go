// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/firewall"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// The Cloud Firewall sync: one Hetzner Cloud Firewall per cluster, named
// kwerft-<cluster>-<instance>, holding firewall.CloudRules of the rule set
// as last confirmed on the nodes, applied to the label selector
// kwerft.dev/cluster=<cluster> and to the cluster's other Cloud servers by
// ID. The host firewall stays the inner layer.
//
// Never lock out: the rules always keep SSH, HTTP, HTTPS, WireGuard and ICMP
// as the required rules say (checked once more before anything is sent),
// a narrowing of SSH arrives only after the console confirmed it on the
// hosts, and a rule set that does not fit into one Cloud Firewall leaves
// the firewall as it is. Should all else fail, the Hetzner Console can
// detach the firewall (Firewalls › kwerft-… › Resources).

// cloudRuleSet is the rule set to mirror: the confirmed snapshot, with the
// revision it belongs to, and each current rule's state relative to it.
func (r *HetznerCloudReconciler) cloudRuleSet(ctx context.Context) ([]kwerftv1.FirewallRule, string, map[string]bool, error) {
	var rules kwerftv1.FirewallRuleList
	if err := r.List(ctx, &rules); err != nil {
		return nil, "", nil, err
	}
	var cm corev1.ConfigMap
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap}, &cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, "", nil, err
	}
	var snap FirewallSnapshot
	if raw := cm.Data[firewall.SnapshotKey]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &snap)
	}
	pending := map[string]bool{}
	if snap.Revision == "" {
		// Nothing confirmed yet (no node agent has reported): mirror the
		// current rules, but never a narrowing of SSH, which only the
		// confirmation on the hosts may bring.
		out := make([]kwerftv1.FirewallRule, 0, len(rules.Items))
		for _, rule := range rules.Items {
			rule := *rule.DeepCopy()
			if rule.Name == firewall.RuleSSH {
				if len(rule.Spec.Sources) > 0 {
					pending[rule.Name] = true
				}
				rule.Spec.Sources = nil
			}
			out = append(out, rule)
		}
		return out, "", pending, nil
	}
	out := make([]kwerftv1.FirewallRule, 0, len(snap.Rules))
	for _, e := range snap.Rules {
		rule := kwerftv1.FirewallRule{Spec: e.Spec}
		rule.Name = e.Name
		if e.Required {
			rule.Labels = map[string]string{firewall.LabelRequired: "true"}
		}
		out = append(out, rule)
	}
	for _, cur := range rules.Items {
		i := slices.IndexFunc(snap.Rules, func(e FirewallSnapshotEntry) bool { return e.Name == cur.Name })
		if i < 0 || cloudRelevant(snap.Rules[i].Spec) != cloudRelevant(cur.Spec) {
			pending[cur.Name] = true
		}
	}
	return out, snap.Revision, pending, nil
}

// cloudRelevant is the part of a rule the Cloud Firewall depends on (not
// the description, not which nodes: the Cloud Firewall opens a
// control-plane rule's port on every Cloud server and the host firewall
// narrows it).
func cloudRelevant(spec kwerftv1.FirewallRuleSpec) string {
	srcs, _ := firewall.NormalizeSources(spec.Sources)
	return fmt.Sprint(spec.Port, spec.EndPort, spec.Protocol, srcs, spec.Disabled)
}

func (r *HetznerCloudReconciler) syncFirewall(ctx context.Context, hz *hetzner.Client, s *kwerftv1.ConsoleSettings, instance string, cloud []CloudNode) (*kwerftv1.CloudFirewallStatus, map[string]string, error) {
	existing, err := hz.Firewalls(ctx, r.ownedSelector(instance))
	if err != nil {
		return previousFirewall(s, err), nil, err
	}
	slices.SortFunc(existing, func(a, b hetzner.Firewall) int { return int(a.ID - b.ID) })

	if s.Spec.HetznerCloud != nil && s.Spec.HetznerCloud.Firewall == kwerftv1.CloudFirewallOff {
		for _, fw := range existing {
			if err := r.removeFirewall(ctx, hz, fw); err != nil {
				return &kwerftv1.CloudFirewallStatus{State: "Error", ID: fw.ID, Name: fw.Name, Message: "Could not remove the Cloud Firewall: " + err.Error()}, nil, err
			}
		}
		states := map[string]string{}
		var rules kwerftv1.FirewallRuleList
		if err := r.List(ctx, &rules); err != nil {
			return nil, nil, err
		}
		for _, rule := range rules.Items {
			states[rule.Name] = firewall.CloudOff
		}
		return &kwerftv1.CloudFirewallStatus{State: "Off", Message: "Turned off in Settings; the host firewall still applies."}, states, nil
	}

	set, revision, pending, err := r.cloudRuleSet(ctx)
	if err != nil {
		return previousFirewall(s, err), nil, err
	}
	rules, states, err := firewall.CloudRules(set)
	for name := range pending {
		states[name] = firewall.CloudPending
	}
	if err != nil {
		st := previousFirewall(s, err)
		st.State = "Error"
		return st, states, nil
	}
	if !firewall.CloudKeepsBaseline(rules) {
		// Cannot happen with CloudRules; refuse rather than lock anyone out.
		return &kwerftv1.CloudFirewallStatus{State: "Error", Message: "Refused to send rules without SSH, HTTP, HTTPS and WireGuard."}, states, nil
	}

	want := []hetzner.FirewallResource{hetzner.SelectorResource(CloudSelector(r.cluster()))}
	for _, n := range cloud {
		if !n.Labelled {
			want = append(want, hetzner.ServerResource(n.Server.ID))
		}
	}

	var fw hetzner.Firewall
	if len(existing) == 0 {
		fw, err = hz.CreateFirewall(ctx, r.resourceName(instance), r.ownedLabels(instance), rules, want)
		var apiErr *hetzner.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "uniqueness_error" {
			return &kwerftv1.CloudFirewallStatus{State: "Error", Name: r.resourceName(instance),
				Message: "The project already has a firewall named " + r.resourceName(instance) + " that Kwerft did not create. Rename or delete it in the Hetzner Console."}, states, nil
		}
		if err != nil {
			return firewallError(fw, "Could not create the Cloud Firewall", err), states, err
		}
	} else {
		fw = existing[0]
		if !firewall.SameCloudRules(fw.Rules, rules) {
			if err := hz.SetFirewallRules(ctx, fw.ID, rules); err != nil {
				return firewallError(fw, "Could not update the Cloud Firewall's rules", err), states, err
			}
		}
		var add, remove []hetzner.FirewallResource
		for _, w := range want {
			if !slices.ContainsFunc(fw.AppliedTo, func(a hetzner.FirewallResource) bool { return a.Key() == w.Key() }) {
				add = append(add, w)
			}
		}
		for _, a := range fw.AppliedTo {
			if !slices.ContainsFunc(want, func(w hetzner.FirewallResource) bool { return a.Key() == w.Key() }) {
				remove = append(remove, a)
			}
		}
		// One at a time, so a server that cannot take it (already five
		// firewalls) does not keep it from the others.
		var applyErr error
		for _, a := range add {
			if err := hz.ApplyFirewall(ctx, fw.ID, a); err != nil {
				applyErr = errors.Join(applyErr, fmt.Errorf("%s: %w", a.Key(), err))
			}
		}
		if applyErr != nil {
			return firewallError(fw, "Could not apply the Cloud Firewall to every server of the cluster", applyErr), states, applyErr
		}
		if err := hz.RemoveFirewall(ctx, fw.ID, remove...); err != nil && !errors.Is(err, hetzner.ErrNotFound) {
			return firewallError(fw, "Could not remove the Cloud Firewall from servers that left the cluster", err), states, err
		}
		// Duplicates (two syncs that raced) go; the oldest stays.
		for _, extra := range existing[1:] {
			_ = r.removeFirewall(ctx, hz, extra)
		}
	}
	if fresh, err := hz.GetFirewall(ctx, fw.ID); err == nil {
		fw = fresh
	}
	st := &kwerftv1.CloudFirewallStatus{State: "InSync", ID: fw.ID, Name: fw.Name, Rules: int32(len(fw.Rules)),
		Servers: int32(fw.Servers()), Revision: revision}
	if st.Servers == 0 {
		st.Message = "No Cloud server of this cluster in the token's project yet; the firewall applies to servers labelled " + CloudSelector(r.cluster()) + " as they appear."
	}
	if len(pending) > 0 {
		names := make([]string, 0, len(pending))
		for n := range pending {
			names = append(names, n)
		}
		slices.Sort(names)
		st.State = "Applying"
		st.Message = "Waiting for the nodes to confirm " + strings.Join(names, ", ") + "; the Cloud Firewall follows once they do."
	}
	return st, states, nil
}

// removeFirewall takes a firewall off everything and deletes it.
func (r *HetznerCloudReconciler) removeFirewall(ctx context.Context, hz *hetzner.Client, fw hetzner.Firewall) error {
	res := make([]hetzner.FirewallResource, 0, len(fw.AppliedTo))
	for _, a := range fw.AppliedTo {
		res = append(res, hetzner.FirewallResource{Type: a.Type, Server: a.Server, LabelSelector: a.LabelSelector})
	}
	if err := hz.RemoveFirewall(ctx, fw.ID, res...); err != nil && !errors.Is(err, hetzner.ErrNotFound) {
		return err
	}
	return hz.DeleteFirewall(ctx, fw.ID)
}

func previousFirewall(s *kwerftv1.ConsoleSettings, err error) *kwerftv1.CloudFirewallStatus {
	st := &kwerftv1.CloudFirewallStatus{State: "Error"}
	if s.Status.HetznerCloud != nil && s.Status.HetznerCloud.Firewall != nil {
		st = s.Status.HetznerCloud.Firewall.DeepCopy()
	}
	if err != nil {
		st.State, st.Message = "Error", err.Error()
	}
	return st
}

func firewallError(fw hetzner.Firewall, what string, err error) *kwerftv1.CloudFirewallStatus {
	return &kwerftv1.CloudFirewallStatus{State: "Error", ID: fw.ID, Name: fw.Name, Rules: int32(len(fw.Rules)),
		Servers: int32(fw.Servers()), Message: fmt.Sprintf("%s: %v", what, err)}
}
