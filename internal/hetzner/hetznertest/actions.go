// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hetznertest

import (
	"net/http"
	"strconv"
	"strings"
)

// Actions for the fake's resource families: changing calls answer with an
// action (NewAction), which GET /actions/{id} reports. By default actions
// finish at once; RunActionsFor makes them run for a few polls, FailNextAction
// makes the next one fail.

type fakeAction struct {
	id      int64
	command string
	polls   int // polls left while running
	failure string
}

type actionState struct {
	next     int64
	actions  map[int64]*fakeAction
	running  int
	failNext string
}

func (s *Server) actionState() *actionState {
	if s.State == nil {
		s.State = map[string]any{}
	}
	st, ok := s.State["actions"].(*actionState)
	if !ok {
		st = &actionState{next: 1000, actions: map[int64]*fakeAction{}}
		s.State["actions"] = st
		if s.handlers == nil {
			s.handlers = map[string]Handler{}
		}
		s.handlers["actions"] = serveActions
	}
	return st
}

// EnableActions serves GET /actions/{id}; the families that create actions
// call it themselves.
func (s *Server) EnableActions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actionState()
}

// RunActionsFor makes new actions report "running" for n polls.
func (s *Server) RunActionsFor(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actionState().running = n
}

// FailNextAction makes the next action end with this error code.
func (s *Server) FailNextAction(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actionState().failNext = code
}

// NewAction records an action for command and returns it as the API shows
// it. Handlers call it with the fake's lock held.
func (s *Server) NewAction(command string) map[string]any {
	st := s.actionState()
	st.next++
	a := &fakeAction{id: st.next, command: command, polls: st.running, failure: st.failNext}
	st.failNext = ""
	st.actions[a.id] = a
	return actionJSON(a)
}

func actionJSON(a *fakeAction) map[string]any {
	out := map[string]any{"id": a.id, "command": a.command, "status": "success", "progress": 100, "error": nil}
	switch {
	case a.polls > 0:
		out["status"], out["progress"] = "running", 50
	case a.failure != "":
		out["status"] = "error"
		out["error"] = map[string]string{"code": a.failure, "message": "injected action failure"}
	}
	return out
}

func serveActions(s *Server, w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 2 || r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	a, ok := s.actionState().actions[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "action not found")
		return
	}
	if a.polls > 0 {
		a.polls--
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": actionJSON(a)})
}

// cloudSelectorMatch evaluates the Cloud API's label selectors as far as
// Kwerft uses them: comma-separated "k=v", "k!=v", "k" and "!k".
func cloudSelectorMatch(labels map[string]string, selector string) bool {
	for _, term := range strings.Split(selector, ",") {
		term = strings.TrimSpace(term)
		switch {
		case term == "":
		case strings.Contains(term, "!="):
			k, v, _ := strings.Cut(term, "!=")
			if labels[strings.TrimSpace(k)] == strings.TrimSpace(v) {
				return false
			}
		case strings.Contains(term, "="):
			k, v, _ := strings.Cut(strings.Replace(term, "==", "=", 1), "=")
			got, ok := labels[strings.TrimSpace(k)]
			if !ok || got != strings.TrimSpace(v) {
				return false
			}
		case strings.HasPrefix(term, "!"):
			if _, ok := labels[term[1:]]; ok {
				return false
			}
		default:
			if _, ok := labels[term]; !ok {
				return false
			}
		}
	}
	return true
}

// writable answers 403 for a read-only fake (SetReadOnly) on changing calls.
func (s *Server) writable(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	if ro, _ := s.State["readOnly"].(bool); ro {
		writeErr(w, http.StatusForbidden, "forbidden", "insufficient permissions for this request")
		return false
	}
	s.writes++
	return true
}

// SetReadOnly makes the token a Read-only one for the Cloud resource
// families (not DNS).
func (s *Server) SetReadOnly(ro bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.State == nil {
		s.State = map[string]any{}
	}
	s.State["readOnly"] = ro
}

func noMorePages() map[string]any {
	return map[string]any{"pagination": map[string]any{"page": 1, "per_page": 50, "next_page": nil, "previous_page": nil, "last_page": 1}}
}
