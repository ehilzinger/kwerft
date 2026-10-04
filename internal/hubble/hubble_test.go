package hubble

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// ---- a fake relay ----------------------------------------------------------

// The encoders below write what Cilium's relay sends (flow.proto numbers), so
// the decoder is tested against the wire format, not against itself alone.

func msg(fields ...[]byte) []byte {
	var b []byte
	for _, f := range fields {
		b = append(b, f...)
	}
	return b
}

func str(num protowire.Number, s string) []byte {
	b := protowire.AppendTag(nil, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func sub(num protowire.Number, m []byte) []byte {
	b := protowire.AppendTag(nil, num, protowire.BytesType)
	return protowire.AppendBytes(b, m)
}

func varint(num protowire.Number, v uint64) []byte {
	b := protowire.AppendTag(nil, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func endpointPB(ns, pod string, labels ...string) []byte {
	m := msg(varint(1, 1234), varint(2, 5678), str(3, ns), str(5, pod))
	for _, l := range labels {
		m = append(m, str(4, l)...)
	}
	return m
}

// flowResponse encodes observer.GetFlowsResponse{flow: ...}.
func flowResponse(f *Flow) []byte {
	m := msg(
		sub(1, encodeTimestamp(f.Time)),
		varint(2, uint64(f.Verdict)),
		varint(3, uint64(f.DropReason)),
		sub(5, msg(str(1, f.Source.IP), str(2, f.Destination.IP), varint(3, 1))),
		sub(8, endpointPB(f.Source.Namespace, f.Source.Pod, f.Source.Labels...)),
		sub(9, endpointPB(f.Destination.Namespace, f.Destination.Pod, f.Destination.Labels...)),
		varint(10, 1), // Type L3_L4
		str(11, "node-1"),
		sub(19, msg(varint(1, uint64(f.EventType)))),
		varint(22, uint64(f.Direction)),
		varint(25, uint64(f.DropReason)),
		sub(26, varint(1, boolint(f.Reply))),
		str(100000, "summary we ignore"),
	)
	switch f.Protocol {
	case "TCP":
		m = append(m, sub(6, sub(1, msg(varint(1, 40000), varint(2, uint64(f.DstPort)), sub(3, varint(1, 1)))))...)
	case "UDP":
		m = append(m, sub(6, sub(2, msg(varint(1, 40000), varint(2, uint64(f.DstPort)))))...)
	}
	for _, n := range f.DestinationNames {
		m = append(m, str(14, n)...)
	}
	for _, p := range f.AllowedBy {
		m = append(m, sub(21002, msg(str(1, p.Name), str(2, p.Namespace), str(3, "k8s:io.cilium.k8s.policy.name="+p.Name), varint(4, 7)))...)
	}
	return msg(sub(1, m), str(1000, "node-1"))
}

func boolint(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func frame(m []byte) []byte {
	out := make([]byte, 5+len(m))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(m)))
	copy(out[5:], m)
	return out
}

type fakeRelay struct {
	mu       sync.Mutex
	requests [][]byte
	flows    []*Flow
	lost     uint64
	status   string // grpc-status in the trailer; "" = 0
}

func (f *fakeRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/observer.Observer/GetFlows" || r.Header.Get("Content-Type") != "application/grpc+proto" || r.ProtoMajor != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, body[5:])
	flows, lost, status := f.flows, f.lost, f.status
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
	for _, fl := range flows {
		_, _ = w.Write(frame(flowResponse(fl)))
	}
	if lost > 0 {
		_, _ = w.Write(frame(sub(3, msg(varint(1, 1), varint(2, lost)))))
	}
	w.(http.Flusher).Flush()
	if status == "" {
		status = "0"
	}
	w.Header().Set("Grpc-Status", status)
	w.Header().Set("Grpc-Message", "relay%20says%20no")
}

func startRelay(t *testing.T, f *fakeRelay) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(f)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// ---- tests ---------------------------------------------------------------------

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func appEP(ns, app string) Endpoint {
	return Endpoint{Namespace: ns, Pod: app + "-7d9f-abcde", IP: "10.42.0.7",
		Labels: []string{"k8s:kwerft.dev/app=" + app, "k8s:io.kubernetes.pod.namespace=" + ns}}
}

func world(ip string) Endpoint { return Endpoint{IP: ip, Labels: []string{"reserved:world"}} }

func TestRelayStreamsDecodedFlows(t *testing.T) {
	allowed := &Flow{Time: t0, Verdict: VerdictForwarded, EventType: EventPolicyVerdict, Direction: DirectionIngress,
		Source: appEP("shop", "web"), Destination: appEP("shop", "api"), Protocol: "TCP", DstPort: 8080,
		AllowedBy: []Policy{{Name: "web-to-api.traffic-in", Namespace: "shop"}}}
	dropped := &Flow{Time: t0.Add(time.Second), Verdict: VerdictDropped, DropReason: DropPolicyDenied, EventType: EventDrop,
		Direction: DirectionEgress, Source: appEP("shop", "worker"), Destination: world("93.184.216.34"),
		Protocol: "UDP", DstPort: 123, DestinationNames: []string{"pool.ntp.org"}}
	relay := &fakeRelay{flows: []*Flow{allowed, dropped}, lost: 9}
	addr := startRelay(t, relay)

	var got []*Flow
	var lost uint64
	connected := false
	err := NewRelay(addr).Flows(context.Background(), t0.Add(-time.Hour), Sink{
		Connected: func() { connected = true },
		Flow:      func(f *Flow) { got = append(got, f) },
		Lost:      func(n uint64) { lost += n },
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF when the stream ends", err)
	}
	if !connected || lost != 9 || len(got) != 2 {
		t.Fatalf("connected %v, lost %d, flows %d", connected, lost, len(got))
	}
	a, d := got[0], got[1]
	if !a.Time.Equal(t0) || !a.Allowed() || a.Source.Namespace != "shop" || a.Source.Label("kwerft.dev/app") != "web" ||
		a.Destination.Label("kwerft.dev/app") != "api" || a.Protocol != "TCP" || a.DstPort != 8080 ||
		len(a.AllowedBy) != 1 || a.AllowedBy[0] != (Policy{Name: "web-to-api.traffic-in", Namespace: "shop"}) ||
		a.Source.IP != "10.42.0.7" || a.Direction != DirectionIngress {
		t.Errorf("allowed flow = %+v", a)
	}
	if !d.PolicyDrop() || d.Destination.Reserved() != "world" || d.Destination.IP != "93.184.216.34" ||
		d.Protocol != "UDP" || d.DstPort != 123 || d.Direction != DirectionEgress || d.DestinationNames[0] != "pool.ntp.org" {
		t.Errorf("dropped flow = %+v", d)
	}

	// The request: follow, since, a filter for drops and policy verdicts.
	req := relay.requests[0]
	var follow bool
	var since time.Time
	var types []uint64
	_ = fields(req, func(num protowire.Number, typ protowire.Type, v uint64, bs []byte) error {
		switch num {
		case 3:
			follow = v == 1
		case 7:
			since, _ = decodeTimestamp(bs)
		case 6: // whitelist
			return fields(bs, func(num protowire.Number, _ protowire.Type, _ uint64, bs []byte) error {
				if num == 6 {
					return fields(bs, func(num protowire.Number, _ protowire.Type, v uint64, _ []byte) error {
						if num == 1 {
							types = append(types, v)
						}
						return nil
					})
				}
				return nil
			})
		}
		return nil
	})
	if !follow || !since.Equal(t0.Add(-time.Hour)) || len(types) != 2 || types[0] != 1 || types[1] != 5 {
		t.Errorf("request: follow %v since %v event types %v", follow, since, types)
	}
}

func TestRelayReportsGRPCErrors(t *testing.T) {
	addr := startRelay(t, &fakeRelay{status: "14"})
	err := NewRelay(addr).Flows(context.Background(), time.Time{}, Sink{})
	var ge *grpcError
	if !errors.As(err, &ge) || ge.code != "14" || ge.message != "relay says no" {
		t.Errorf("err = %v", err)
	}
	if err := NewRelay("127.0.0.1:1").Flows(context.Background(), time.Time{}, Sink{}); err == nil {
		t.Error("no relay: want an error")
	}
}

func TestAggregatorRunsAgainstTheRelay(t *testing.T) {
	now := time.Now()
	relay := &fakeRelay{flows: []*Flow{{Time: now.Add(-time.Minute), Verdict: VerdictForwarded, EventType: EventPolicyVerdict,
		Source: appEP("shop", "web"), Destination: appEP("shop", "api"), Protocol: "TCP", DstPort: 8080,
		AllowedBy: []Policy{{Name: "r.traffic-in", Namespace: "shop"}}}}}
	addr := startRelay(t, relay)
	agg := NewAggregator()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { agg.Run(ctx, NewRelay(addr)); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for agg.Allowed("shop", "r.traffic-in") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if n := agg.Allowed("shop", "r.traffic-in"); n == 0 {
		t.Fatal("no allowed connection counted")
	}
	// The stream ended (EOF): the next attempt resumes after the newest flow.
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if st := agg.Status(); st.State != "error" && st.State != "ok" {
		t.Errorf("state = %q", st.State)
	}
}

func TestAggregatorCountsAndWindow(t *testing.T) {
	now := t0
	agg := &Aggregator{Now: func() time.Time { return now }, state: "connecting"}
	if agg.RuleCounts(&kwerftv1.TrafficRule{}) != nil {
		t.Error("counts before any flow should be nil")
	}
	verdict := func(at time.Time, p Policy) *Flow {
		return &Flow{Time: at, Verdict: VerdictForwarded, EventType: EventPolicyVerdict, AllowedBy: []Policy{p},
			Source: appEP("shop", "web"), Destination: appEP("shop", "api"), Protocol: "TCP", DstPort: 8080}
	}
	in := Policy{Name: "web-to-api.traffic-in", Namespace: "shop"}
	agg.Add(verdict(now.Add(-90*time.Minute), in)) // outside the window
	agg.Add(verdict(now.Add(-50*time.Minute), in))
	agg.Add(verdict(now.Add(-time.Minute), in))
	agg.Add(verdict(now, in))
	reply := verdict(now, in)
	reply.Reply = true
	agg.Add(reply) // replies are not connections
	agg.Add(verdict(now, Policy{Name: "web-to-api.traffic-in", Namespace: "other"}))
	if n := agg.Allowed("shop", "web-to-api.traffic-in", "web-to-api.traffic-out"); n != 3 {
		t.Errorf("allowed = %d, want 3", n)
	}

	drop := func(at time.Time, from, to Endpoint, port uint32) *Flow {
		return &Flow{Time: at, Verdict: VerdictDropped, DropReason: DropPolicyDenied, EventType: EventDrop,
			Source: from, Destination: to, Protocol: "TCP", DstPort: port, Direction: DirectionIngress}
	}
	for range 3 {
		agg.Add(drop(now.Add(-2*time.Minute), appEP("shop", "web"), appEP("shop", "api"), 9090))
	}
	agg.Add(drop(now, appEP("shop", "web"), appEP("shop", "api"), 9090))
	agg.Add(drop(now, appEP("internal", "cron"), appEP("billing", "ledger"), 5432))
	// Not policy drops: ignored.
	agg.Add(&Flow{Time: now, Verdict: VerdictDropped, DropReason: 130, EventType: EventDrop, Source: appEP("shop", "web"), Destination: appEP("shop", "api")})

	rule := &kwerftv1.TrafficRule{ObjectMeta: metav1.ObjectMeta{Name: "web-to-api", Namespace: "shop"},
		Spec: kwerftv1.TrafficRuleSpec{From: []kwerftv1.TrafficPeer{{App: "web"}}, To: []kwerftv1.TrafficPeer{{App: "api"}},
			Ports: []kwerftv1.TrafficPort{{Port: 8080}}}}
	c := agg.RuleCounts(rule)
	if c == nil || c.Allowed != 3 || c.Dropped != 4 || !c.Since.Time.Equal(now.Add(-time.Hour)) {
		t.Errorf("counts = %+v", c)
	}

	// Scope: shop sees its drop, not internal → billing.
	drops := agg.Drops(Namespaces("shop"), nil, 10)
	if len(drops) != 1 || drops[0].Count != 4 || drops[0].To.App != "api" || drops[0].Port != 9090 ||
		!drops[0].First.Equal(now.Add(-2*time.Minute)) || !drops[0].Last.Equal(now) {
		t.Errorf("shop drops = %+v", drops)
	}
	if got := agg.Drops(Namespaces("billing"), nil, 10); len(got) != 1 || got[0].From.Namespace != "internal" {
		t.Errorf("billing drops = %+v", got)
	}
	if got := agg.Drops(Scope{}, nil, 10); len(got) != 0 {
		t.Errorf("empty scope sees %+v", got)
	}
	if got := agg.Drops(Unconfined(), nil, 1); len(got) != 1 || got[0].Count != 4 {
		t.Errorf("limit 1 = %+v", got)
	}

	// An hour later everything has aged out.
	now = now.Add(61 * time.Minute)
	if n := agg.Allowed("shop", "web-to-api.traffic-in"); n != 0 {
		t.Errorf("allowed after an hour = %d", n)
	}
	if got := agg.Drops(Unconfined(), nil, 0); len(got) != 0 {
		t.Errorf("drops after an hour = %+v", got)
	}
}

func TestAggregatorBoundsDistinctDrops(t *testing.T) {
	agg := &Aggregator{Now: func() time.Time { return t0 }}
	for i := range maxDropKeys + 10 {
		agg.Add(&Flow{Time: t0, Verdict: VerdictDropped, DropReason: DropPolicyDenied, EventType: EventDrop,
			Source: appEP("shop", "web"), Destination: world("1.2.3.4"), Protocol: "TCP", DstPort: uint32(i + 1), Direction: DirectionEgress})
	}
	if got := len(agg.Drops(Unconfined(), nil, 0)); got != maxDropKeys {
		t.Errorf("distinct drops = %d, want %d", got, maxDropKeys)
	}
}

func TestRuleCoversAndSuggest(t *testing.T) {
	rule := func(from, to kwerftv1.TrafficPeer) *kwerftv1.TrafficRule {
		return &kwerftv1.TrafficRule{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "shop"},
			Spec: kwerftv1.TrafficRuleSpec{From: []kwerftv1.TrafficPeer{from}, To: []kwerftv1.TrafficPeer{to}}}
	}
	pod := func(ns, app string) Side { return Side{Kind: "pod", Namespace: ns, App: app} }
	for _, tc := range []struct {
		name string
		rule *kwerftv1.TrafficRule
		key  DropKey
		want bool
	}{
		{"same project apps", rule(kwerftv1.TrafficPeer{App: "web"}, kwerftv1.TrafficPeer{App: "api"}), DropKey{From: pod("shop", "web"), To: pod("shop", "api")}, true},
		{"other source", rule(kwerftv1.TrafficPeer{App: "web"}, kwerftv1.TrafficPeer{App: "api"}), DropKey{From: pod("shop", "cron"), To: pod("shop", "api")}, false},
		{"cross project", rule(kwerftv1.TrafficPeer{App: "internal/cron"}, kwerftv1.TrafficPeer{Project: "shop"}), DropKey{From: pod("internal", "cron"), To: pod("shop", "db")}, true},
		{"internet in", rule(kwerftv1.TrafficPeer{Internet: true}, kwerftv1.TrafficPeer{App: "api"}), DropKey{From: Side{Kind: "remote-node"}, To: pod("shop", "api")}, true},
		{"internet out", rule(kwerftv1.TrafficPeer{App: "api"}, kwerftv1.TrafficPeer{Internet: true}), DropKey{From: pod("shop", "api"), To: Side{Kind: "world", IP: "93.184.216.34"}}, true},
		{"private is not internet", rule(kwerftv1.TrafficPeer{App: "api"}, kwerftv1.TrafficPeer{Internet: true}), DropKey{From: pod("shop", "api"), To: Side{Kind: "world", IP: "10.0.0.5"}}, false},
		{"cidr", rule(kwerftv1.TrafficPeer{App: "api"}, kwerftv1.TrafficPeer{CIDR: "10.0.0.0/16"}), DropKey{From: pod("shop", "api"), To: Side{Kind: "world", IP: "10.0.0.5"}}, true},
	} {
		if got := RuleCovers(tc.rule, tc.key); got != tc.want {
			t.Errorf("%s: covers = %v", tc.name, got)
		}
	}

	s := Suggest(DropKey{From: pod("shop", "billing-worker"), To: pod("shop", "api"), Port: 8080, Protocol: "TCP"})
	if s == nil || s.Project != "shop" || s.Name != "billing-worker-to-api" || s.Spec.From[0].App != "billing-worker" ||
		s.Spec.To[0].App != "api" || s.Spec.Ports[0] != (kwerftv1.TrafficPort{Port: 8080, Protocol: "TCP"}) {
		t.Errorf("same project = %+v", s)
	}
	s = Suggest(DropKey{From: pod("internal", "cron"), To: pod("shop", "api"), Port: 8080, Protocol: "TCP"})
	if s == nil || s.Project != "shop" || s.Spec.From[0].App != "internal/cron" || s.Name != "internal-cron-to-api" {
		t.Errorf("cross project = %+v", s)
	}
	s = Suggest(DropKey{From: pod("shop", "worker"), To: Side{Kind: "world", IP: "93.184.216.34"}, Port: 80, Protocol: "TCP", Egress: true})
	if s == nil || s.Project != "shop" || !s.Spec.To[0].Internet || s.Name != "worker-to-internet" {
		t.Errorf("internet = %+v", s)
	}
	s = Suggest(DropKey{From: pod("shop", "worker"), To: Side{Kind: "world", IP: "10.0.1.2"}, Port: 5432, Protocol: "TCP", Egress: true})
	if s == nil || s.Spec.To[0].CIDR != "10.0.1.2/32" {
		t.Errorf("private address = %+v", s)
	}
	if s := Suggest(DropKey{From: Side{Kind: "kube-apiserver"}, To: pod("shop", "api")}); s != nil {
		t.Errorf("api server = %+v, want none", s)
	}
	if s := Suggest(DropKey{From: pod("shop", "api"), To: pod("kwerft-system", "")}); s != nil {
		t.Errorf("platform pod = %+v, want none", s)
	}
}

// The request's field numbers, checked against cilium/api/v1
// observer/observer.proto and flow/flow.proto at v1.20.2: GetFlowsRequest
// follow 3, whitelist 6, since 7; FlowFilter event_type 6; EventTypeFilter
// type 1 (monitor types: drop 1, policy verdict 5). A wrong whitelist
// number makes the relay ignore the filter and stream every flow.
func TestGetFlowsRequestFieldNumbers(t *testing.T) {
	seen := map[protowire.Number]bool{}
	var types []uint64
	err := fields(encodeGetFlowsRequest(t0), func(num protowire.Number, typ protowire.Type, _ uint64, bs []byte) error {
		seen[num] = true
		if num != 6 {
			return nil
		}
		return fields(bs, func(num protowire.Number, _ protowire.Type, _ uint64, bs []byte) error {
			if num != 6 {
				t.Errorf("FlowFilter field %d, want event_type 6", num)
				return nil
			}
			return fields(bs, func(num protowire.Number, _ protowire.Type, v uint64, _ []byte) error {
				if num == 1 {
					types = append(types, v)
				}
				return nil
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seen[3] || !seen[6] || !seen[7] || seen[4] {
		t.Errorf("request fields = %v, want follow 3, whitelist 6, since 7", seen)
	}
	if !slices.Equal(types, []uint64{1, 5}) {
		t.Errorf("event types = %v, want drop 1 and policy verdict 5", types)
	}
}
