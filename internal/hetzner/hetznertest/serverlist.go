// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hetznertest

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// A read-only server list (GET /servers) for the firewall and Load Balancer
// integrations: tests put servers in with AddServer. The node pools' fake
// (servers that can be created and deleted) replaces it when both are used;
// it keeps its servers in the same State key.

const serverListKey = "serverList"

func (s *Server) serverList() []hetzner.ServerSummary {
	list, _ := s.State[serverListKey].([]hetzner.ServerSummary)
	return list
}

// AddServer adds (or replaces, by ID) a server and serves GET /servers.
func (s *Server) AddServer(srv hetzner.ServerSummary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.State == nil {
		s.State = map[string]any{}
	}
	if s.handlers == nil {
		s.handlers = map[string]Handler{}
	}
	if _, ok := s.handlers["servers"]; !ok {
		s.handlers["servers"] = serveServerList
	}
	list := slices.DeleteFunc(s.serverList(), func(o hetzner.ServerSummary) bool { return o.ID == srv.ID })
	s.State[serverListKey] = append(list, cloneServer(srv))
}

// NewServerSummary builds a server with a public IPv4, an IPv6 /64 and,
// when privateIP is set, a place in network.
func NewServerSummary(id int64, name, location, publicIP, ipv6Net string, network int64, privateIP string, labels map[string]string) hetzner.ServerSummary {
	raw, _ := json.Marshal(map[string]any{
		"id": id, "name": name, "status": "running", "labels": labels,
		"public_net": map[string]any{"ipv4": map[string]string{"ip": publicIP}, "ipv6": map[string]string{"ip": ipv6Net}},
		"private_net": func() []map[string]any {
			if privateIP == "" {
				return []map[string]any{}
			}
			return []map[string]any{{"network": network, "ip": privateIP}}
		}(),
		"location": map[string]string{"name": location, "network_zone": "eu-central"},
	})
	var out hetzner.ServerSummary
	_ = json.Unmarshal(raw, &out)
	return out
}

func cloneServer(srv hetzner.ServerSummary) hetzner.ServerSummary {
	raw, _ := json.Marshal(srv)
	var out hetzner.ServerSummary
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *Server) serverExists(id int64) bool {
	return slices.ContainsFunc(s.serverList(), func(o hetzner.ServerSummary) bool { return o.ID == id })
}

func (s *Server) serversMatching(selector string) []hetzner.ServerSummary {
	var out []hetzner.ServerSummary
	for _, srv := range s.serverList() {
		if cloudSelectorMatch(srv.Labels, selector) {
			out = append(out, srv)
		}
	}
	return out
}

func serveServerList(s *Server, w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 1 || r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	list := s.serversMatching(r.URL.Query().Get("label_selector"))
	if list == nil {
		list = []hetzner.ServerSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": list, "meta": noMorePages()})
}
