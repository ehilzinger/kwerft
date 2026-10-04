package hubble

import (
	"cmp"
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Window is how far back counts and drops go.
const Window = time.Hour

const (
	// maxDropKeys bounds the distinct drops one minute keeps; more are
	// counted as "other" only.
	maxDropKeys = 2000
	// minutes in the ring: the window plus the minute in progress.
	ringSize = 61
)

// Scope is what a caller may see: flows with either side in one of its
// namespaces. The zero value sees nothing. (The console builds it from the
// projects the user reaches; see internal/server.)
type Scope struct {
	all        bool
	namespaces map[string]bool
}

// Unconfined sees every namespace.
func Unconfined() Scope { return Scope{all: true} }

// Namespaces sees only these namespaces.
func Namespaces(ns ...string) Scope {
	m := make(map[string]bool, len(ns))
	for _, n := range ns {
		m[n] = true
	}
	return Scope{namespaces: m}
}

// Has reports whether namespace is in the scope.
func (s Scope) Has(namespace string) bool {
	return namespace != "" && (s.all || s.namespaces[namespace])
}

// Side is one end of a dropped connection as the console shows it: an app
// (or another pod) of a namespace, or something outside the pods: "world"
// (with its address), "host", "remote-node", "kube-apiserver", ...
type Side struct {
	Namespace string `json:"namespace,omitempty"`
	App       string `json:"app,omitempty"`
	// Kind is "pod" or Cilium's reserved identity.
	Kind string `json:"kind"`
	// IP is set for addresses outside the cluster.
	IP string `json:"ip,omitempty"`
	// Name is a DNS name Cilium saw for the address, if any.
	Name string `json:"name,omitempty"`
}

func side(e Endpoint, names []string) Side {
	if r := e.Reserved(); r != "" {
		s := Side{Kind: r}
		if r == "world" {
			s.IP = e.IP
			if len(names) > 0 {
				s.Name = names[0]
			}
		}
		return s
	}
	app := e.Label("kwerft.dev/app")
	if app == "" {
		app = e.Label("kwerft.dev/as-app")
	}
	return Side{Namespace: e.Namespace, App: app, Kind: "pod"}
}

// DropKey identifies dropped connections that look alike.
type DropKey struct {
	From     Side
	To       Side
	Port     uint32
	Protocol string
	// Egress: dropped on the way out (the source's policy), else on the
	// way in (the destination's).
	Egress bool
}

// Drop is DropKey with what the window saw of it.
type Drop struct {
	DropKey
	Count int64
	First time.Time
	Last  time.Time
}

type dropStats struct {
	count       int64
	first, last time.Time
}

type bucket struct {
	minute  int64
	allowed map[Policy]int64
	drops   map[DropKey]*dropStats
	other   int64 // drops beyond maxDropKeys
}

// Status says whether flows arrive.
type Status struct {
	// State: off (no relay configured), connecting, ok or error.
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since,omitzero"` // counts cover Since to now
	Lost    int64     `json:"lost,omitempty"` // flows the relay reported lost in the window's lifetime
}

// Aggregator keeps an hour of policy verdicts and drops, by minute.
type Aggregator struct {
	Now    func() time.Time
	Logger *slog.Logger

	mu      sync.Mutex
	ring    [ringSize]bucket
	state   string
	message string
	covered time.Time // earliest flow received since the first connection
	last    time.Time // newest flow received (where to resume)
	lost    int64
}

// NewAggregator returns an empty aggregator; Run feeds it.
func NewAggregator() *Aggregator {
	return &Aggregator{state: "connecting"}
}

func (a *Aggregator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Run reads flows from src until ctx ends, reconnecting with backoff. The
// first connection asks for the last hour the relay still has; later ones
// resume after the newest flow seen.
func (a *Aggregator) Run(ctx context.Context, src Source) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		a.mu.Lock()
		since := a.last
		if since.IsZero() {
			since = a.now().Add(-Window)
		} else {
			since = since.Add(time.Nanosecond)
		}
		a.mu.Unlock()

		started := time.Now()
		err := src.Flows(ctx, since, Sink{Connected: a.connected, Flow: a.Add, Lost: a.addLost})
		if ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		a.state = "error"
		a.message = "Hubble relay: " + err.Error()
		a.mu.Unlock()
		if a.Logger != nil {
			a.Logger.Warn("hubble flows interrupted", "err", err)
		}
		if time.Since(started) > time.Minute {
			backoff = 2 * time.Second // it worked for a while
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// Add records one flow; Run calls it, tests may too.
func (a *Aggregator) Add(f *Flow) {
	drop, allowed := f.PolicyDrop(), f.Allowed() && len(f.AllowedBy) > 0
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state, a.message = "ok", ""
	t := f.Time
	if t.IsZero() {
		t = a.now()
	}
	if t.After(a.last) {
		a.last = t
	}
	if a.covered.IsZero() || t.Before(a.covered) {
		a.covered = t
	}
	if !drop && !allowed {
		return
	}
	if a.now().Sub(t) > Window {
		return
	}
	b := a.bucket(t)
	if allowed {
		for _, p := range f.AllowedBy {
			b.allowed[p]++
		}
		return
	}
	k := DropKey{
		From:     side(f.Source, nil),
		To:       side(f.Destination, f.DestinationNames),
		Port:     f.DstPort,
		Protocol: f.Protocol,
		Egress:   f.Direction == DirectionEgress,
	}
	s := b.drops[k]
	if s == nil {
		if len(b.drops) >= maxDropKeys {
			b.other++
			return
		}
		s = &dropStats{first: t}
		b.drops[k] = s
	}
	s.count++
	if t.Before(s.first) {
		s.first = t
	}
	if t.After(s.last) {
		s.last = t
	}
}

func (a *Aggregator) connected() {
	a.mu.Lock()
	a.state, a.message = "ok", ""
	if a.covered.IsZero() {
		// Nothing older arrived: the counts start now.
		a.covered = a.now()
	}
	a.mu.Unlock()
}

func (a *Aggregator) addLost(n uint64) {
	a.mu.Lock()
	a.lost += int64(n)
	a.mu.Unlock()
}

// bucket returns t's minute, emptied if it held an older minute. Callers
// hold the lock.
func (a *Aggregator) bucket(t time.Time) *bucket {
	m := t.Unix() / 60
	b := &a.ring[m%ringSize]
	if b.minute != m || b.allowed == nil {
		*b = bucket{minute: m, allowed: map[Policy]int64{}, drops: map[DropKey]*dropStats{}}
	}
	return b
}

// each calls fn for every bucket inside the window. Callers hold the lock.
func (a *Aggregator) each(fn func(*bucket)) {
	oldest := a.now().Add(-Window).Unix() / 60
	for i := range a.ring {
		if b := &a.ring[i]; b.allowed != nil && b.minute >= oldest {
			fn(b)
		}
	}
}

// Status reports the connection and what the counts cover.
func (a *Aggregator) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Status{State: a.state, Message: a.message, Lost: a.lost}
	if !a.covered.IsZero() {
		s.Since = maxTime(a.covered, a.now().Add(-Window))
	}
	return s
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Allowed sums the connections the named policies of namespace allowed in
// the window.
func (a *Aggregator) Allowed(namespace string, policies ...string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int64
	a.each(func(b *bucket) {
		for _, name := range policies {
			n += b.allowed[Policy{Name: name, Namespace: namespace}]
		}
	})
	return n
}

// Drops lists the window's drops that match (nil: all) and that scope may
// see (either side in it), most frequent first, at most limit.
func (a *Aggregator) Drops(scope Scope, match func(DropKey) bool, limit int) []Drop {
	a.mu.Lock()
	sum := map[DropKey]*Drop{}
	a.each(func(b *bucket) {
		for k, s := range b.drops {
			if !scope.Has(k.From.Namespace) && !scope.Has(k.To.Namespace) {
				continue
			}
			if match != nil && !match(k) {
				continue
			}
			d := sum[k]
			if d == nil {
				d = &Drop{DropKey: k, First: s.first, Last: s.last}
				sum[k] = d
			}
			d.Count += s.count
			if s.first.Before(d.First) {
				d.First = s.first
			}
			if s.last.After(d.Last) {
				d.Last = s.last
			}
		}
	})
	a.mu.Unlock()
	out := make([]Drop, 0, len(sum))
	for _, d := range sum {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(x, y Drop) int {
		if c := cmp.Compare(y.Count, x.Count); c != 0 {
			return c
		}
		return y.Last.Compare(x.Last)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// inCIDR reports whether ip is inside cidr.
func inCIDR(ip, cidr string) bool {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(ip)
	return err == nil && p.Contains(a)
}
