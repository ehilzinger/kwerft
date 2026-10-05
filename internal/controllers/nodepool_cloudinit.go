package controllers

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Cloud-init for node pool servers. The user data is a small script that
// waits for the private network, downloads the installer from the console
// (the release's copy on GitHub when the console does not serve one) and
// runs it:
//
//   - joining:   install.sh --join <console> --token <kwft_join_…> --role worker|control-plane
//                --platform cloud [--k3s-version <v>] [--node-label k=v]… [--node-taint k=v:Effect]… --yes
//   - bootstrap: install.sh --agent --console <console> --cluster-token <token>
//                --platform cloud [--version <v>] [--k3s-version <v>] [--node-label k=v]… --yes
//                (the first control-plane server of a new hetzner-cloud
//                cluster; agent mode is W3's, docs/phase5.md › Installer flags)
//
// The join token is single-use in effect (bound to this server's name) and
// expires; the user data never holds a k3s token. The bootstrap variant
// carries the cluster's bootstrap token from Secret cluster-<name>-agent,
// which W3 deletes once the agent first connected.
//
// --k3s-version is the cluster's running Kubernetes version
// (Cluster.status.kubernetesVersion) when it is known: a node joining after a
// k3s upgrade installs what the cluster runs, not the installer's pin.

// joinScript are the parameters of a server's user data.
type joinScript struct {
	Console     string // https://<console>
	FallbackURL string // the release's install.sh, used when the console serves none
	Mode        string // "join" or "agent"
	Token       string
	// Version is the console's release (agent mode installs the same); ""
	// for development builds.
	Version string
	// K3sVersion is the k3s version the cluster runs (v1.37.1+k3s1); ""
	// installs the installer's pinned version.
	K3sVersion string
	Role       string // join: worker | control-plane
	Labels     map[string]string
	Taints     []string
	Cluster    string
	Pool       string
}

// k3sVersionRE is what the installer's --k3s-version accepts.
var k3sVersionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?\+k3s[0-9]+$`)

// k3sVersion is the version a new node of a cluster reporting running
// should install: running itself when it is a k3s release, else "" (the
// installer's pin).
func k3sVersion(running string) string {
	if k3sVersionRE.MatchString(running) {
		return running
	}
	return ""
}

// shellQuote quotes s for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func (j joinScript) args() []string {
	var args []string
	switch j.Mode {
	case "agent":
		// --await-cloud-token: the console hands the cluster its Cloud API
		// token once the agent connects; then the installer adds the CSI
		// driver (cluster_hcloud.go).
		args = []string{"--agent", "--console", j.Console, "--cluster-token", j.Token, "--platform", "cloud", "--await-cloud-token"}
		if j.Version != "" {
			args = append(args, "--version", j.Version)
		}
	default:
		args = []string{"--join", j.Console, "--token", j.Token, "--role", j.Role, "--platform", "cloud"}
	}
	if j.K3sVersion != "" {
		args = append(args, "--k3s-version", j.K3sVersion)
	}
	keys := make([]string, 0, len(j.Labels))
	for k := range j.Labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--node-label", k+"="+j.Labels[k])
	}
	for _, t := range j.Taints {
		args = append(args, "--node-taint", t)
	}
	return append(args, "--yes")
}

// userData renders the cloud-config.
func (j joinScript) userData() string {
	quoted := make([]string, 0, 16)
	for _, a := range j.args() {
		quoted = append(quoted, shellQuote(a))
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "# Kwerft node pool %s of cluster %s.\n", j.Pool, j.Cluster)
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /root/kwerft-join.sh\n")
	b.WriteString("    permissions: \"0700\"\n")
	b.WriteString("    content: |\n")
	script := fmt.Sprintf(`#!/bin/bash
# Written by Kwerft: joins this server to its cluster, then deletes itself.
set -uo pipefail
exec >>/var/log/kwerft-join.log 2>&1
console=%s
fallback=%s
# The Cloud Network is attached at creation; wait until it has an address.
for _ in $(seq 1 90); do
  def=$(ip -4 route show default | awk '{print $5; exit}')
  ip -4 -o addr show scope global | awk -v d="$def" '$2 != d {print $4}' \
    | grep -qE '^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)' && break
  sleep 2
done
installer=$(mktemp)
for attempt in 1 2 3 4 5; do
  if curl -fsSL --retry 5 --retry-delay 5 "$console/install.sh" -o "$installer" \
     || { [ -n "$fallback" ] && curl -fsSL --retry 5 "$fallback" -o "$installer"; }; then
    if bash "$installer" %s; then
      rm -f "$installer" /root/kwerft-join.sh
      exit 0
    fi
  fi
  echo "kwerft: attempt $attempt failed; retrying in 30 s"
  sleep 30
done
exit 1
`, shellQuote(j.Console), shellQuote(j.FallbackURL), strings.Join(quoted, " "))
	for _, line := range strings.Split(strings.TrimSuffix(script, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("      " + line + "\n")
	}
	b.WriteString("runcmd:\n")
	b.WriteString("  - [bash, /root/kwerft-join.sh]\n")
	return b.String()
}
