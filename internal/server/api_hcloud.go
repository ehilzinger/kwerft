// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Settings › Hetzner Cloud API: the Cloud API token (write-only, like the
// DNS token: owners and admins may patch the Secret kwerft-hcloud-token but
// not read it), whether Kwerft keeps the Cloud Firewall, and the Load
// Balancer in front of the ingress (ConsoleSettings.spec.hetznerCloud). The
// Hetzner Cloud reconciler does the work and reports into
// status.hetznerCloud. Every write is impersonated and audited.

// Clusters: the Cloud Firewall and the Load Balancer are per cluster, so
// PUT /settings/hcloud takes ?cluster=<name> (default local). A remote
// hetzner-cloud cluster's settings live on its Cluster object
// (spec.hetznerCloud), which the Cluster reconciler hands to the cluster's
// own Hetzner Cloud reconciler through the tunnel, with the project token
// (Hetzner tokens cannot be scoped; docs/phase5.md › As built (W1)). The
// token is the management cluster's only: the token routes refuse other
// clusters.

var clusterNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// hcloudCluster reads ?cluster= (default local).
func hcloudCluster(r *http.Request) (string, bool) {
	name := r.URL.Query().Get("cluster")
	if name == "" {
		return clusters.Local, true
	}
	return name, clusterNameRE.MatchString(name)
}

// clusterRefused answers a request for a cluster this route cannot serve.
func clusterRefused(w http.ResponseWriter, r *http.Request, tokenRoute bool) bool {
	cluster, ok := hcloudCluster(r)
	switch {
	case !ok:
		writeFieldError(w, "cluster", "Unknown cluster.")
		return true
	case tokenRoute && cluster != clusters.Local:
		writeError(w, http.StatusBadRequest, "The Hetzner Cloud API token is set in the management cluster's settings; clusters Kwerft creates get it from there.")
		return true
	}
	return false
}

// HCloudVolumesClass is the StorageClass of hcloud-volume Volumes (the CSI
// driver's, installed by install.sh with a Cloud API token).
const HCloudVolumesClass = "hcloud-volumes"

func (s *settingsAPI) registerHCloud(mux *http.ServeMux, admin func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("PUT /api/v1/settings/hcloud-token", admin(s.setHCloudToken))
	mux.HandleFunc("DELETE /api/v1/settings/hcloud-token", admin(s.removeHCloudToken))
	mux.HandleFunc("PUT /api/v1/settings/hcloud", admin(s.setHCloud))
	mux.HandleFunc("GET /api/v1/volume-classes", s.requireUser(s.volumeClasses))
}

type hcloudLoadBalancerJSON struct {
	Enabled  bool   `json:"enabled"`
	Type     string `json:"type,omitempty"`
	Location string `json:"location,omitempty"`
}

type hcloudJSON struct {
	TokenSet bool `json:"tokenSet"`
	// Platform the installer detected (cloud | dedicated).
	Platform string `json:"platform"`
	// CCM: the hcloud cloud-controller-manager runs (an install-time choice).
	CCM bool `json:"ccm"`
	// Volumes: the hcloud-volumes StorageClass exists (CSI driver).
	Volumes bool `json:"volumes"`
	// LoadBalancerReady: the ingress accepts the PROXY protocol from the
	// private network, so a Load Balancer can be turned on.
	LoadBalancerReady bool                         `json:"loadBalancerReady"`
	Firewall          string                       `json:"firewall"` // sync | off
	LoadBalancer      hcloudLoadBalancerJSON       `json:"loadBalancer"`
	Status            *kwerftv1.HetznerCloudStatus `json:"status,omitempty"`
}

func (s *settingsAPI) hcloudView(ctx context.Context, cs *kwerftv1.ConsoleSettings) hcloudJSON {
	out := hcloudJSON{Platform: s.cfg.Platform, CCM: s.cfg.HCloudCCM, LoadBalancerReady: s.cfg.HCloudProxyNetwork != "",
		Firewall: string(kwerftv1.CloudFirewallSync), Volumes: s.storageClassExists(ctx, HCloudVolumesClass)}
	if cs == nil {
		return out
	}
	out.TokenSet = cs.Annotations[controllers.AnnotationHCloudTokenUpdated] != ""
	if h := cs.Spec.HetznerCloud; h != nil {
		if h.Firewall != "" {
			out.Firewall = string(h.Firewall)
		}
		if lb := h.LoadBalancer; lb != nil {
			out.LoadBalancer = hcloudLoadBalancerJSON{Enabled: lb.Enabled, Type: lb.Type, Location: lb.Location}
		}
	}
	if out.TokenSet {
		out.Status = cs.Status.HetznerCloud
	}
	return out
}

// storageClassExists asks with the console's own identity (StorageClass
// names are no secret, and users cannot list them).
func (s *settingsAPI) storageClassExists(ctx context.Context, name string) bool {
	if s.cfg.StorageClassExists != nil {
		return s.cfg.StorageClassExists(ctx, name)
	}
	return false
}

// StorageClassChecker returns a Config.StorageClassExists that reads with r.
func StorageClassChecker(r client.Reader) func(context.Context, string) bool {
	return func(ctx context.Context, name string) bool {
		// A cache that cannot sync (missing RBAC) must not hang the page.
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var sc storagev1.StorageClass
		return r.Get(ctx, client.ObjectKey{Name: name}, &sc) == nil
	}
}

// ---- the token -------------------------------------------------------------------

type hcloudCheckJSON struct {
	// Servers in the token's project.
	Servers int `json:"servers"`
	// Nodes of this cluster found among them.
	Nodes []hcloudNodeJSON `json:"nodes"`
	// Locations the project's servers are in.
	Locations []string `json:"locations"`
}

type hcloudNodeJSON struct {
	Node     string `json:"node"`
	Server   string `json:"server"`
	Location string `json:"location"`
}

// checkHCloudToken lists the token's servers (any valid token may), finds
// this cluster's nodes among them and makes sure it may write.
func (s *settingsAPI) checkHCloudToken(ctx context.Context, c client.Client, token string) (*hcloudCheckJSON, string, error) {
	hz := &hetzner.Client{Token: token, Base: s.hetznerAPI, HTTP: s.http}
	servers, err := hz.ServerSummaries(ctx, "")
	if err != nil {
		return nil, "", err
	}
	if err := hz.ProbeWrite(ctx); err != nil {
		return nil, "", err
	}
	out := &hcloudCheckJSON{Servers: len(servers), Nodes: []hcloudNodeJSON{}, Locations: []string{}}
	for _, srv := range servers {
		if l := srv.Location.Name; l != "" && !slices.Contains(out.Locations, l) {
			out.Locations = append(out.Locations, l)
		}
	}
	slices.Sort(out.Locations)
	var nodes corev1.NodeList
	if err := s.list(ctx, c, &nodes); err != nil {
		return out, "", nil // the check of the token itself passed
	}
	for _, n := range controllers.MatchCloudNodes(nodes.Items, servers, clusters.Local) {
		out.Nodes = append(out.Nodes, hcloudNodeJSON{Node: n.Node, Server: n.Server.Name, Location: n.Server.Location.Name})
	}
	warning := ""
	cloudNodes := 0
	for _, n := range nodes.Items {
		if n.Labels[controllers.LabelPlatform] == "cloud" {
			cloudNodes++
		}
	}
	if len(out.Nodes) == 0 && cloudNodes > 0 {
		warning = "None of this cluster's servers is in the token's project. Is it the token of the project the servers run in?"
	}
	return out, warning, nil
}

var hcloudTokenRE = regexp.MustCompile(`^[A-Za-z0-9]{32,128}$`)

func (s *settingsAPI) setHCloudToken(w http.ResponseWriter, r *http.Request) {
	if clusterRefused(w, r, true) {
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	token := strings.TrimSpace(req.Token)
	if !hcloudTokenRE.MatchString(token) {
		writeFieldError(w, "token", "That does not look like a Hetzner Cloud API token (64 letters and digits).")
		return
	}
	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	check, warning, err := s.checkHCloudToken(ctx, c, token)
	var rl *hetzner.RateLimitError
	switch {
	case errors.Is(err, hetzner.ErrReadOnly):
		writeFieldError(w, "token", "This token may only read. Create a Read & Write token in the Hetzner Console (Security → API tokens).")
		return
	case errors.Is(err, hetzner.ErrTokenRejected):
		writeFieldError(w, "token", "Hetzner rejected this token. Create one in the Hetzner Console under Security → API tokens, with Read & Write access.")
		return
	case errors.As(err, &rl):
		writeError(w, http.StatusServiceUnavailable, rl.Error()+". Try again then.")
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "Could not reach the Hetzner Cloud API to check the token: "+err.Error())
		return
	}
	cs, err := s.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.hcloud_token", "hetzner-cloud", "Settings not found.", err)
		return
	}
	if cs == nil {
		// The Domain reconciler creates the token's Secret once settings exist.
		if err := s.patchSettings(ctx, c, nil, map[string]any{}); err != nil {
			s.kubeError(w, r, p, "settings.hcloud_token", "hetzner-cloud", "Settings not found.", err)
			return
		}
		if cs, err = s.load(ctx, c); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if err := s.writeSecretKey(ctx, c, controllers.HCloudTokenSecret, controllers.HCloudTokenKey, token); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusServiceUnavailable, "The console is still preparing the token's storage. Try again in a moment.")
			return
		}
		s.kubeError(w, r, p, "settings.hcloud_token", controllers.HCloudTokenSecret, "Token storage not found.", err)
		return
	}
	if err := s.markHCloudToken(ctx, c, cs); err != nil {
		s.kubeError(w, r, p, "settings.hcloud_token", "hetzner-cloud", "Settings not found.", err)
		return
	}
	s.audit(r, p.user.Email, "settings.hcloud_token", "hetzner-cloud", "token replaced")
	cs, err = s.load(ctx, c)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := map[string]any{"settings": s.view(cs), "check": check}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(w, http.StatusOK, out)
}

// removeHCloudToken empties the token: Kwerft stops managing the Cloud
// Firewall and the Load Balancer, which stay in the project as they are.
// The CSI driver and cloud-controller-manager keep their copy.
func (s *settingsAPI) removeHCloudToken(w http.ResponseWriter, r *http.Request) {
	if clusterRefused(w, r, true) {
		return
	}
	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cs, err := s.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.hcloud_token", "hetzner-cloud", "Settings not found.", err)
		return
	}
	if cs == nil || cs.Annotations[controllers.AnnotationHCloudTokenUpdated] == "" {
		writeJSON(w, http.StatusOK, map[string]any{"settings": s.view(cs)})
		return
	}
	if err := s.writeSecretKey(ctx, c, controllers.HCloudTokenSecret, controllers.HCloudTokenKey, ""); err != nil && !apierrors.IsNotFound(err) {
		s.kubeError(w, r, p, "settings.hcloud_token", controllers.HCloudTokenSecret, "Token storage not found.", err)
		return
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{controllers.AnnotationHCloudTokenUpdated: nil}}}
	if err := s.patchSettings(ctx, c, cs, patch); err != nil {
		s.kubeError(w, r, p, "settings.hcloud_token", "hetzner-cloud", "Settings not found.", err)
		return
	}
	s.audit(r, p.user.Email, "settings.hcloud_token", "hetzner-cloud", "token removed")
	if cs, err = s.load(ctx, c); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.view(cs)})
}

func (s *settingsAPI) markHCloudToken(ctx context.Context, c client.Client, cs *kwerftv1.ConsoleSettings) error {
	return s.patchSettings(ctx, c, cs, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		controllers.AnnotationHCloudTokenUpdated: s.now().UTC().Format(time.RFC3339Nano)}}})
}

// writeSecretKey patches one key of a write-only Secret in kwerft-system as
// the user (their role may patch it, never read it).
func (s *settingsAPI) writeSecretKey(ctx context.Context, c client.Client, name, key, value string) error {
	raw, err := json.Marshal(map[string]any{"data": map[string]string{key: base64.StdEncoding.EncodeToString([]byte(value))}})
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: name}}
	for deadline := time.Now().Add(5 * time.Second); ; {
		err = c.Patch(ctx, sec, client.RawPatch(types.MergePatchType, raw))
		if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---- firewall and Load Balancer --------------------------------------------------

var hcloudNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (s *settingsAPI) setHCloud(w http.ResponseWriter, r *http.Request) {
	if clusterRefused(w, r, false) {
		return
	}
	cluster, _ := hcloudCluster(r)
	var req struct {
		Firewall     string                 `json:"firewall"`
		LoadBalancer hcloudLoadBalancerJSON `json:"loadBalancer"`
	}
	if !decode(w, r, &req) {
		return
	}
	switch kwerftv1.CloudFirewallMode(req.Firewall) {
	case "", kwerftv1.CloudFirewallSync, kwerftv1.CloudFirewallOff:
	default:
		writeFieldError(w, "firewall", "Choose sync or off.")
		return
	}
	lb := req.LoadBalancer
	lb.Type, lb.Location = strings.ToLower(strings.TrimSpace(lb.Type)), strings.ToLower(strings.TrimSpace(lb.Location))
	if lb.Type != "" && !hcloudNameRE.MatchString(lb.Type) {
		writeFieldError(w, "loadBalancer.type", "Enter a Load Balancer type such as lb11.")
		return
	}
	if lb.Location != "" && !hcloudNameRE.MatchString(lb.Location) {
		writeFieldError(w, "loadBalancer.location", "Enter a location such as fsn1, or leave it empty.")
		return
	}
	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cs, err := s.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.hcloud", cluster, "Settings not found.", err)
		return
	}
	if cluster != clusters.Local {
		s.setClusterHCloud(w, r, c, p, cluster, cs, kwerftv1.CloudFirewallMode(req.Firewall), lb)
		return
	}
	view := s.hcloudView(ctx, cs)
	if lb.Enabled && !view.LoadBalancer.Enabled {
		switch {
		case !view.TokenSet:
			writeFieldError(w, "loadBalancer.enabled", "Enter a Hetzner Cloud API token first.")
			return
		case !view.LoadBalancerReady:
			writeFieldError(w, "loadBalancer.enabled", "The ingress does not accept a Load Balancer yet: re-run the installer on a Cloud server with a private network (Hetzner Cloud Network).")
			return
		}
	}
	firewallMode := any(nil)
	if req.Firewall != "" {
		firewallMode = req.Firewall
	}
	patch := map[string]any{"spec": map[string]any{"hetznerCloud": map[string]any{
		"firewall":     firewallMode,
		"loadBalancer": map[string]any{"enabled": lb.Enabled, "type": nilIfEmpty(lb.Type), "location": nilIfEmpty(lb.Location)},
	}}}
	if err := s.patchSettings(ctx, c, cs, patch); err != nil {
		s.kubeError(w, r, p, "settings.hcloud", cluster, "Settings not found.", err)
		return
	}
	s.audit(r, p.user.Email, "settings.hcloud", cluster, hcloudDetail(kwerftv1.CloudFirewallMode(req.Firewall), lb.Enabled))
	if cs, err = s.load(ctx, c); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(cs))
}

func hcloudDetail(firewall kwerftv1.CloudFirewallMode, lb bool) string {
	detail := "firewall " + string(cmp.Or(firewall, kwerftv1.CloudFirewallSync))
	if lb {
		return detail + ", load balancer on"
	}
	return detail + ", load balancer off"
}

// setClusterHCloud writes a remote hetzner-cloud cluster's Cloud settings
// to its Cluster object, as the signed-in user, and answers the cluster's
// view of them.
func (s *settingsAPI) setClusterHCloud(w http.ResponseWriter, r *http.Request, c client.Client, p *principal, cluster string,
	cs *kwerftv1.ConsoleSettings, firewall kwerftv1.CloudFirewallMode, lb hcloudLoadBalancerJSON) {
	ctx := r.Context()
	var cl kwerftv1.Cluster
	if err := c.Get(ctx, client.ObjectKey{Name: cluster}, &cl); err != nil {
		s.kubeError(w, r, p, "settings.hcloud", cluster, "Cluster not found.", err)
		return
	}
	h := cl.Spec.HetznerCloud
	if cl.Spec.Provider != kwerftv1.ClusterHetznerCloud || h == nil {
		writeError(w, http.StatusBadRequest, "Only Hetzner Cloud clusters Kwerft created get their Cloud Firewall and Load Balancer from the console.")
		return
	}
	tokenSet := cs != nil && cs.Annotations[controllers.AnnotationHCloudTokenUpdated] != ""
	if lb.Enabled && (h.LoadBalancer == nil || !h.LoadBalancer.Enabled) && !tokenSet {
		writeFieldError(w, "loadBalancer.enabled", "Enter a Hetzner Cloud API token in Settings first.")
		return
	}
	orig := cl.DeepCopy()
	h.Firewall = firewall
	h.LoadBalancer = &kwerftv1.LoadBalancerSettings{Enabled: lb.Enabled, Type: lb.Type, Location: lb.Location}
	if !lb.Enabled && lb.Type == "" && lb.Location == "" {
		h.LoadBalancer = nil
	}
	if err := c.Patch(ctx, &cl, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		s.kubeError(w, r, p, "settings.hcloud", cluster, "Cluster not found.", err)
		return
	}
	s.audit(r, p.user.Email, "settings.hcloud", cluster, hcloudDetail(firewall, lb.Enabled))
	writeJSON(w, http.StatusOK, map[string]any{"cloud": clusterCloudView(&cl)})
}

// clusterCloudJSON is a remote hetzner-cloud cluster's Cloud Firewall and
// Load Balancer: its settings and what its own reconciler reported.
type clusterCloudJSON struct {
	Firewall     string                       `json:"firewall"` // sync | off
	LoadBalancer hcloudLoadBalancerJSON       `json:"loadBalancer"`
	Status       *kwerftv1.HetznerCloudStatus `json:"status,omitempty"`
}

func clusterCloudView(cl *kwerftv1.Cluster) *clusterCloudJSON {
	h := cl.Spec.HetznerCloud
	if cl.Spec.Provider != kwerftv1.ClusterHetznerCloud || h == nil {
		return nil
	}
	out := &clusterCloudJSON{Firewall: string(cmp.Or(h.Firewall, kwerftv1.CloudFirewallSync)), Status: cl.Status.HetznerCloud}
	if lb := h.LoadBalancer; lb != nil {
		out.LoadBalancer = hcloudLoadBalancerJSON{Enabled: lb.Enabled, Type: lb.Type, Location: lb.Location}
	}
	return out
}

// ---- volume classes --------------------------------------------------------------

type volumeClassJSON struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// volumeClasses tells the Volumes page which classes this cluster has.
func (s *settingsAPI) volumeClasses(w http.ResponseWriter, r *http.Request) {
	out := []volumeClassJSON{{ID: "local-nvme", Available: true}}
	cloud := volumeClassJSON{ID: "hcloud-volume", Available: s.storageClassExists(r.Context(), HCloudVolumesClass)}
	if !cloud.Available {
		cloud.Reason = "Hetzner Cloud Volumes are not set up on this cluster: enter a Hetzner Cloud API token under Settings and re-run the installer on the Cloud server."
		if s.cfg.Platform == "dedicated" {
			cloud.Reason = "Hetzner Cloud Volumes need Hetzner Cloud servers; this cluster runs on dedicated servers."
		}
	}
	writeJSON(w, http.StatusOK, append(out, cloud))
}
