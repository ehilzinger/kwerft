package hetznertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Load Balancers: /load_balancers with services, targets (servers and label
// selectors, resolved against the server list) and networks. Every target
// is healthy unless SetTargetHealth says otherwise.

type fakeLB struct {
	id       int64
	name     string
	typ      string
	location string
	labels   map[string]string
	network  int64
	services []hetzner.LBService
	targets  []hetzner.LBTarget
}

type lbState struct {
	next   int64
	lbs    map[int64]*fakeLB
	health map[int64]string // by server ID
}

// EnableLoadBalancers serves /load_balancers.
func (s *Server) EnableLoadBalancers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lbState()
}

func (s *Server) lbState() *lbState {
	if s.State == nil {
		s.State = map[string]any{}
	}
	st, ok := s.State["loadBalancers"].(*lbState)
	if !ok {
		st = &lbState{next: 700, lbs: map[int64]*fakeLB{}, health: map[int64]string{}}
		s.State["loadBalancers"] = st
		if s.handlers == nil {
			s.handlers = map[string]Handler{}
		}
		s.handlers["load_balancers"] = serveLoadBalancers
		s.actionState()
	}
	return st
}

// SetTargetHealth sets a server's health on every Load Balancer.
func (s *Server) SetTargetHealth(server int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lbState().health[server] = status
}

// LoadBalancers returns the fake's Load Balancers as the API shows them.
func (s *Server) LoadBalancers() []hetzner.LoadBalancer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []hetzner.LoadBalancer
	for _, lb := range s.lbState().lbs {
		out = append(out, s.lbJSON(lb))
	}
	slices.SortFunc(out, func(a, b hetzner.LoadBalancer) int { return int(a.ID - b.ID) })
	return out
}

func (s *Server) targetHealth(id int64, services []hetzner.LBService) []hetzner.LBHealth {
	status := s.lbState().health[id]
	if status == "" {
		status = "healthy"
	}
	out := []hetzner.LBHealth{}
	for _, svc := range services {
		out = append(out, hetzner.LBHealth{ListenPort: svc.ListenPort, Status: status})
	}
	return out
}

func (s *Server) lbJSON(lb *fakeLB) hetzner.LoadBalancer {
	raw, _ := json.Marshal(map[string]any{
		"id": lb.id, "name": lb.name, "labels": lb.labels,
		"public_net": map[string]any{"enabled": true,
			"ipv4": map[string]string{"ip": fmt.Sprintf("198.51.100.%d", lb.id%250)},
			"ipv6": map[string]string{"ip": fmt.Sprintf("2001:db8:%x::1", lb.id)}},
		"location":           map[string]string{"name": lb.location, "network_zone": "eu-central"},
		"load_balancer_type": map[string]string{"name": lb.typ},
		"private_net": func() []map[string]any {
			if lb.network == 0 {
				return []map[string]any{}
			}
			return []map[string]any{{"network": lb.network, "ip": fmt.Sprintf("10.0.255.%d", lb.id%250)}}
		}(),
	})
	var out hetzner.LoadBalancer
	_ = json.Unmarshal(raw, &out)
	out.Services = slices.Clone(lb.services)
	if out.Services == nil {
		out.Services = []hetzner.LBService{}
	}
	out.Targets = []hetzner.LBTarget{}
	for _, t := range lb.targets {
		t.HealthStatus, t.Targets = nil, nil
		switch {
		case t.Server != nil:
			t.HealthStatus = s.targetHealth(t.Server.ID, lb.services)
		case t.LabelSelector != nil:
			t.Targets = []hetzner.LBTarget{}
			for _, srv := range s.serversMatching(t.LabelSelector.Selector) {
				sub := hetzner.LBTarget{Type: "server", Server: &hetzner.ResourceRef{ID: srv.ID}, UsePrivateIP: t.UsePrivateIP,
					HealthStatus: s.targetHealth(srv.ID, lb.services)}
				t.Targets = append(t.Targets, sub)
			}
		}
		out.Targets = append(out.Targets, t)
	}
	return out
}

func (s *Server) checkTarget(lb *fakeLB, t hetzner.LBTarget) (string, string) {
	switch {
	case t.Type == "server" && t.Server != nil:
		if !s.serverExists(t.Server.ID) {
			return "not_found", "server not found"
		}
		if t.UsePrivateIP {
			in := false
			for _, srv := range s.serverList() {
				if srv.ID == t.Server.ID {
					in = slices.ContainsFunc(srv.PrivateNet, func(p hetzner.PrivateNetRef) bool {
						return p.Network == lb.network && lb.network != 0
					})
				}
			}
			if !in {
				return "target_server_not_attached_to_network", "the server is not in the Load Balancer's network"
			}
		}
	case t.Type == "label_selector" && t.LabelSelector != nil && t.LabelSelector.Selector != "":
		if t.UsePrivateIP && lb.network == 0 {
			return "load_balancer_not_attached_to_network", "the Load Balancer is not in a network"
		}
	default:
		return "invalid_input", "invalid target"
	}
	if slices.ContainsFunc(lb.targets, func(o hetzner.LBTarget) bool { return o.Key() == t.Key() }) {
		return "target_already_defined", "target is already defined"
	}
	return "", ""
}

func validService(svc hetzner.LBService) bool {
	return (svc.Protocol == "tcp" || svc.Protocol == "http" || svc.Protocol == "https") &&
		svc.ListenPort > 0 && svc.DestinationPort > 0 && svc.HealthCheck.Port > 0 &&
		svc.HealthCheck.Interval >= 3 && svc.HealthCheck.Timeout >= 1 && svc.HealthCheck.Retries >= 1
}

func serveLoadBalancers(s *Server, w http.ResponseWriter, r *http.Request, parts []string) {
	if !s.writable(w, r) {
		return
	}
	st := s.lbState()
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			out := []hetzner.LoadBalancer{}
			for _, lb := range st.lbs {
				if sel := r.URL.Query().Get("label_selector"); sel != "" && !cloudSelectorMatch(lb.labels, sel) {
					continue
				}
				out = append(out, s.lbJSON(lb))
			}
			slices.SortFunc(out, func(a, b hetzner.LoadBalancer) int { return int(a.ID - b.ID) })
			writeJSON(w, http.StatusOK, map[string]any{"load_balancers": out, "meta": noMorePages()})
		case http.MethodPost:
			var req hetzner.LoadBalancerOpts
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.LoadBalancerType == "" ||
				(req.Location == "" && req.NetworkZone == "") {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "name, load_balancer_type and location are required")
				return
			}
			for _, lb := range st.lbs {
				if lb.name == req.Name {
					writeErr(w, http.StatusConflict, "uniqueness_error", "name is already used")
					return
				}
			}
			if req.Network != 0 {
				if ns, ok := s.State["networks"].(*networkState); !ok || ns.networks[req.Network] == nil {
					writeErr(w, http.StatusNotFound, "not_found", "network not found")
					return
				}
			}
			for _, svc := range req.Services {
				if !validService(svc) {
					writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "invalid service")
					return
				}
			}
			st.next++
			lb := &fakeLB{id: st.next, name: req.Name, typ: req.LoadBalancerType, location: req.Location, labels: req.Labels,
				network: req.Network, services: req.Services}
			for _, t := range req.Targets {
				if code, msg := s.checkTarget(lb, t); code != "" {
					writeErr(w, http.StatusUnprocessableEntity, code, msg)
					return
				}
				lb.targets = append(lb.targets, t)
			}
			st.lbs[lb.id] = lb
			writeJSON(w, http.StatusCreated, map[string]any{"load_balancer": s.lbJSON(lb), "action": s.NewAction("create_load_balancer")})
		default:
			writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		}
		return
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	lb, ok := st.lbs[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "load balancer not found")
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"load_balancer": s.lbJSON(lb)})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		delete(st.lbs, id)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 4 && parts[2] == "actions" && r.Method == http.MethodPost:
		s.lbAction(w, r, lb, parts[3])
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func (s *Server) lbAction(w http.ResponseWriter, r *http.Request, lb *fakeLB, action string) {
	raw := json.RawMessage{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeErr(w, http.StatusBadRequest, "json_error", "invalid JSON")
		return
	}
	switch action {
	case "add_service", "update_service":
		var svc hetzner.LBService
		_ = json.Unmarshal(raw, &svc)
		i := slices.IndexFunc(lb.services, func(o hetzner.LBService) bool { return o.ListenPort == svc.ListenPort })
		switch {
		case !validService(svc):
			writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "invalid service")
			return
		case action == "add_service" && i >= 0:
			writeErr(w, http.StatusUnprocessableEntity, "source_port_already_used", "listen port already used")
			return
		case action == "update_service" && i < 0:
			writeErr(w, http.StatusNotFound, "not_found", "no service on that port")
			return
		case i >= 0:
			lb.services[i] = svc
		default:
			lb.services = append(lb.services, svc)
		}
	case "add_target":
		var t hetzner.LBTarget
		_ = json.Unmarshal(raw, &t)
		if code, msg := s.checkTarget(lb, t); code != "" {
			writeErr(w, http.StatusUnprocessableEntity, code, msg)
			return
		}
		lb.targets = append(lb.targets, t)
	case "remove_target":
		var t hetzner.LBTarget
		_ = json.Unmarshal(raw, &t)
		i := slices.IndexFunc(lb.targets, func(o hetzner.LBTarget) bool { return o.Key() == t.Key() })
		if i < 0 {
			writeErr(w, http.StatusNotFound, "load_balancer_target_not_found", "no such target")
			return
		}
		lb.targets = slices.Delete(lb.targets, i, i+1)
	case "attach_to_network":
		var req struct {
			Network int64 `json:"network"`
		}
		_ = json.Unmarshal(raw, &req)
		if ns, ok := s.State["networks"].(*networkState); !ok || ns.networks[req.Network] == nil {
			writeErr(w, http.StatusNotFound, "not_found", "network not found")
			return
		}
		if lb.network != 0 {
			writeErr(w, http.StatusUnprocessableEntity, "load_balancer_already_attached", "already attached to a network")
			return
		}
		lb.network = req.Network
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such action")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"action": s.NewAction(action)})
}
