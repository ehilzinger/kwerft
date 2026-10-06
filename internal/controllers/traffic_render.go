package controllers

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Network policies as Cilium sees them.
//
// Cilium is the CNI (kube-proxy replacement), so Kwerft writes
// CiliumNetworkPolicies rather than Kubernetes NetworkPolicies: only Cilium's
// kind can name the nodes (entities host and remote-node), which is where
// traffic from the ingress comes from — Traefik runs on the host network, and
// on another node than the app it reaches the app as "remote-node".
//
// What every project gets, all additive (Cilium allows what any policy
// allows):
//
//   - Project "default-deny.project" (isolated projects only): every pod of
//     the namespace denies ingress unless another policy allows it.
//   - App "<app>": the App's pods accept, on the App's ports, the ingress
//     (host, remote-node), platform namespaces (labelled kwerft.dev/system),
//     the apps and their Tasks listed in spec.allowFrom and, when the project
//     is not isolated, every project's pods. Outbound follows spec.egress
//     (none: DNS and the cluster; https: plus port 443 on public addresses
//     and on the nodes, which is this cluster's own ingress; all: no
//     limit). Apps of the same project do not reach each other unless
//     allowFrom or a TrafficRule says so.
//   - Task "<task>.task": the Task's pods accept nothing; outbound as an
//     App's.
//   - TrafficRule "<rule>.traffic-in" / "<rule>.traffic-out": what the rule
//     allows on top, never denying anything (enableDefaultDeny off), so a
//     rule cannot cut an App off from what its own policy allows.
//
// Names: App names are DNS labels without dots, so the dotted names of the
// project's and the rules' policies never collide with an App's.

// CiliumNetworkPolicyGVK is Cilium's namespaced policy kind. Kwerft does not
// import Cilium's Go types; the policies are unstructured.
var CiliumNetworkPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}

const (
	// ProjectPolicyName is the isolated project's default-deny policy.
	ProjectPolicyName = "default-deny.project"

	// LabelTrafficRule marks the policies a TrafficRule became.
	LabelTrafficRule = "kwerft.dev/traffic-rule"

	// Cilium's label keys for a pod's namespace and its namespace's labels.
	ciliumNamespace       = "k8s:io.kubernetes.pod.namespace"
	ciliumNamespaceLabels = "k8s:io.cilium.k8s.namespace.labels."
)

// privateRanges are never "the internet": RFC 1918 and link-local (the
// cloud metadata service) stay out of internet egress.
var privateRanges = []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"}

// TrafficPolicyNames are the CiliumNetworkPolicies a TrafficRule may become:
// one for what its project's apps receive, one for what they send.
func TrafficPolicyNames(rule string) (ingress, egress string) {
	return rule + ".traffic-in", rule + ".traffic-out"
}

// newCiliumPolicy is an unstructured CiliumNetworkPolicy owned by owner.
func newCiliumPolicy(name, namespace string, labels map[string]string, owner metav1.OwnerReference, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CiliumNetworkPolicyGVK)
	u.SetName(name)
	u.SetNamespace(namespace)
	u.SetLabels(labels)
	u.SetOwnerReferences([]metav1.OwnerReference{owner})
	u.Object["spec"] = spec
	return u
}

func ownerRef(owner metav1.Object, gvk schema.GroupVersionKind) metav1.OwnerReference {
	return *metav1.NewControllerRef(owner, gvk)
}

// ---- selectors ---------------------------------------------------------------

func matchLabels(kv ...string) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return map[string]any{"matchLabels": m}
}

func exists(key string) map[string]any {
	return map[string]any{"key": key, "operator": "Exists"}
}

// appEndpoint selects the pods of app in namespace, and the pods of Tasks
// that run as that app (fromApp), which carry its network identity.
func appEndpoints(namespace, app string) []any {
	return []any{
		matchLabels(ciliumNamespace, namespace, LabelApp, app),
		matchLabels(ciliumNamespace, namespace, LabelAsApp, app),
	}
}

// systemEndpoints selects pods of every namespace labelled kwerft.dev/system.
// Cilium confines a selector without a namespace key to the policy's own
// namespace, hence the explicit "any namespace".
func systemEndpoints() map[string]any {
	return map[string]any{
		"matchLabels":      map[string]any{ciliumNamespaceLabels + LabelSystem: "true"},
		"matchExpressions": []any{exists(ciliumNamespace)},
	}
}

// projectEndpoints selects the pods of every project namespace.
func projectEndpoints() map[string]any {
	return map[string]any{"matchExpressions": []any{exists(ciliumNamespaceLabels + LabelProject), exists(ciliumNamespace)}}
}

// anyPodEndpoints selects every pod in the cluster.
func anyPodEndpoints() map[string]any {
	return map[string]any{"matchExpressions": []any{exists(ciliumNamespace)}}
}

// ingressEntities is where traffic through the ingress comes from: Traefik on
// this node's host network (host) or another node's (remote-node). Kubelet
// probes come from host too. The same entities are where the ingress is
// reached from inside (appEgress).
func ingressEntities() []any { return []any{"host", "remote-node"} }

func toPorts(ports ...map[string]any) []any {
	if len(ports) == 0 {
		return nil
	}
	list := make([]any, len(ports))
	for i, p := range ports {
		list[i] = p
	}
	return []any{map[string]any{"ports": list}}
}

func port(n int32, proto corev1.Protocol) map[string]any {
	return map[string]any{"port": strconv.Itoa(int(n)), "protocol": string(proto)}
}

// withPorts adds toPorts to rule unless ports is empty (every port).
func withPorts(rule map[string]any, ports []any) map[string]any {
	if len(ports) > 0 {
		rule["toPorts"] = ports
	}
	return rule
}

// ---- Apps and Projects -------------------------------------------------------

// ciliumPolicy is the App's CiliumNetworkPolicy (see the package comment
// above). isolated is the project's spec.isolated.
func (a *appRender) ciliumPolicy(isolated bool) *unstructured.Unstructured {
	var ps []map[string]any
	for _, p := range a.app.Spec.Ports {
		ps = append(ps, port(p.Container, protocol(p)))
	}
	ports := toPorts(ps...)

	peers := []any{systemEndpoints()}
	for _, ref := range a.app.Spec.AllowFrom {
		project, app := a.project, ref
		if before, after, ok := strings.Cut(ref, "/"); ok {
			project, app = before, after
		}
		peers = append(peers, appEndpoints(project, app)...)
	}
	if !isolated {
		peers = append(peers, projectEndpoints())
	}
	ingress := []any{
		withPorts(map[string]any{"fromEntities": ingressEntities()}, ports),
		withPorts(map[string]any{"fromEndpoints": peers}, ports),
	}

	spec := map[string]any{
		"endpointSelector": matchLabels(LabelApp, a.app.Name),
		"ingress":          ingress,
	}
	if egress := appEgress(a.app.Spec.Egress); egress != nil {
		spec["egress"] = egress
	}
	return newCiliumPolicy(a.app.Name, a.app.Namespace, a.labels,
		ownerRef(a.app, kwerftv1.GroupVersion.WithKind("App")), spec)
}

// appEgress limits outbound traffic: none (DNS and the cluster only), https
// (plus port 443 on public addresses and on the nodes) or all (nil: no
// egress policy). In the cluster the target's ingress policy decides.
//
// The nodes need a rule of their own: a hostname this cluster serves (an
// App's, the console's) resolves to a node's public address, and Cilium
// gives every node address the identity host or remote-node, which CIDR
// rules never match. Without it a job checking its own App over HTTPS
// times out (hatchure, 2026-10-06). Only 443 is opened there, which is
// Traefik on the host network; the nodes' other ports (SSH, the
// Kubernetes API on 6443, the kubelet, metrics) stay closed.
func appEgress(egress string) []any {
	if egress == "" {
		egress = "https"
	}
	if egress == "all" {
		return nil
	}
	out := []any{
		map[string]any{
			"toEndpoints": []any{matchLabels(ciliumNamespace, "kube-system", "k8s-app", "kube-dns")},
			"toPorts":     toPorts(port(53, corev1.ProtocolUDP), port(53, corev1.ProtocolTCP)),
		},
		map[string]any{"toEndpoints": []any{anyPodEndpoints()}},
	}
	if egress == "https" {
		out = append(out, map[string]any{
			"toCIDRSet": []any{internetCIDR()},
			"toPorts":   toPorts(port(443, corev1.ProtocolTCP)),
		}, map[string]any{
			"toEntities": ingressEntities(),
			"toPorts":    toPorts(port(443, corev1.ProtocolTCP)),
		})
	}
	return out
}

func internetCIDR() map[string]any {
	return map[string]any{"cidr": "0.0.0.0/0", "except": slices.Clone(privateRanges)}
}

// projectPolicy denies all ingress to the isolated project's pods; App and
// TrafficRule policies open it selectively.
func projectPolicy(p *kwerftv1.Project) *unstructured.Unstructured {
	return newCiliumPolicy(ProjectPolicyName, p.Name, map[string]string{LabelManagedBy: ManagedByKwerft},
		ownerRef(p, kwerftv1.GroupVersion.WithKind("Project")),
		map[string]any{
			"endpointSelector": map[string]any{},
			// An empty rule allows nothing but turns on default deny.
			"ingress": []any{map[string]any{}},
		})
}

// projectIsolated reports a Project's isolation; the default is isolated.
func projectIsolated(p *kwerftv1.Project) bool {
	return p == nil || p.Spec.Isolated == nil || *p.Spec.Isolated
}

// ---- TrafficRules ------------------------------------------------------------

// TrafficFieldError is a TrafficRule spec problem, with the field it is in.
type TrafficFieldError struct {
	Field   string // e.g. spec.from[0].app
	Message string
}

func (e *TrafficFieldError) Error() string { return e.Field + ": " + e.Message }

// PeerApp splits an app peer ("api" or "project/api") into its project and
// app; a bare name is in project.
func PeerApp(project, ref string) (string, string) {
	if before, after, ok := strings.Cut(ref, "/"); ok {
		return before, after
	}
	return project, ref
}

// local reports whether peer is (some or all of) project's own apps.
func local(project string, peer kwerftv1.TrafficPeer) bool {
	switch {
	case peer.App != "":
		p, _ := PeerApp(project, peer.App)
		return p == project
	case peer.Project != "":
		return peer.Project == project
	}
	return false
}

// ValidateTrafficRule checks what the CRD cannot: names, CIDRs, port ranges,
// and that a rule which sends traffic out of project (to the internet, a
// CIDR or another project) has only project's own apps as sources.
func ValidateTrafficRule(project string, spec *kwerftv1.TrafficRuleSpec) error {
	if len(spec.From) == 0 {
		return &TrafficFieldError{"spec.from", "Add at least one source."}
	}
	if len(spec.To) == 0 {
		return &TrafficFieldError{"spec.to", "Add at least one destination."}
	}
	check := func(side string, i int, p kwerftv1.TrafficPeer) error {
		field := fmt.Sprintf("spec.%s[%d]", side, i)
		n := 0
		for _, set := range []bool{p.App != "", p.Project != "", p.Internet, p.CIDR != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			return &TrafficFieldError{field, "Pick exactly one of an app, a project, the internet or an address range."}
		}
		switch {
		case p.App != "":
			parts := strings.Split(p.App, "/")
			if len(parts) > 2 {
				return &TrafficFieldError{field + ".app", `Name an app as "app" or "project/app".`}
			}
			for _, part := range parts {
				if len(validation.IsDNS1123Label(part)) > 0 {
					return &TrafficFieldError{field + ".app", fmt.Sprintf("%q is not an app or project name.", part)}
				}
			}
		case p.Project != "":
			if len(validation.IsDNS1123Label(p.Project)) > 0 {
				return &TrafficFieldError{field + ".project", fmt.Sprintf("%q is not a project name.", p.Project)}
			}
		case p.CIDR != "":
			ip, nw, err := net.ParseCIDR(p.CIDR)
			if err != nil {
				return &TrafficFieldError{field + ".cidr", "Write an address range like 10.0.0.0/16."}
			}
			if !ip.Equal(nw.IP) {
				return &TrafficFieldError{field + ".cidr", fmt.Sprintf("Write the range as %s (no host bits).", nw)}
			}
		}
		return nil
	}
	for i, p := range spec.From {
		if err := check("from", i, p); err != nil {
			return err
		}
	}
	outward := false
	for i, p := range spec.To {
		if err := check("to", i, p); err != nil {
			return err
		}
		outward = outward || !local(project, p)
	}
	if outward {
		for i, p := range spec.From {
			if !local(project, p) {
				return &TrafficFieldError{fmt.Sprintf("spec.from[%d]", i),
					"A rule that reaches the internet, an address range or another project sends traffic from this project, so its sources must be apps of " + project + "."}
			}
		}
	}
	for i, p := range spec.Ports {
		field := fmt.Sprintf("spec.ports[%d]", i)
		if p.Port < 1 || p.Port > 65535 {
			return &TrafficFieldError{field + ".port", "Ports go from 1 to 65535."}
		}
		if p.EndPort != 0 && (p.EndPort < p.Port || p.EndPort > 65535) {
			return &TrafficFieldError{field + ".endPort", "The range must end at or after its first port, at most 65535."}
		}
		if p.Protocol != "" && p.Protocol != "TCP" && p.Protocol != "UDP" {
			return &TrafficFieldError{field + ".protocol", "TCP or UDP."}
		}
	}
	return nil
}

// trafficPorts renders a rule's ports; empty means every port.
func trafficPorts(ports []kwerftv1.TrafficPort) []any {
	var ps []map[string]any
	for _, p := range ports {
		proto := corev1.Protocol(p.Protocol)
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		m := port(p.Port, proto)
		if p.EndPort > p.Port {
			m["endPort"] = int64(p.EndPort)
		}
		ps = append(ps, m)
	}
	return toPorts(ps...)
}

// localSelector selects the apps of project that peers name (all local).
func localSelector(project string, peers []kwerftv1.TrafficPeer) map[string]any {
	var apps []string
	for _, p := range peers {
		if p.Project != "" {
			// Every app of the project; Tasks and other pods stay closed.
			return map[string]any{"matchExpressions": []any{exists(LabelApp)}}
		}
		_, app := PeerApp(project, p.App)
		if !slices.Contains(apps, app) {
			apps = append(apps, app)
		}
	}
	slices.Sort(apps)
	values := make([]any, len(apps))
	for i, a := range apps {
		values[i] = a
	}
	return map[string]any{"matchExpressions": []any{map[string]any{"key": LabelApp, "operator": "In", "values": values}}}
}

// trafficPolicies renders a valid rule (ValidateTrafficRule) in project:
// an ingress policy on the destinations in project, and an egress policy on
// the (local) sources for destinations outside it. Either may be nil.
//
// Crossing projects needs consent from the receiving side: an egress policy
// here only lets the sources send; the other project's own policies (its
// Apps' allowFrom, its TrafficRules, or no isolation) decide whether its
// apps accept. See TrafficRuleReconciler.
func trafficPolicies(rule *kwerftv1.TrafficRule, project string) (ingress, egress *unstructured.Unstructured) {
	ports := trafficPorts(rule.Spec.Ports)
	labels := map[string]string{LabelManagedBy: ManagedByKwerft, LabelProject: project}
	if len(validation.IsValidLabelValue(rule.Name)) == 0 {
		labels[LabelTrafficRule] = rule.Name
	}
	owner := ownerRef(rule, kwerftv1.GroupVersion.WithKind("TrafficRule"))
	// Only add allows: a rule never turns on default deny for the pods it
	// selects (an App with egress "all" keeps it).
	additive := map[string]any{"ingress": false, "egress": false}
	inName, outName := TrafficPolicyNames(rule.Name)

	var receivers, outside []kwerftv1.TrafficPeer
	for _, p := range rule.Spec.To {
		if local(project, p) {
			receivers = append(receivers, p)
		} else {
			outside = append(outside, p)
		}
	}

	if len(receivers) > 0 {
		var rules []any
		for _, p := range rule.Spec.From {
			switch {
			case p.App != "":
				q, app := PeerApp(project, p.App)
				rules = append(rules, withPorts(map[string]any{"fromEndpoints": appEndpoints(q, app)}, ports))
			case p.Project != "":
				rules = append(rules, withPorts(map[string]any{"fromEndpoints": []any{matchLabels(ciliumNamespace, p.Project)}}, ports))
			case p.Internet:
				// Through the ingress (Traefik on the nodes), or straight
				// from outside should a node ever forward to the pod.
				rules = append(rules, withPorts(map[string]any{"fromEntities": []any{"host", "remote-node", "world"}}, ports))
			case p.CIDR != "":
				rules = append(rules, withPorts(map[string]any{"fromCIDRSet": []any{map[string]any{"cidr": p.CIDR}}}, ports))
			}
		}
		ingress = newCiliumPolicy(inName, rule.Namespace, labels, owner, map[string]any{
			"endpointSelector":  localSelector(project, receivers),
			"enableDefaultDeny": additive,
			"ingress":           rules,
		})
	}

	if len(outside) > 0 {
		var rules []any
		for _, p := range outside {
			switch {
			case p.App != "":
				q, app := PeerApp(project, p.App)
				rules = append(rules, withPorts(map[string]any{"toEndpoints": []any{matchLabels(ciliumNamespace, q, LabelApp, app)}}, ports))
			case p.Project != "":
				rules = append(rules, withPorts(map[string]any{"toEndpoints": []any{matchLabels(ciliumNamespace, p.Project)}}, ports))
			case p.Internet:
				rules = append(rules, withPorts(map[string]any{"toCIDRSet": []any{internetCIDR()}}, ports))
			case p.CIDR != "":
				rules = append(rules, withPorts(map[string]any{"toCIDRSet": []any{map[string]any{"cidr": p.CIDR}}}, ports))
			}
		}
		egress = newCiliumPolicy(outName, rule.Namespace, labels, owner, map[string]any{
			"endpointSelector":  localSelector(project, rule.Spec.From),
			"enableDefaultDeny": additive,
			"egress":            rules,
		})
	}
	return ingress, egress
}
