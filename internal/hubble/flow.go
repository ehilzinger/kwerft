// Package hubble reads Cilium's flow log from the Hubble relay and keeps what
// the console shows about it: per policy, the connections it allowed in the
// last hour, and the connections policies dropped.
//
// Source: the relay's gRPC observer API (Observer/GetFlows, follow mode),
// which the installer enables with Hubble (not with --lite). Kwerft does not
// import Cilium's Go module (it would pin Kubernetes libraries); the few
// messages it needs are encoded and decoded here with protowire, after
// cilium/api/v1/observer/observer.proto and flow/flow.proto. Field numbers
// are noted next to each field; unknown fields are skipped, so newer relays
// keep working.
//
// Why not Hubble's Prometheus metrics in VictoriaMetrics: their labels carry
// workloads and verdicts but no policy names and no ports, so they cannot
// say which rule allowed a connection, nor which port a dropped one wanted.
package hubble

import (
	"slices"
	"strings"
	"time"
)

// Verdict of a flow (flow.Verdict).
type Verdict int32

const (
	VerdictUnknown   Verdict = 0
	VerdictForwarded Verdict = 1
	VerdictDropped   Verdict = 2
)

// Monitor event types (flow.CiliumEventType.type) Kwerft reads.
const (
	EventDrop          int32 = 1
	EventPolicyVerdict int32 = 5
)

// Drop reasons (flow.DropReason) that mean a policy said no.
const (
	DropPolicyDenied uint32 = 133 // no policy allows it (default deny)
	DropPolicyDeny   uint32 = 181 // a deny rule matched
)

// Direction of a flow relative to the endpoint that reported it.
type Direction int32

const (
	DirectionUnknown Direction = 0
	DirectionIngress Direction = 1
	DirectionEgress  Direction = 2
)

// Endpoint is one side of a flow (flow.Endpoint).
type Endpoint struct {
	Identity  uint32
	Namespace string
	Pod       string
	Labels    []string // "k8s:kwerft.dev/app=api", "reserved:world", ...
	IP        string
}

// Label returns the value of a Kubernetes pod label.
func (e Endpoint) Label(key string) string {
	for _, l := range e.Labels {
		if v, ok := strings.CutPrefix(l, "k8s:"+key+"="); ok {
			return v
		}
	}
	return ""
}

// Reserved returns Cilium's reserved identity of the endpoint ("world",
// "host", "remote-node", "kube-apiserver", ...), empty for pods.
func (e Endpoint) Reserved() string {
	for _, l := range e.Labels {
		if v, ok := strings.CutPrefix(l, "reserved:"); ok {
			return v
		}
	}
	return ""
}

// Policy names a policy in a flow's allowed_by lists (flow.Policy).
type Policy struct {
	Name      string
	Namespace string
}

// Flow is the part of a Hubble flow Kwerft uses.
type Flow struct {
	Time        time.Time
	Verdict     Verdict
	DropReason  uint32
	EventType   int32
	Source      Endpoint
	Destination Endpoint
	Protocol    string // TCP, UDP, SCTP, ICMP
	DstPort     uint32
	Direction   Direction
	Reply       bool
	// AllowedBy are the policies that allowed a forwarded policy verdict,
	// ingress and egress.
	AllowedBy []Policy
	// DestinationNames are DNS names Cilium saw for the destination.
	DestinationNames []string
}

// PolicyDrop reports whether f is a packet a network policy dropped.
func (f *Flow) PolicyDrop() bool {
	return f.Verdict == VerdictDropped && (f.DropReason == DropPolicyDenied || f.DropReason == DropPolicyDeny)
}

// Allowed reports whether f is a connection a policy allowed.
func (f *Flow) Allowed() bool {
	return f.EventType == EventPolicyVerdict && f.Verdict == VerdictForwarded && !f.Reply
}

func (f *Flow) allowedBy(p Policy) bool { return slices.Contains(f.AllowedBy, p) }
