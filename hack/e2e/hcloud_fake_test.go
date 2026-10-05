package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCloud is an in-memory Hetzner Cloud API: servers, SSH keys, server
// types and images, as much as the harness uses. Kept apart from
// internal/hetzner/hetznertest, which fakes the product's own client.
type fakeCloud struct {
	*httptest.Server
	t     *testing.T
	token string

	mu       sync.Mutex
	nextID   int64
	servers  map[int64]*hServer
	keys     map[int64]*hSSHKey
	networks map[int64]*hNetwork
	// attached: the networks each server was created in.
	attached map[int64][]int64
	// nextIP: the last octet of the next server's address (203.0.113.x).
	nextIP int
	// onDelete is called (without the lock) when a server is deleted.
	onDelete func(ip string)
	types    map[string]hServerType
	images   map[string]bool
	// unavailable "type/location" combinations answer resource_unavailable.
	unavailable map[string]bool
	// userData of each created server, by id.
	userData map[int64]string
	// failDeletes makes server deletions fail with 500.
	failDeletes bool
	// pollsUntilGone: GETs of a deleted server that still find it.
	pollsUntilGone int
	gone           map[int64]int
	requests       []string
}

func newFakeCloud(t *testing.T) *fakeCloud {
	f := &fakeCloud{
		t: t, token: "test-token", nextID: 100,
		servers: map[int64]*hServer{}, keys: map[int64]*hSSHKey{}, networks: map[int64]*hNetwork{},
		attached: map[int64][]int64{}, nextIP: 10,
		types:       map[string]hServerType{},
		images:      map[string]bool{"ubuntu-26.04": true, "ubuntu-24.04": true},
		unavailable: map[string]bool{}, userData: map[int64]string{}, gone: map[int64]int{},
	}
	for _, st := range []struct {
		name  string
		cores int
		mem   float64
		price string
	}{{"cx23", 2, 4, "0.0088"}, {"cx33", 4, 8, "0.0136"}, {"cx43", 8, 16, "0.0256"}} {
		typ := hServerType{ID: int64(len(f.types) + 1), Name: st.name, Cores: st.cores, Memory: st.mem, Disk: 80}
		for _, loc := range []string{"nbg1", "fsn1", "hel1"} {
			p := hPrice{Location: loc}
			p.PriceHourly.Gross = st.price
			typ.Prices = append(typ.Prices, p)
		}
		f.types[st.name] = typ
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCloud) client() *hcloud {
	c := newHcloud(f.token, f.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func writeJSONTest(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSONTest(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// matches implements equality label selectors: "a=b,c=d".
func matches(selector string, labels map[string]string) bool {
	if selector == "" {
		return true
	}
	for _, term := range strings.Split(selector, ",") {
		k, v, _ := strings.Cut(term, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		apiErr(w, 401, "unauthorized", "unable to authenticate")
		return
	}
	f.mu.Lock()
	var after func() // runs once the lock is released
	defer func() {
		f.mu.Unlock()
		if after != nil {
			after()
		}
	}()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && parts[0] == "server_types":
		var out []hServerType
		if st, ok := f.types[q.Get("name")]; ok {
			out = append(out, st)
		}
		writeJSONTest(w, 200, map[string]any{"server_types": out})
	case r.Method == "GET" && parts[0] == "images":
		out := []hImage{}
		if f.images[q.Get("name")] && q.Get("type") == "system" {
			out = append(out, hImage{ID: 7, Name: q.Get("name")})
		}
		writeJSONTest(w, 200, map[string]any{"images": out})
	case parts[0] == "ssh_keys" && len(parts) == 1 && r.Method == "POST":
		var in struct {
			Name      string            `json:"name"`
			PublicKey string            `json:"public_key"`
			Labels    map[string]string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if !strings.HasPrefix(in.PublicKey, "ssh-ed25519 ") {
			apiErr(w, 400, "invalid_input", "bad key")
			return
		}
		f.nextID++
		k := &hSSHKey{ID: f.nextID, Name: in.Name, Labels: in.Labels, Created: time.Now()}
		f.keys[k.ID] = k
		writeJSONTest(w, 201, map[string]any{"ssh_key": k})
	case parts[0] == "ssh_keys" && len(parts) == 1 && r.Method == "GET":
		out := []hSSHKey{}
		for _, k := range f.keys {
			if matches(q.Get("label_selector"), k.Labels) {
				out = append(out, *k)
			}
		}
		writeJSONTest(w, 200, map[string]any{"ssh_keys": out, "meta": map[string]any{"pagination": map[string]any{"next_page": nil}}})
	case parts[0] == "ssh_keys" && len(parts) == 2 && r.Method == "DELETE":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		if _, ok := f.keys[id]; !ok {
			apiErr(w, 404, "not_found", "no key")
			return
		}
		delete(f.keys, id)
		w.WriteHeader(204)
	case parts[0] == "servers" && len(parts) == 1 && r.Method == "POST":
		var in createServerRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if f.unavailable[in.ServerType+"/"+in.Location] {
			apiErr(w, 412, "resource_unavailable", "server type not available here")
			return
		}
		if _, ok := f.types[in.ServerType]; !ok || !f.images[in.Image] || len(in.SSHKeys) == 0 {
			apiErr(w, 400, "invalid_input", "invalid input")
			return
		}
		for _, id := range in.SSHKeys {
			if _, ok := f.keys[id]; !ok {
				apiErr(w, 400, "invalid_input", "no such SSH key")
				return
			}
		}
		for _, s := range f.servers {
			if s.Name == in.Name {
				apiErr(w, 409, "uniqueness_error", "server name is already used")
				return
			}
		}
		for _, id := range in.Networks {
			if _, ok := f.networks[id]; !ok {
				apiErr(w, 400, "invalid_input", "no such network")
				return
			}
		}
		f.nextID++
		s := &hServer{ID: f.nextID, Name: in.Name, Status: "initializing", Labels: in.Labels, Created: time.Now()}
		s.PublicNet.IPv4.IP = "203.0.113." + strconv.Itoa(f.nextIP)
		f.nextIP++
		f.servers[s.ID] = s
		f.attached[s.ID] = in.Networks
		f.userData[s.ID] = in.UserData
		writeJSONTest(w, 201, map[string]any{"server": s, "action": map[string]any{"id": 1}})
	case parts[0] == "servers" && len(parts) == 1 && r.Method == "GET":
		out := []hServer{}
		for _, s := range f.servers {
			if matches(q.Get("label_selector"), s.Labels) {
				out = append(out, *s)
			}
		}
		writeJSONTest(w, 200, map[string]any{"servers": out, "meta": map[string]any{"pagination": map[string]any{"next_page": nil}}})
	case parts[0] == "servers" && len(parts) == 2:
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		s, ok := f.servers[id]
		if !ok {
			apiErr(w, 404, "not_found", "no server")
			return
		}
		switch r.Method {
		case "GET":
			if n, deleting := f.gone[id]; deleting {
				if n <= 0 {
					delete(f.servers, id)
					apiErr(w, 404, "not_found", "no server")
					return
				}
				f.gone[id] = n - 1
				s.Status = "deleting"
			} else {
				s.Status = "running"
			}
			writeJSONTest(w, 200, map[string]any{"server": s})
		case "DELETE":
			if f.failDeletes {
				apiErr(w, 500, "server_error", "try again")
				return
			}
			f.gone[id] = f.pollsUntilGone
			if f.pollsUntilGone == 0 {
				delete(f.servers, id)
			}
			delete(f.attached, id)
			if hook := f.onDelete; hook != nil {
				ip := s.PublicNet.IPv4.IP
				after = func() { hook(ip) }
			}
			writeJSONTest(w, 200, map[string]any{"action": map[string]any{"id": 2}})
		}
	case parts[0] == "networks" && len(parts) == 1 && r.Method == "POST":
		var in struct {
			Name    string            `json:"name"`
			IPRange string            `json:"ip_range"`
			Labels  map[string]string `json:"labels"`
			Subnets []struct {
				Type        string `json:"type"`
				IPRange     string `json:"ip_range"`
				NetworkZone string `json:"network_zone"`
			} `json:"subnets"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.IPRange == "" || len(in.Subnets) != 1 || in.Subnets[0].NetworkZone == "" {
			apiErr(w, 400, "invalid_input", "a network needs an ip_range and a subnet")
			return
		}
		f.nextID++
		n := &hNetwork{ID: f.nextID, Name: in.Name, IPRange: in.IPRange, Labels: in.Labels, Created: time.Now()}
		f.networks[n.ID] = n
		writeJSONTest(w, 201, map[string]any{"network": n})
	case parts[0] == "networks" && len(parts) == 1 && r.Method == "GET":
		out := []hNetwork{}
		for _, n := range f.networks {
			if matches(q.Get("label_selector"), n.Labels) {
				out = append(out, *n)
			}
		}
		writeJSONTest(w, 200, map[string]any{"networks": out, "meta": map[string]any{"pagination": map[string]any{"next_page": nil}}})
	case parts[0] == "networks" && len(parts) == 2 && r.Method == "DELETE":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		if _, ok := f.networks[id]; !ok {
			apiErr(w, 404, "not_found", "no network")
			return
		}
		for _, nets := range f.attached {
			if slices.Contains(nets, id) {
				apiErr(w, 409, "conflict", "the network is still in use")
				return
			}
		}
		delete(f.networks, id)
		writeJSONTest(w, 200, map[string]any{"action": map[string]any{"id": 3}})
	default:
		apiErr(w, 404, "not_found", "no route "+r.Method+" "+r.URL.Path)
	}
}

func (f *fakeCloud) counts() (servers, keys int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.servers), len(f.keys)
}

func (f *fakeCloud) networkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.networks)
}

// serverByIP is the name of the server with that address, "" if none.
func (f *fakeCloud) serverByIP(ip string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		if s.PublicNet.IPv4.IP == ip {
			return s.Name
		}
	}
	return ""
}
