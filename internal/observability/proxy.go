// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package observability

import (
	"fmt"
	"net/url"
	"strings"
)

// ServiceProxyURL turns one of the in-cluster URLs above into the same
// service reached through a Kubernetes API server's service proxy:
// <apiServer>/api/v1/namespaces/<ns>/services/<scheme>:<name>:<port>/proxy.
// The console uses it for remote clusters (docs/phase5.md), whose
// observability stack it reaches only through their API server (the agent
// tunnel); the identity is Kwerft's own there, which needs get and create
// on services/proxy in Namespace.
func ServiceProxyURL(apiServer, serviceURL string) (string, error) {
	u, err := url.Parse(serviceURL)
	if err != nil {
		return "", err
	}
	// <name>.<namespace>.svc[.cluster.local]
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) < 3 || parts[2] != "svc" || u.Port() == "" {
		return "", fmt.Errorf("observability: %q is not an in-cluster service URL", serviceURL)
	}
	base := strings.TrimRight(apiServer, "/")
	return fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s:%s:%s/proxy%s",
		base, parts[1], u.Scheme, parts[0], u.Port(), strings.TrimRight(u.Path, "/")), nil
}
