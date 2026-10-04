package hubble

import (
	"fmt"
	"net/netip"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// What a TrafficRule did, in Hubble's terms:
//
//   - Allowed: connections whose policy verdict names one of the rule's
//     CiliumNetworkPolicies (controllers.TrafficPolicyNames) in allowed_by.
//   - Dropped: policy drops between the rule's sources and destinations
//     (any port): traffic the rule is about but did not let through, such
//     as a port it does not list. A drop does not name the policy that
//     would have allowed it, so this is the closest a rule gets.

// RuleCounts implements controllers.TrafficCounter: nil until the relay has
// been heard from.
func (a *Aggregator) RuleCounts(rule *kwerftv1.TrafficRule) *kwerftv1.TrafficCounts {
	st := a.Status()
	if st.Since.IsZero() {
		return nil
	}
	in, out := controllers.TrafficPolicyNames(rule.Name)
	since := metav1.NewTime(st.Since.Truncate(60e9)) // a minute: no status write per flow
	return &kwerftv1.TrafficCounts{
		Allowed: a.Allowed(rule.Namespace, in, out),
		Dropped: a.dropped(rule),
		Since:   &since,
	}
}

func (a *Aggregator) dropped(rule *kwerftv1.TrafficRule) int64 {
	var n int64
	for _, d := range a.Drops(Unconfined(), func(k DropKey) bool { return RuleCovers(rule, k) }, 0) {
		n += d.Count
	}
	return n
}

// RuleCovers reports whether a drop went from one of rule's sources to one
// of its destinations (ports aside).
func RuleCovers(rule *kwerftv1.TrafficRule, k DropKey) bool {
	project := rule.Namespace
	from, to := false, false
	for _, p := range rule.Spec.From {
		from = from || peerMatches(project, p, k.From, true)
	}
	for _, p := range rule.Spec.To {
		to = to || peerMatches(project, p, k.To, false)
	}
	return from && to
}

func peerMatches(project string, p kwerftv1.TrafficPeer, s Side, source bool) bool {
	switch {
	case p.App != "":
		q, app := controllers.PeerApp(project, p.App)
		return s.Kind == "pod" && s.Namespace == q && s.App == app
	case p.Project != "":
		return s.Kind == "pod" && s.Namespace == p.Project
	case p.Internet:
		if source {
			// Through the ingress: Traefik on the nodes.
			return s.Kind == "world" || s.Kind == "host" || s.Kind == "remote-node" || s.Kind == "ingress"
		}
		return s.Kind == "world" && !private(s.IP)
	case p.CIDR != "":
		return s.Kind == "world" && inCIDR(s.IP, p.CIDR)
	}
	return false
}

func private(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && (a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLoopback())
}

// Suggestion is a TrafficRule that would have allowed a drop, for the
// console's "Create allow rule".
type Suggestion struct {
	Project string                   `json:"project"`
	Name    string                   `json:"name"`
	Spec    kwerftv1.TrafficRuleSpec `json:"spec"`
}

// Suggest proposes a rule for a drop, in the project that has to allow it:
// the destination's for traffic into an app, the source's for traffic out
// to an address outside the cluster. nil when no rule can help (a platform
// pod on either side, or neither side an app).
func Suggest(d DropKey) *Suggestion {
	var ports []kwerftv1.TrafficPort
	if d.Port > 0 && (d.Protocol == "TCP" || d.Protocol == "UDP") {
		ports = []kwerftv1.TrafficPort{{Port: int32(d.Port), Protocol: d.Protocol}}
	}
	switch {
	case d.To.Kind == "pod" && d.To.App != "" && !d.Egress:
		project := d.To.Namespace
		var from kwerftv1.TrafficPeer
		switch {
		case d.From.Kind == "pod" && d.From.App != "" && d.From.Namespace == project:
			from.App = d.From.App
		case d.From.Kind == "pod" && d.From.App != "":
			from.App = d.From.Namespace + "/" + d.From.App
		case d.From.Kind == "pod" && d.From.Namespace != "":
			from.Project = d.From.Namespace
		case d.From.Kind == "world" || d.From.Kind == "host" || d.From.Kind == "remote-node" || d.From.Kind == "ingress":
			from.Internet = true
		default:
			return nil
		}
		return &Suggestion{
			Project: project,
			Name:    ruleName(peerWord(from), d.To.App),
			Spec:    kwerftv1.TrafficRuleSpec{From: []kwerftv1.TrafficPeer{from}, To: []kwerftv1.TrafficPeer{{App: d.To.App}}, Ports: ports},
		}
	case d.From.Kind == "pod" && d.From.App != "" && d.To.Kind == "world" && d.To.IP != "":
		to := kwerftv1.TrafficPeer{Internet: true}
		word := "internet"
		if private(d.To.IP) {
			bits := 32
			if strings.Contains(d.To.IP, ":") {
				bits = 128
			}
			to = kwerftv1.TrafficPeer{CIDR: fmt.Sprintf("%s/%d", d.To.IP, bits)}
			word = strings.NewReplacer(".", "-", ":", "-").Replace(d.To.IP)
		}
		return &Suggestion{
			Project: d.From.Namespace,
			Name:    ruleName(d.From.App, word),
			Spec:    kwerftv1.TrafficRuleSpec{From: []kwerftv1.TrafficPeer{{App: d.From.App}}, To: []kwerftv1.TrafficPeer{to}, Ports: ports},
		}
	}
	return nil
}

func peerWord(p kwerftv1.TrafficPeer) string {
	switch {
	case p.App != "":
		return strings.ReplaceAll(p.App, "/", "-")
	case p.Project != "":
		return p.Project
	}
	return "internet"
}

// ruleName is "<from>-to-<to>", cut to a valid name.
func ruleName(from, to string) string {
	name := strings.Trim(from+"-to-"+to, "-")
	if len(name) > 50 {
		name = strings.Trim(name[:50], "-")
	}
	if len(validation.IsDNS1123Label(name)) > 0 {
		return "allow-traffic"
	}
	return name
}
