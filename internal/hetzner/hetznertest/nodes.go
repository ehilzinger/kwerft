package hetznertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// The Cloud API's server side for node pools: servers, SSH keys, placement
// groups, server types, locations, images and actions. FakeNodes registers
// it; tests read and change the state through the helpers below.

const nodesKey = "nodes"

type nodesState struct {
	nextID      int64
	servers     map[int64]*hetzner.Server
	userData    map[int64]string
	sshKeys     map[int64]*hetzner.SSHKey
	groups      map[int64]*hetzner.PlacementGroup
	types       []hetzner.ServerType
	locations   []hetzner.Location
	images      []hetzner.Image
	actions     map[int64]*hetzner.Action
	deleted     []string // names of deleted servers, in order
	createCalls int
}

// FakeNodes adds the node-pool resource families to the fake, with a small
// catalogue: locations fsn1, nbg1, hel1 (eu-central) and ash; server types
// cx23, cx33 (x86) and cax21 (arm), available everywhere; images
// ubuntu-24.04 and ubuntu-26.04 for both architectures.
func (s *Server) FakeNodes() {
	st := &nodesState{
		nextID: 100, servers: map[int64]*hetzner.Server{}, userData: map[int64]string{}, sshKeys: map[int64]*hetzner.SSHKey{},
		groups: map[int64]*hetzner.PlacementGroup{}, actions: map[int64]*hetzner.Action{},
	}
	zones := map[string]string{"fsn1": "eu-central", "nbg1": "eu-central", "hel1": "eu-central", "ash": "us-east"}
	for i, name := range []string{"fsn1", "nbg1", "hel1", "ash"} {
		st.locations = append(st.locations, hetzner.Location{ID: int64(i + 1), Name: name, NetworkZone: zones[name], Description: strings.ToUpper(name)})
	}
	for i, t := range []struct {
		name, arch string
		cores      int
		mem        float64
		price      string
	}{{"cx23", "x86", 2, 4, "4.7000"}, {"cx33", "x86", 4, 8, "7.6000"}, {"cax21", "arm", 4, 8, "7.0000"}} {
		typ := hetzner.ServerType{ID: int64(i + 1), Name: t.name, Description: strings.ToUpper(t.name), Cores: t.cores, Memory: t.mem,
			Disk: 80, CPUType: "shared", Architecture: t.arch}
		for _, l := range st.locations {
			typ.Locations = append(typ.Locations, hetzner.ServerTypeLocation{ID: l.ID, Name: l.Name, Available: true})
			typ.Prices = append(typ.Prices, hetzner.ServerTypePrice{Location: l.Name, PriceMonthly: hetzner.Price{Net: t.price, Gross: t.price}})
		}
		st.types = append(st.types, typ)
	}
	id := int64(1)
	for _, release := range []string{"24.04", "26.04"} {
		for _, arch := range []string{"x86", "arm"} {
			st.images = append(st.images, hetzner.Image{ID: id, Type: "system", Status: "available", Name: "ubuntu-" + release,
				OSFlavor: "ubuntu", OSVersion: release, Architecture: arch})
			id++
		}
	}
	s.Lock()
	if s.State == nil {
		s.State = map[string]any{}
	}
	s.State[nodesKey] = st
	s.Unlock()
	for _, family := range []string{"servers", "ssh_keys", "placement_groups", "server_types", "locations", "images", "actions"} {
		s.Handle(family, serveNodes)
	}
}

func nodes(s *Server) *nodesState { return s.State[nodesKey].(*nodesState) }

// ---- helpers for tests (they lock the fake) ---------------------------------

// CloudServers returns copies of the fake's servers, sorted by ID.
func (s *Server) CloudServers() []hetzner.Server {
	s.Lock()
	defer s.Unlock()
	st := nodes(s)
	out := make([]hetzner.Server, 0, len(st.servers))
	for _, srv := range st.servers {
		out = append(out, *srv)
	}
	slices.SortFunc(out, func(a, b hetzner.Server) int { return int(a.ID - b.ID) })
	return out
}

// UserData is the cloud-init user data a server was created with.
func (s *Server) UserData(id int64) string {
	s.Lock()
	defer s.Unlock()
	return nodes(s).userData[id]
}

// SetServerStatus changes a server's status (e.g. to running).
func (s *Server) SetServerStatus(id int64, status string) {
	s.Lock()
	defer s.Unlock()
	if srv, ok := nodes(s).servers[id]; ok {
		srv.Status = status
	}
}

// RunAllServers sets every server to running.
func (s *Server) RunAllServers() {
	s.Lock()
	defer s.Unlock()
	for _, srv := range nodes(s).servers {
		srv.Status = hetzner.ServerRunning
	}
}

// PutServer adds a server as if created elsewhere (by hand, or by another
// tool); it gets an ID and is running.
func (s *Server) PutServer(srv hetzner.Server) int64 {
	s.Lock()
	defer s.Unlock()
	st := nodes(s)
	st.nextID++
	srv.ID = st.nextID
	if srv.Status == "" {
		srv.Status = hetzner.ServerRunning
	}
	st.servers[srv.ID] = &srv
	return srv.ID
}

// DeletedServers are the names of the servers deleted so far, in order.
func (s *Server) DeletedServers() []string {
	s.Lock()
	defer s.Unlock()
	return slices.Clone(nodes(s).deleted)
}

// ServerCreates is how many create requests the fake accepted.
func (s *Server) ServerCreates() int {
	s.Lock()
	defer s.Unlock()
	return nodes(s).createCalls
}

// AddSSHKey stores a key as if uploaded in the Hetzner Console.
func (s *Server) AddSSHKey(name string, labels map[string]string) int64 {
	s.Lock()
	defer s.Unlock()
	st := nodes(s)
	st.nextID++
	st.sshKeys[st.nextID] = &hetzner.SSHKey{ID: st.nextID, Name: name, PublicKey: "ssh-ed25519 AAAA" + name, Labels: labels}
	return st.nextID
}

// PlacementGroupsNow returns copies of the placement groups.
func (s *Server) PlacementGroupsNow() []hetzner.PlacementGroup {
	s.Lock()
	defer s.Unlock()
	var out []hetzner.PlacementGroup
	for _, g := range nodes(s).groups {
		cp := *g
		cp.Servers = slices.Clone(g.Servers)
		out = append(out, cp)
	}
	return out
}

// SetTypeAvailable marks a server type (un)available in a location.
func (s *Server) SetTypeAvailable(typ, location string, available bool) {
	s.Lock()
	defer s.Unlock()
	for i := range nodes(s).types {
		t := &nodes(s).types[i]
		if t.Name != typ {
			continue
		}
		for j := range t.Locations {
			if t.Locations[j].Name == location {
				t.Locations[j].Available = available
			}
		}
	}
}

// ---- the API ----------------------------------------------------------------

// matches implements the Cloud API's label selectors: k, !k, k=v, k!=v,
// joined by commas.
func matches(labels map[string]string, sel string) bool {
	if sel == "" {
		return true
	}
	for _, term := range strings.Split(sel, ",") {
		term = strings.TrimSpace(term)
		switch {
		case strings.Contains(term, "!="):
			k, v, _ := strings.Cut(term, "!=")
			if labels[k] == v {
				return false
			}
		case strings.Contains(term, "="):
			k, v, _ := strings.Cut(term, "=")
			if got, ok := labels[k]; !ok || got != v {
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

var lastPage = map[string]any{"pagination": map[string]any{"page": 1, "next_page": nil}}

func (st *nodesState) action(command string) hetzner.Action {
	st.nextID++
	a := &hetzner.Action{ID: st.nextID, Command: command, Status: hetzner.ActionSuccess, Progress: 100}
	st.actions[a.ID] = a
	return *a
}

func serveNodes(s *Server, w http.ResponseWriter, r *http.Request, parts []string) {
	st := nodes(s)
	q := r.URL.Query()
	var id int64
	if len(parts) > 1 {
		var err error
		if id, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid_input", "invalid id")
			return
		}
	}
	switch {
	case parts[0] == "locations" && len(parts) == 1 && r.Method == http.MethodGet:
		WriteJSON(w, http.StatusOK, map[string]any{"locations": st.locations, "meta": lastPage})
	case parts[0] == "server_types" && len(parts) == 1 && r.Method == http.MethodGet:
		WriteJSON(w, http.StatusOK, map[string]any{"server_types": st.types, "meta": lastPage})
	case parts[0] == "images" && len(parts) == 1 && r.Method == http.MethodGet:
		out := []hetzner.Image{}
		for _, img := range st.images {
			if t := q.Get("type"); t != "" && img.Type != t || q.Get("architecture") != "" && img.Architecture != q.Get("architecture") {
				continue
			}
			out = append(out, img)
		}
		WriteJSON(w, http.StatusOK, map[string]any{"images": out, "meta": lastPage})
	case parts[0] == "actions" && len(parts) == 2 && r.Method == http.MethodGet:
		a, ok := st.actions[id]
		if !ok {
			WriteError(w, http.StatusNotFound, "not_found", "action not found")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"action": a})
	case parts[0] == "ssh_keys":
		serveSSHKeys(s, st, w, r, parts, id)
	case parts[0] == "placement_groups":
		servePlacementGroups(s, st, w, r, parts, id)
	case parts[0] == "servers":
		serveServers(s, st, w, r, parts, id)
	default:
		WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func serveSSHKeys(s *Server, st *nodesState, w http.ResponseWriter, r *http.Request, parts []string, id int64) {
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		out := []hetzner.SSHKey{}
		for _, k := range st.sshKeys {
			if matches(k.Labels, r.URL.Query().Get("label_selector")) {
				out = append(out, *k)
			}
		}
		slices.SortFunc(out, func(a, b hetzner.SSHKey) int { return int(a.ID - b.ID) })
		WriteJSON(w, http.StatusOK, map[string]any{"ssh_keys": out, "meta": lastPage})
	case len(parts) == 1 && r.Method == http.MethodPost:
		var k hetzner.SSHKey
		if err := json.NewDecoder(r.Body).Decode(&k); err != nil || k.Name == "" || k.PublicKey == "" {
			WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "name and public_key are required")
			return
		}
		s.writes++
		st.nextID++
		k.ID = st.nextID
		st.sshKeys[k.ID] = &k
		WriteJSON(w, http.StatusCreated, map[string]any{"ssh_key": k})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		if _, ok := st.sshKeys[id]; !ok {
			WriteError(w, http.StatusNotFound, "not_found", "ssh key not found")
			return
		}
		s.writes++
		delete(st.sshKeys, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func servePlacementGroups(s *Server, st *nodesState, w http.ResponseWriter, r *http.Request, parts []string, id int64) {
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		out := []hetzner.PlacementGroup{}
		for _, g := range st.groups {
			if matches(g.Labels, r.URL.Query().Get("label_selector")) && (r.URL.Query().Get("name") == "" || r.URL.Query().Get("name") == g.Name) {
				out = append(out, *g)
			}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"placement_groups": out, "meta": lastPage})
	case len(parts) == 1 && r.Method == http.MethodPost:
		var g hetzner.PlacementGroup
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil || g.Name == "" || g.Type != "spread" {
			WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "name and type spread are required")
			return
		}
		for _, o := range st.groups {
			if o.Name == g.Name {
				WriteError(w, http.StatusConflict, "uniqueness_error", "name is already used")
				return
			}
		}
		s.writes++
		st.nextID++
		g.ID, g.Servers = st.nextID, []int64{}
		st.groups[g.ID] = &g
		WriteJSON(w, http.StatusCreated, map[string]any{"placement_group": g})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		g, ok := st.groups[id]
		if !ok {
			WriteError(w, http.StatusNotFound, "not_found", "placement group not found")
			return
		}
		if len(g.Servers) > 0 {
			WriteError(w, http.StatusUnprocessableEntity, "resource_in_use", "placement group still has servers")
			return
		}
		s.writes++
		delete(st.groups, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func serveServers(s *Server, st *nodesState, w http.ResponseWriter, r *http.Request, parts []string, id int64) {
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		out := []hetzner.Server{}
		for _, srv := range st.servers {
			if matches(srv.Labels, r.URL.Query().Get("label_selector")) && (r.URL.Query().Get("name") == "" || r.URL.Query().Get("name") == srv.Name) {
				out = append(out, *srv)
			}
		}
		slices.SortFunc(out, func(a, b hetzner.Server) int { return int(a.ID - b.ID) })
		WriteJSON(w, http.StatusOK, map[string]any{"servers": out, "meta": lastPage})
	case len(parts) == 1 && r.Method == http.MethodPost:
		createServer(s, st, w, r)
	case len(parts) == 2 && r.Method == http.MethodGet:
		srv, ok := st.servers[id]
		if !ok {
			WriteError(w, http.StatusNotFound, "not_found", "server not found")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"server": srv})
	case len(parts) == 2 && r.Method == http.MethodPut:
		srv, ok := st.servers[id]
		if !ok {
			WriteError(w, http.StatusNotFound, "not_found", "server not found")
			return
		}
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.writes++
		srv.Labels = body.Labels
		WriteJSON(w, http.StatusOK, map[string]any{"server": srv})
	case len(parts) == 2 && r.Method == http.MethodDelete:
		srv, ok := st.servers[id]
		if !ok {
			WriteError(w, http.StatusNotFound, "not_found", "server not found")
			return
		}
		s.writes++
		delete(st.servers, id)
		st.deleted = append(st.deleted, srv.Name)
		if srv.PlacementGroup != nil {
			if g, ok := st.groups[srv.PlacementGroup.ID]; ok {
				g.Servers = slices.DeleteFunc(g.Servers, func(x int64) bool { return x == id })
			}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"action": st.action("delete_server")})
	default:
		WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func createServer(s *Server, st *nodesState, w http.ResponseWriter, r *http.Request) {
	var in hetzner.CreateServerOpts
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.ServerType == "" || in.Image == "" {
		WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "name, server_type and image are required")
		return
	}
	if !hetzner.ValidServerName(in.Name) {
		WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "invalid name")
		return
	}
	for _, o := range st.servers {
		if o.Name == in.Name {
			WriteError(w, http.StatusConflict, "uniqueness_error", "server name is already used")
			return
		}
	}
	typ, ok := hetzner.FindServerType(st.types, in.ServerType)
	if !ok {
		WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "unknown server type")
		return
	}
	if !typ.AvailableIn(in.Location) {
		WriteError(w, http.StatusPreconditionFailed, "resource_unavailable", "server type not available in this location")
		return
	}
	var loc hetzner.Location
	for _, l := range st.locations {
		if l.Name == in.Location {
			loc = l
		}
	}
	var img *hetzner.Image
	for i := range st.images {
		if st.images[i].Name == in.Image && st.images[i].Architecture == typ.Architecture {
			img = &st.images[i]
		}
	}
	if img == nil {
		WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "image not found for this architecture")
		return
	}
	for _, k := range in.SSHKeys {
		n, _ := strconv.ParseInt(k, 10, 64)
		if _, ok := st.sshKeys[n]; !ok {
			WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "unknown ssh key "+k)
			return
		}
	}
	st.createCalls++
	s.writes++
	st.nextID++
	srv := &hetzner.Server{ID: st.nextID, Name: in.Name, Status: "initializing", Created: time.Now().UTC(), Labels: in.Labels,
		ServerType: typ, Location: loc, Image: img}
	srv.PublicNet.IPv4 = &struct {
		IP string `json:"ip"`
	}{IP: fmt.Sprintf("203.0.113.%d", srv.ID%250+1)}
	for _, n := range in.Networks {
		srv.PrivateNet = append(srv.PrivateNet, struct {
			Network int64  `json:"network"`
			IP      string `json:"ip"`
		}{Network: n, IP: fmt.Sprintf("10.0.0.%d", srv.ID%250+2)})
	}
	if in.PlacementGroup != 0 {
		g, ok := st.groups[in.PlacementGroup]
		if !ok {
			WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "placement group not found")
			return
		}
		if len(g.Servers) >= hetzner.SpreadGroupMax {
			WriteError(w, http.StatusUnprocessableEntity, "placement_error", "placement group is full")
			return
		}
		g.Servers = append(g.Servers, srv.ID)
		srv.PlacementGroup = &hetzner.PlacementGroup{ID: g.ID, Name: g.Name, Type: g.Type}
	}
	st.servers[srv.ID] = srv
	st.userData[srv.ID] = in.UserData
	WriteJSON(w, http.StatusCreated, map[string]any{"server": srv, "action": st.action("create_server"), "next_actions": []any{}})
}

// FakeClusterNetworks answers the network lookups node pools make
// (GET /networks by label or name) and add_subnet, for these networks.
// Tests of node pools use it; the networks fake of the Cloud integrations
// (W1) replaces it where both are needed. It returns the IDs it gave.
func (s *Server) FakeClusterNetworks(nets ...hetzner.NetworkRef) []int64 {
	var ids []int64
	for i := range nets {
		if nets[i].ID == 0 {
			nets[i].ID = int64(9000 + i)
		}
		ids = append(ids, nets[i].ID)
	}
	s.Lock()
	if s.State == nil {
		s.State = map[string]any{}
	}
	s.State["subnets"] = &[]fakeSubnet{}
	s.Unlock()
	s.Handle("networks", func(s *Server, w http.ResponseWriter, r *http.Request, parts []string) {
		q := r.URL.Query()
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			out := []hetzner.NetworkRef{}
			for _, n := range nets {
				sel := q.Get("label_selector")
				// Networks named kwerft-<cluster> carry the cluster label.
				l := map[string]string{}
				if strings.HasPrefix(n.Name, "kwerft-") {
					l[hetzner.LabelCluster] = strings.TrimPrefix(n.Name, "kwerft-")
				}
				if !matches(l, sel) {
					continue
				}
				if q.Get("name") != "" && q.Get("name") != n.Name {
					continue
				}
				out = append(out, n)
			}
			WriteJSON(w, http.StatusOK, map[string]any{"networks": out, "meta": lastPage})
		case len(parts) == 4 && parts[2] == "actions" && parts[3] == "add_subnet" && r.Method == http.MethodPost:
			nid, _ := strconv.ParseInt(parts[1], 10, 64)
			if !slices.Contains(ids, nid) {
				WriteError(w, http.StatusNotFound, "not_found", "network not found")
				return
			}
			var body struct {
				Type        string `json:"type"`
				IPRange     string `json:"ip_range"`
				NetworkZone string `json:"network_zone"`
				VSwitchID   int64  `json:"vswitch_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.NetworkZone == "" || body.Type == "vswitch" && body.VSwitchID == 0 {
				WriteError(w, http.StatusUnprocessableEntity, "invalid_input", "type, network_zone and vswitch_id are required")
				return
			}
			s.writes++
			subs := s.State["subnets"].(*[]fakeSubnet)
			*subs = append(*subs, fakeSubnet{Network: nid, Type: body.Type, IPRange: body.IPRange, NetworkZone: body.NetworkZone, VSwitch: body.VSwitchID})
			WriteJSON(w, http.StatusCreated, map[string]any{"action": map[string]any{"id": 1, "command": "add_subnet", "status": "running", "progress": 0}})
		default:
			WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
		}
	})
	return ids
}

type fakeSubnet struct {
	Network, VSwitch           int64
	Type, IPRange, NetworkZone string
}

// VSwitchSubnets lists "network/vswitch/ip_range/zone" of every vswitch
// subnet added through FakeClusterNetworks.
func (s *Server) VSwitchSubnets() []string {
	s.Lock()
	defer s.Unlock()
	subs, _ := s.State["subnets"].(*[]fakeSubnet)
	if subs == nil {
		return nil
	}
	var out []string
	for _, sub := range *subs {
		if sub.Type == "vswitch" {
			out = append(out, fmt.Sprintf("%d/%d/%s/%s", sub.Network, sub.VSwitch, sub.IPRange, sub.NetworkZone))
		}
	}
	return out
}
