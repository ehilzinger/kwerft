// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package hetznertest is an in-memory Hetzner Cloud DNS API for tests: zones
// and their RRsets, behind one token.
package hetznertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Server is a fake Cloud API. Lock it (or use the helpers) to inspect state.
type Server struct {
	*httptest.Server
	Token string

	mu    sync.Mutex
	zones map[string]*zone // by name
	// Writes counts create, set_records, label and delete calls.
	writes int
	// Fail, when set, answers every request with this status.
	fail int
	// handlers serve other Cloud API resources (servers, firewalls, ...),
	// registered by the files that fake them; by first path segment.
	handlers map[string]Handler
	// State is free for those handlers, guarded by the server's lock.
	State map[string]any
}

// Handler serves one Cloud API resource family (e.g. "servers") under the
// fake's lock, after the token check. parts is the path split at "/".
type Handler func(s *Server, w http.ResponseWriter, r *http.Request, parts []string)

// Handle registers h for paths whose first segment is resource. Call it
// before the first request; files that fake a resource family do it in a
// constructor of their own (e.g. NewCloud) so each family stays in its own
// file.
func (s *Server) Handle(resource string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlers == nil {
		s.handlers = map[string]Handler{}
	}
	s.handlers[resource] = h
}

// Lock and Unlock let tests and handlers' helpers inspect State safely.
func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// WriteJSON and WriteError answer like the Cloud API does.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	writeErr(w, status, code, msg)
}

type zone struct {
	id     int64
	mode   string
	rrsets map[string]*hetzner.RRSet // "name/TYPE"
}

// New starts a fake with the given primary zones; it closes with the test.
func New(t testing.TB, token string, zones ...string) *Server {
	s := &Server{Token: token, zones: map[string]*zone{}}
	for _, z := range zones {
		s.AddZone(z, "primary")
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Client is a client for this fake with its token.
func (s *Server) Client() *hetzner.Client {
	return &hetzner.Client{Token: s.Token, Base: s.URL}
}

func (s *Server) AddZone(name, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zones[name] = &zone{id: int64(len(s.zones) + 1), mode: mode, rrsets: map[string]*hetzner.RRSet{}}
}

// Put stores an RRset as if someone created it in the Hetzner Console.
func (s *Server) Put(zoneName string, set hetzner.RRSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := clone(set)
	s.zones[zoneName].rrsets[set.Name+"/"+set.Type] = &cp
}

// Get returns a copy of an RRset, or false.
func (s *Server) Get(zoneName, name, typ string) (hetzner.RRSet, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, ok := s.zones[zoneName]
	if !ok {
		return hetzner.RRSet{}, false
	}
	set, ok := z.rrsets[name+"/"+typ]
	if !ok {
		return hetzner.RRSet{}, false
	}
	return clone(*set), true
}

// Names lists "name/TYPE" of every RRset in the zone, sorted.
func (s *Server) Names(zoneName string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.zones[zoneName].rrsets {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Writes is how many changing requests the fake has served.
func (s *Server) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// SetToken changes the token the fake accepts (a rotated token).
func (s *Server) SetToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Token = token
}

// FailWith makes every request answer status (0 turns it off).
func (s *Server) FailWith(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = status
}

func clone(set hetzner.RRSet) hetzner.RRSet {
	set.Records = slices.Clone(set.Records)
	if set.Labels != nil {
		l := make(map[string]string, len(set.Labels))
		for k, v := range set.Labels {
			l[k] = v
		}
		set.Labels = l
	}
	return set
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var action = map[string]any{"action": map[string]any{"id": 1, "command": "x", "status": "success", "progress": 100}}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != 0 {
		writeErr(w, s.fail, "service_error", "injected failure")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "unable to authenticate")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if h, ok := s.handlers[parts[0]]; ok {
		if s.State == nil {
			s.State = map[string]any{}
		}
		h(s, w, r, parts)
		return
	}
	noMore := map[string]any{"pagination": map[string]any{"next_page": nil}}

	if len(parts) == 1 && parts[0] == "zones" && r.Method == http.MethodGet {
		zones := []map[string]any{}
		for name, z := range s.zones {
			if q := r.URL.Query().Get("name"); q != "" && q != name {
				continue
			}
			zones = append(zones, map[string]any{"id": z.id, "name": name, "mode": z.mode})
		}
		writeJSON(w, http.StatusOK, map[string]any{"zones": zones, "meta": noMore})
		return
	}
	if len(parts) < 3 || parts[0] != "zones" || parts[2] != "rrsets" {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	z, ok := s.zones[parts[1]]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "zone not found")
		return
	}
	if z.mode != "primary" {
		writeErr(w, http.StatusUnprocessableEntity, "incorrect_zone_mode", "not supported for secondary zones")
		return
	}

	switch {
	case len(parts) == 3 && r.Method == http.MethodGet:
		types := r.URL.Query()["type"]
		out := []hetzner.RRSet{}
		for _, set := range z.rrsets {
			if len(types) == 0 || slices.Contains(types, set.Type) {
				out = append(out, clone(*set))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"rrsets": out, "meta": noMore})

	case len(parts) == 3 && r.Method == http.MethodPost:
		var set hetzner.RRSet
		if err := json.NewDecoder(r.Body).Decode(&set); err != nil || set.Name == "" || len(set.Records) == 0 {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "invalid rrset")
			return
		}
		key := set.Name + "/" + set.Type
		if _, exists := z.rrsets[key]; exists {
			writeErr(w, http.StatusConflict, "uniqueness_error", "rrset exists")
			return
		}
		if set.Type != "CNAME" && z.rrsets[set.Name+"/CNAME"] != nil || set.Type == "CNAME" && hasName(z, set.Name) {
			writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "CNAME cannot coexist with other records")
			return
		}
		s.writes++
		z.rrsets[key] = &set
		writeJSON(w, http.StatusCreated, map[string]any{"rrset": set, "action": action["action"]})

	case len(parts) >= 5:
		key := parts[3] + "/" + parts[4]
		set, exists := z.rrsets[key]
		if !exists {
			writeErr(w, http.StatusNotFound, "not_found", "rrset not found")
			return
		}
		switch {
		case len(parts) == 5 && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"rrset": set})
		case len(parts) == 5 && r.Method == http.MethodPut:
			var body struct {
				Labels map[string]string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.writes++
			set.Labels = body.Labels
			writeJSON(w, http.StatusOK, map[string]any{"rrset": set})
		case len(parts) == 5 && r.Method == http.MethodDelete:
			s.writes++
			delete(z.rrsets, key)
			writeJSON(w, http.StatusCreated, action)
		case len(parts) == 7 && parts[6] == "set_records" && r.Method == http.MethodPost:
			var body struct {
				Records []hetzner.Record `json:"records"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Records) == 0 {
				writeErr(w, http.StatusUnprocessableEntity, "invalid_input", "records must not be empty")
				return
			}
			s.writes++
			set.Records = body.Records
			writeJSON(w, http.StatusCreated, action)
		default:
			writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		}
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func hasName(z *zone, name string) bool {
	for _, set := range z.rrsets {
		if set.Name == name {
			return true
		}
	}
	return false
}
