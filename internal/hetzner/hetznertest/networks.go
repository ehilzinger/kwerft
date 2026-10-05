package hetznertest

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Networks: /networks with add_subnet. Servers count as attached when the
// server list (AddServer) has them in the network.

type networkState struct {
	next     int64
	networks map[int64]*hetzner.Network
}

// EnableNetworks serves /networks.
func (s *Server) EnableNetworks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.networkState()
}

func (s *Server) networkState() *networkState {
	if s.State == nil {
		s.State = map[string]any{}
	}
	st, ok := s.State["networks"].(*networkState)
	if !ok {
		st = &networkState{next: 500, networks: map[int64]*hetzner.Network{}}
		s.State["networks"] = st
		if s.handlers == nil {
			s.handlers = map[string]Handler{}
		}
		s.handlers["networks"] = serveNetworks
		s.actionState()
	}
	return st
}

// PutNetwork stores a network (its ID is kept when set) and returns the ID.
func (s *Server) PutNetwork(n hetzner.Network) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.networkState()
	if n.ID == 0 {
		st.next++
		n.ID = st.next
	}
	cp := n
	st.networks[n.ID] = &cp
	return n.ID
}

// Networks returns the fake's networks by ID.
func (s *Server) Networks() []hetzner.Network {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []hetzner.Network
	for _, n := range s.networkState().networks {
		out = append(out, s.networkJSON(n))
	}
	slices.SortFunc(out, func(a, b hetzner.Network) int { return int(a.ID - b.ID) })
	return out
}

func (s *Server) networkJSON(n *hetzner.Network) hetzner.Network {
	out := *n
	out.Subnets = slices.Clone(n.Subnets)
	if out.Subnets == nil {
		out.Subnets = []hetzner.Subnet{}
	}
	out.Routes = []hetzner.Route{}
	out.Servers = []int64{}
	for _, srv := range s.serverList() {
		for _, p := range srv.PrivateNet {
			if p.Network == n.ID {
				out.Servers = append(out.Servers, srv.ID)
			}
		}
	}
	out.LoadBalancers = []int64{}
	if lbs, ok := s.State["loadBalancers"].(*lbState); ok {
		for _, lb := range lbs.lbs {
			if lb.network == n.ID {
				out.LoadBalancers = append(out.LoadBalancers, lb.id)
			}
		}
	}
	return out
}

func serveNetworks(s *Server, w http.ResponseWriter, r *http.Request, parts []string) {
	if !s.writable(w, r) {
		return
	}
	st := s.networkState()
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			out := []hetzner.Network{}
			for _, n := range st.networks {
				if sel := r.URL.Query().Get("label_selector"); sel != "" && !cloudSelectorMatch(n.Labels, sel) {
					continue
				}
				if name := r.URL.Query().Get("name"); name != "" && name != n.Name {
					continue
				}
				out = append(out, s.networkJSON(n))
			}
			slices.SortFunc(out, func(a, b hetzner.Network) int { return int(a.ID - b.ID) })
			writeJSON(w, http.StatusOK, map[string]any{"networks": out, "meta": noMorePages()})
		case http.MethodPost:
			var req hetzner.NetworkOpts
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "name is required")
				return
			}
			if p, err := netip.ParsePrefix(req.IPRange); err != nil || p.Masked() != p || !p.Addr().IsPrivate() {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "ip_range must be a private network")
				return
			}
			st.next++
			n := &hetzner.Network{ID: st.next, Name: req.Name, IPRange: req.IPRange, Labels: req.Labels,
				Subnets: req.Subnets, ExposeRoutesToVSwitch: req.ExposeRoutesToVSwitch}
			st.networks[n.ID] = n
			writeJSON(w, http.StatusCreated, map[string]any{"network": s.networkJSON(n)})
		default:
			writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		}
		return
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	n, ok := st.networks[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "network not found")
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"network": s.networkJSON(n)})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		if v := s.networkJSON(n); len(v.Servers)+len(v.LoadBalancers) > 0 {
			writeErr(w, http.StatusConflict, "conflict", "network still has attached resources")
			return
		}
		delete(st.networks, id)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 4 && parts[3] == "add_subnet" && r.Method == http.MethodPost:
		var sub hetzner.Subnet
		if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.NetworkZone == "" {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "network_zone is required")
			return
		}
		n.Subnets = append(n.Subnets, sub)
		writeJSON(w, http.StatusCreated, map[string]any{"action": s.NewAction("add_subnet")})
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}
