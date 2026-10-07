// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package kube gives the console Kubernetes clients that act as the signed-in
// user. Kubernetes RBAC, not the console, is the final gate for everything a
// user does: the console's own service account only holds the right to
// impersonate.
package kube

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// Identity prefixes. A user signed in as mara@example.com with the developer
// role reaches the API server as user "kwerft:mara@example.com" in groups
// "kwerft:role:developer" and "system:authenticated". The chart binds the
// role groups to the kwerft:<role> ClusterRoles.
const (
	UserPrefix  = "kwerft:"
	GroupPrefix = "kwerft:role:"
	// Authenticated is added to every impersonated identity, as the API
	// server does for real users, so default discovery and self-review
	// permissions keep working.
	Authenticated = "system:authenticated"
)

// Roles the console knows. Anything else is refused rather than forwarded,
// so a corrupt role can never become an arbitrary group.
var Roles = []string{"owner", "admin", "developer", "viewer"}

// UserName is the Kubernetes user name for a console account.
func UserName(email string) string { return UserPrefix + email }

// RoleGroup is the Kubernetes group for a console role.
func RoleGroup(role string) string { return GroupPrefix + role }

// maxCached bounds the per-identity client cache. Clients are cheap to
// rebuild, so on overflow the cache simply starts over.
const maxCached = 256

// Impersonator builds controller-runtime clients that impersonate console
// users. Every client shares one HTTP transport (connection pool, TLS
// session, service-account credentials) and one RESTMapper; only a thin
// round tripper that adds the Impersonate-* headers differs per identity.
type Impersonator struct {
	cfg    *rest.Config
	http   *http.Client
	mapper meta.RESTMapper
	scheme *runtime.Scheme

	mu         sync.Mutex
	clients    map[identity]client.Client
	clientsets map[identity]kubernetes.Interface
	self       kubernetes.Interface // see Self
}

type identity struct{ email, role string }

// NewImpersonator returns an Impersonator for the console's own credentials
// in cfg. httpClient and mapper may be nil; pass the manager's to share them
// with the reconcilers.
func NewImpersonator(cfg *rest.Config, httpClient *http.Client, mapper meta.RESTMapper, scheme *runtime.Scheme) (*Impersonator, error) {
	if cfg.Impersonate.UserName != "" || len(cfg.Impersonate.Groups) > 0 {
		return nil, fmt.Errorf("base config must not impersonate")
	}
	var err error
	if httpClient == nil {
		if httpClient, err = rest.HTTPClientFor(cfg); err != nil {
			return nil, err
		}
	}
	if mapper == nil {
		if mapper, err = apiutil.NewDynamicRESTMapper(cfg, httpClient); err != nil {
			return nil, err
		}
	}
	return &Impersonator{cfg: rest.CopyConfig(cfg), http: httpClient, mapper: mapper, scheme: scheme,
		clients: map[identity]client.Client{}, clientsets: map[identity]kubernetes.Interface{}}, nil
}

func checkIdentity(email, role string) error {
	if email == "" {
		return fmt.Errorf("impersonate: empty user")
	}
	for _, r := range Roles {
		if r == role {
			return nil
		}
	}
	return fmt.Errorf("impersonate: unknown role %q", role)
}

func impersonation(email, role string) transport.ImpersonationConfig {
	return transport.ImpersonationConfig{
		UserName: UserName(email),
		Groups:   []string{RoleGroup(role), Authenticated},
	}
}

// httpClient is the shared transport with the user's Impersonate-* headers.
// timeout 0 suits long-lived streams (logs), which their context bounds.
func (i *Impersonator) httpClient(email, role string, timeout time.Duration) *http.Client {
	base := i.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{Transport: transport.NewImpersonatingRoundTripper(impersonation(email, role), base), Timeout: timeout}
}

// For returns a client acting as the console user with this email and role.
func (i *Impersonator) For(email, role string) (client.Client, error) {
	if err := checkIdentity(email, role); err != nil {
		return nil, err
	}
	id := identity{email, role}
	i.mu.Lock()
	defer i.mu.Unlock()
	if c, ok := i.clients[id]; ok {
		return c, nil
	}
	c, err := client.New(i.cfg, client.Options{HTTPClient: i.httpClient(email, role, i.http.Timeout), Mapper: i.mapper, Scheme: i.scheme})
	if err != nil {
		return nil, err
	}
	if len(i.clients) >= maxCached {
		i.clients = map[identity]client.Client{}
	}
	i.clients[id] = c
	return c, nil
}

// Clientset returns a typed clientset acting as the user, for what the
// controller-runtime client cannot do: log streams, subresource URLs and raw
// requests to aggregated APIs such as metrics.k8s.io. Its HTTP client has no
// overall timeout, so streams last as long as their context.
func (i *Impersonator) Clientset(email, role string) (kubernetes.Interface, error) {
	if err := checkIdentity(email, role); err != nil {
		return nil, err
	}
	id := identity{email, role}
	i.mu.Lock()
	defer i.mu.Unlock()
	if cs, ok := i.clientsets[id]; ok {
		return cs, nil
	}
	cs, err := kubernetes.NewForConfigAndClient(i.cfg, i.httpClient(email, role, 0))
	if err != nil {
		return nil, err
	}
	if len(i.clientsets) >= maxCached {
		i.clientsets = map[identity]kubernetes.Interface{}
	}
	i.clientsets[id] = cs
	return cs, nil
}

// RESTConfig returns a copy of the console's config that impersonates the
// user, for code that builds its own connections (exec over WebSocket or
// SPDY). Every request made with it carries the user's identity.
func (i *Impersonator) RESTConfig(email, role string) (*rest.Config, error) {
	if err := checkIdentity(email, role); err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(i.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: UserName(email), Groups: []string{RoleGroup(role), Authenticated}}
	return cfg, nil
}

// Upstream returns the API server's URL and a round tripper that sends
// requests with the console's credentials and the user's Impersonate-*
// headers, for the Kubernetes proxy (internal/server/kubeproxy.go). Callers
// must remove every Impersonate-* and Authorization header a client sent:
// the round trippers leave a request alone that already carries them.
func (i *Impersonator) Upstream(email, role string) (*url.URL, http.RoundTripper, error) {
	if err := checkIdentity(email, role); err != nil {
		return nil, nil, err
	}
	host, _, err := rest.DefaultServerUrlFor(i.cfg)
	if err != nil {
		return nil, nil, err
	}
	base := i.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return host, transport.NewImpersonatingRoundTripper(impersonation(email, role), base), nil
}

// Self returns a clientset with the console's own identity, without
// impersonation. It exists for reads users may not make themselves but are
// entitled to see once Kubernetes has authorised an impersonated check: the
// build pod's log in the kwerft-builds namespace, after the user's own get of
// the Build succeeded (internal/server/api_builds.go). Never use it for a
// write a user asks for.
func (i *Impersonator) Self() (kubernetes.Interface, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.self != nil {
		return i.self, nil
	}
	base := i.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	// No overall timeout: log streams last as long as their context.
	cs, err := kubernetes.NewForConfigAndClient(i.cfg, &http.Client{Transport: base})
	if err != nil {
		return nil, err
	}
	i.self = cs
	return cs, nil
}
