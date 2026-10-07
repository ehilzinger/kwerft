// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ehilzinger/kwerft/internal/firewall"
)

// recordingNFT answers like a prepared host and remembers applied scripts.
type recordingNFT struct{ applied []string }

func (r *recordingNFT) Run(_ context.Context, stdin string, args ...string) (string, error) {
	switch args[0] {
	case "list":
		return "table inet kwerft {\n\tchain managed_ssh {\n\t}\n\tchain managed_open {\n\t}\n\tchain input {\n\t\ttcp dport 22 jump managed_ssh\n\t\tjump managed_open\n\t}\n}\n", nil
	case "--check":
		return "", nil
	case "-f":
		r.applied = append(r.applied, stdin)
		return "", nil
	}
	return "", errors.New("unexpected")
}

func TestNodeAgentReadsDesiredAndReports(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	d := firewall.Desired{Revision: "r1", Confirmed: "r1", Nodes: map[string]firewall.NodeRules{
		"n1": {Open: []firewall.Open{{Rule: "web", Protocol: "tcp", Port: 8080}}},
	}}
	raw, _ := json.Marshal(d)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap}, Data: map[string]string{firewall.DesiredKey: string(raw)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: firewall.Namespace, Name: firewall.StatusConfigMap}, Data: map[string]string{"n2": "{}"}},
	).Build()
	nft := &recordingNFT{}
	na := &nodeAgent{c: c, namespace: firewall.Namespace, node: "n1", log: slog.New(slog.DiscardHandler),
		agent: &firewall.Agent{Node: "n1", NFT: nft, Dir: t.TempDir()}}
	na.tick(t.Context(), true)

	if len(nft.applied) != 1 || !strings.Contains(nft.applied[0], "tcp dport 8080 accept") {
		t.Fatalf("applied: %q", nft.applied)
	}
	var cm corev1.ConfigMap
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.StatusConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	var st firewall.NodeStatus
	if err := json.Unmarshal([]byte(cm.Data["n1"]), &st); err != nil {
		t.Fatal(err)
	}
	if st.State != firewall.StateInSync || st.Seen != "r1" || st.Confirmed != "r1" {
		t.Fatalf("status: %+v", st)
	}
	if cm.Data["n2"] != "{}" {
		t.Fatal("another node's report was touched")
	}
	// Ticks without reading keep the revision last seen.
	na.tick(t.Context(), false)
	if na.lastReport.Seen != "r1" {
		t.Fatalf("seen: %q", na.lastReport.Seen)
	}
}

func TestPrepareChroot(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"usr/sbin", "usr/lib", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "usr/sbin/nft"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 { // idempotent
		if err := prepareChroot(root); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"lib", "bin", "sbin"} {
		if target, err := os.Readlink(filepath.Join(root, d)); err != nil || target != filepath.Join("usr", d) {
			t.Errorf("%s -> %q (%v)", d, target, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "lib64")); err == nil {
		t.Error("lib64 linked although the host has no usr/lib64")
	}
}
