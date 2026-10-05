package hetznertest

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Cloud Firewalls: /firewalls with set_rules, apply_to_resources and
// remove_from_resources, checked the way the Cloud API checks them (rule
// shape and limits, resources that exist, no deleting a firewall in use).

type fakeFirewall struct {
	id        int64
	name      string
	labels    map[string]string
	rules     []hetzner.FirewallRule
	appliedTo []hetzner.FirewallResource // server or label_selector, without applied_to_resources
}

type firewallState struct {
	next      int64
	firewalls map[int64]*fakeFirewall
}

// EnableFirewalls serves /firewalls (and the actions they start).
func (s *Server) EnableFirewalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.firewallState()
}

func (s *Server) firewallState() *firewallState {
	if s.State == nil {
		s.State = map[string]any{}
	}
	st, ok := s.State["firewalls"].(*firewallState)
	if !ok {
		st = &firewallState{next: 100, firewalls: map[int64]*fakeFirewall{}}
		s.State["firewalls"] = st
		if s.handlers == nil {
			s.handlers = map[string]Handler{}
		}
		s.handlers["firewalls"] = serveFirewalls
		s.actionState()
	}
	return st
}

// Firewalls returns the fake's firewalls as the API shows them, by ID.
func (s *Server) Firewalls() []hetzner.Firewall {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.firewallState()
	var out []hetzner.Firewall
	for _, f := range st.firewalls {
		out = append(out, s.firewallJSON(f))
	}
	slices.SortFunc(out, func(a, b hetzner.Firewall) int { return int(a.ID - b.ID) })
	return out
}

// PutFirewall stores a firewall as if someone created it in the Hetzner
// Console; it returns its ID.
func (s *Server) PutFirewall(f hetzner.Firewall) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.firewallState()
	st.next++
	ff := &fakeFirewall{id: st.next, name: f.Name, labels: f.Labels, rules: f.Rules}
	for _, r := range f.AppliedTo {
		ff.appliedTo = append(ff.appliedTo, hetzner.FirewallResource{Type: r.Type, Server: r.Server, LabelSelector: r.LabelSelector})
	}
	st.firewalls[ff.id] = ff
	return ff.id
}

func (s *Server) firewallJSON(f *fakeFirewall) hetzner.Firewall {
	out := hetzner.Firewall{ID: f.id, Name: f.name, Labels: f.labels, Rules: slices.Clone(f.rules), AppliedTo: []hetzner.FirewallResource{}}
	if out.Rules == nil {
		out.Rules = []hetzner.FirewallRule{}
	}
	for _, r := range f.appliedTo {
		res := hetzner.FirewallResource{Type: r.Type, Server: r.Server, LabelSelector: r.LabelSelector}
		if r.LabelSelector != nil {
			for _, srv := range s.serversMatching(r.LabelSelector.Selector) {
				res.AppliedToResources = append(res.AppliedToResources, hetzner.ServerResource(srv.ID))
			}
		}
		out.AppliedTo = append(out.AppliedTo, res)
	}
	return out
}

func validFirewallRules(rules []hetzner.FirewallRule) string {
	if len(rules) > hetzner.MaxFirewallRules {
		return "too many rules"
	}
	for _, r := range rules {
		if r.Direction != "in" && r.Direction != "out" {
			return "direction must be in or out"
		}
		ips := r.SourceIPs
		if r.Direction == "out" {
			ips = r.DestinationIPs
		}
		if len(ips) == 0 || len(ips) > hetzner.MaxFirewallRuleBlocks {
			return "source_ips must have 1 to 100 entries"
		}
		for _, ip := range ips {
			p, err := netip.ParsePrefix(ip)
			if err != nil || p.Masked() != p {
				return "invalid CIDR " + ip
			}
		}
		switch r.Protocol {
		case "tcp", "udp":
			lo, hi, isRange := strings.Cut(r.Port, "-")
			a, err1 := strconv.Atoi(lo)
			b := a
			var err2 error
			if isRange {
				b, err2 = strconv.Atoi(hi)
			}
			if err1 != nil || err2 != nil || a < 1 || b > 65535 || b < a {
				return "invalid port " + r.Port
			}
		case "icmp", "esp", "gre":
			if r.Port != "" {
				return "port is only allowed for tcp and udp"
			}
		default:
			return "invalid protocol " + r.Protocol
		}
	}
	return ""
}

func (s *Server) checkFirewallResources(res []hetzner.FirewallResource) (int, string, string) {
	for _, r := range res {
		switch {
		case r.Type == "server" && r.Server != nil:
			if !s.serverExists(r.Server.ID) {
				return http.StatusNotFound, "firewall_resource_not_found", "server " + strconv.FormatInt(r.Server.ID, 10) + " not found"
			}
		case r.Type == "label_selector" && r.LabelSelector != nil && r.LabelSelector.Selector != "":
		default:
			return http.StatusUnprocessableEntity, "invalid_input", "invalid resource"
		}
	}
	return 0, "", ""
}

func serveFirewalls(s *Server, w http.ResponseWriter, r *http.Request, parts []string) {
	if !s.writable(w, r) {
		return
	}
	st := s.firewallState()
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			out := []hetzner.Firewall{}
			for _, f := range st.firewalls {
				if q := r.URL.Query().Get("name"); q != "" && q != f.name {
					continue
				}
				if sel := r.URL.Query().Get("label_selector"); sel != "" && !cloudSelectorMatch(f.labels, sel) {
					continue
				}
				out = append(out, s.firewallJSON(f))
			}
			slices.SortFunc(out, func(a, b hetzner.Firewall) int { return int(a.ID - b.ID) })
			writeJSON(w, http.StatusOK, map[string]any{"firewalls": out, "meta": noMorePages()})
		case http.MethodPost:
			var req struct {
				Name    string                     `json:"name"`
				Labels  map[string]string          `json:"labels"`
				Rules   []hetzner.FirewallRule     `json:"rules"`
				ApplyTo []hetzner.FirewallResource `json:"apply_to"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "name is required")
				return
			}
			for _, f := range st.firewalls {
				if f.name == req.Name {
					writeErr(w, http.StatusConflict, "uniqueness_error", "name is already used")
					return
				}
			}
			if msg := validFirewallRules(req.Rules); msg != "" {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_input", msg)
				return
			}
			if code, c, msg := s.checkFirewallResources(req.ApplyTo); code != 0 {
				writeErr(w, code, c, msg)
				return
			}
			st.next++
			f := &fakeFirewall{id: st.next, name: req.Name, labels: req.Labels, rules: req.Rules, appliedTo: req.ApplyTo}
			st.firewalls[f.id] = f
			actions := []any{s.NewAction("set_firewall_rules")}
			if len(req.ApplyTo) > 0 {
				actions = append(actions, s.NewAction("apply_firewall"))
			}
			writeJSON(w, http.StatusCreated, map[string]any{"firewall": s.firewallJSON(f), "actions": actions})
		default:
			writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		}
		return
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	f, ok := st.firewalls[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "firewall not found")
		return
	}
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"firewall": s.firewallJSON(f)})
	case len(parts) == 2 && r.Method == http.MethodPut:
		var req struct {
			Name   *string           `json:"name"`
			Labels map[string]string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Name != nil {
			f.name = *req.Name
		}
		if req.Labels != nil {
			f.labels = req.Labels
		}
		writeJSON(w, http.StatusOK, map[string]any{"firewall": s.firewallJSON(f)})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		if len(f.appliedTo) > 0 {
			writeErr(w, http.StatusUnprocessableEntity, "resource_in_use", "firewall still applied to a resource")
			return
		}
		delete(st.firewalls, id)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 4 && parts[2] == "actions" && r.Method == http.MethodPost:
		s.firewallAction(w, r, f, parts[3])
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func (s *Server) firewallAction(w http.ResponseWriter, r *http.Request, f *fakeFirewall, action string) {
	var req struct {
		Rules      []hetzner.FirewallRule     `json:"rules"`
		ApplyTo    []hetzner.FirewallResource `json:"apply_to"`
		RemoveFrom []hetzner.FirewallResource `json:"remove_from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "json_error", "invalid JSON")
		return
	}
	switch action {
	case "set_rules":
		if req.Rules == nil {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "rules is required")
			return
		}
		if msg := validFirewallRules(req.Rules); msg != "" {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_input", msg)
			return
		}
		f.rules = req.Rules
		writeJSON(w, http.StatusCreated, map[string]any{"actions": []any{s.NewAction("set_firewall_rules")}})
	case "apply_to_resources":
		if code, c, msg := s.checkFirewallResources(req.ApplyTo); code != 0 {
			writeErr(w, code, c, msg)
			return
		}
		for _, res := range req.ApplyTo {
			if slices.ContainsFunc(f.appliedTo, func(o hetzner.FirewallResource) bool { return o.Key() == res.Key() }) {
				writeErr(w, http.StatusUnprocessableEntity, "firewall_already_applied", "firewall is already applied to "+res.Key())
				return
			}
		}
		f.appliedTo = append(f.appliedTo, req.ApplyTo...)
		writeJSON(w, http.StatusCreated, map[string]any{"actions": []any{s.NewAction("apply_firewall")}})
	case "remove_from_resources":
		for _, res := range req.RemoveFrom {
			i := slices.IndexFunc(f.appliedTo, func(o hetzner.FirewallResource) bool { return o.Key() == res.Key() })
			if i < 0 {
				writeErr(w, http.StatusNotFound, "firewall_resource_not_found", "not applied to "+res.Key())
				return
			}
			f.appliedTo = slices.Delete(f.appliedTo, i, i+1)
		}
		writeJSON(w, http.StatusCreated, map[string]any{"actions": []any{s.NewAction("remove_firewall")}})
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such action")
	}
}
