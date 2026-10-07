// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package observability

import "testing"

func TestServiceProxyURL(t *testing.T) {
	got, err := ServiceProxyURL("https://tunnel.example/", MetricsURL)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://tunnel.example/api/v1/namespaces/kwerft-observability/services/http:vmsingle-vm-victoria-metrics-k8s-stack:8428/proxy"
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if _, err := ServiceProxyURL("https://x", "http://example.com:80"); err == nil {
		t.Error("a non-service URL was accepted")
	}
}
