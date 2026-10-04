// Package kube gives the console Kubernetes clients that act as the signed-in
// user. Kubernetes RBAC, not the console, is the final gate for everything a
// user does: the console's own service account only holds the right to
// impersonate.
package kube

import (
	"fmt"
	"net/http"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
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

	mu      sync.Mutex
	clients map[identity]client.Client
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
	return &Impersonator{cfg: rest.CopyConfig(cfg), http: httpClient, mapper: mapper, scheme: scheme, clients: map[identity]client.Client{}}, nil
}

// For returns a client acting as the console user with this email and role.
func (i *Impersonator) For(email, role string) (client.Client, error) {
	if email == "" {
		return nil, fmt.Errorf("impersonate: empty user")
	}
	known := false
	for _, r := range Roles {
		known = known || r == role
	}
	if !known {
		return nil, fmt.Errorf("impersonate: unknown role %q", role)
	}

	id := identity{email, role}
	i.mu.Lock()
	defer i.mu.Unlock()
	if c, ok := i.clients[id]; ok {
		return c, nil
	}
	base := i.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc := &http.Client{
		Transport: transport.NewImpersonatingRoundTripper(transport.ImpersonationConfig{
			UserName: UserName(email),
			Groups:   []string{RoleGroup(role), Authenticated},
		}, base),
		Timeout: i.http.Timeout,
	}
	c, err := client.New(i.cfg, client.Options{HTTPClient: hc, Mapper: i.mapper, Scheme: i.scheme})
	if err != nil {
		return nil, err
	}
	if len(i.clients) >= maxCached {
		i.clients = map[identity]client.Client{}
	}
	i.clients[id] = c
	return c, nil
}
