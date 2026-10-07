// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hetznertest

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Robot is an in-memory Hetzner Robot webservice with vSwitches, behind
// one webservice user (HTTP basic auth, form-encoded requests).
type Robot struct {
	*httptest.Server
	User, Password string

	mu       sync.Mutex
	nextID   int64
	vswitchs map[int64]*hetzner.VSwitch
}

// NewRobot starts the fake; it closes with the test.
func NewRobot(t testing.TB, user, password string) *Robot {
	r := &Robot{User: user, Password: password, nextID: 1000, vswitchs: map[int64]*hetzner.VSwitch{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Close)
	return r
}

// Client is a client for this fake with its credentials.
func (r *Robot) Client() *hetzner.RobotClient {
	return &hetzner.RobotClient{User: r.User, Password: r.Password, Base: r.URL}
}

// VSwitchesNow returns copies of the vSwitches.
func (r *Robot) VSwitchesNow() []hetzner.VSwitch {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hetzner.VSwitch
	for _, v := range r.vswitchs {
		out = append(out, *v)
	}
	return out
}

func robotErr(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, map[string]any{"error": map[string]any{"status": status, "code": code, "message": msg}})
}

func (r *Robot) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, p, ok := req.BasicAuth(); !ok || u != r.User || p != r.Password {
		robotErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
		return
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if parts[0] != "vswitch" {
		robotErr(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	_ = req.ParseForm()
	switch {
	case len(parts) == 1 && req.Method == http.MethodGet:
		out := []map[string]any{}
		for _, v := range r.vswitchs {
			out = append(out, map[string]any{"id": v.ID, "name": v.Name, "vlan": v.VLAN, "cancelled": v.Cancelled})
		}
		WriteJSON(w, http.StatusOK, out)
	case len(parts) == 1 && req.Method == http.MethodPost:
		vlan, err := strconv.Atoi(req.PostForm.Get("vlan"))
		name := req.PostForm.Get("name")
		if err != nil || vlan < 4000 || vlan > 4091 || name == "" {
			robotErr(w, http.StatusBadRequest, "INVALID_INPUT", "invalid input")
			return
		}
		for _, v := range r.vswitchs {
			if v.VLAN == vlan && !v.Cancelled {
				robotErr(w, http.StatusConflict, "VSWITCH_VLAN_NOT_UNIQUE", "conflicting VLAN ID")
				return
			}
		}
		r.nextID++
		v := &hetzner.VSwitch{ID: r.nextID, Name: name, VLAN: vlan}
		r.vswitchs[v.ID] = v
		WriteJSON(w, http.StatusCreated, v)
	case len(parts) >= 2:
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		v, ok := r.vswitchs[id]
		if !ok {
			robotErr(w, http.StatusNotFound, "NOT_FOUND", "vSwitch not found")
			return
		}
		switch {
		case len(parts) == 2 && req.Method == http.MethodGet:
			WriteJSON(w, http.StatusOK, v)
		case len(parts) == 3 && parts[2] == "server" && req.Method == http.MethodPost:
			servers := req.PostForm["server[]"]
			if len(servers) == 0 {
				robotErr(w, http.StatusBadRequest, "INVALID_INPUT", "invalid input")
				return
			}
			for _, s := range servers {
				entry := hetzner.VSwitchServer{Status: "in process"}
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					entry.ServerNumber = n
				} else {
					entry.ServerIP = s
				}
				v.Server = append(v.Server, entry)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			robotErr(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		}
	default:
		robotErr(w, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}
