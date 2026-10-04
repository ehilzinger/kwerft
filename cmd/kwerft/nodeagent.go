package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/firewall"
	"github.com/ehilzinger/kwerft/internal/version"
)

// runNodeAgent is `kwerft node-agent`: the DaemonSet (charts/kwerft
// templates/node-agent.yaml) that applies this node's firewall rules. It
// needs the host's network namespace, CAP_NET_ADMIN, and the host's nft:
// the image has none, so nft runs from the host's /usr, mounted read-only
// under --host-root, in a chroot there (CAP_SYS_CHROOT). Its Kubernetes
// access is two ConfigMaps (read the desired rules, patch its status).
func runNodeAgent(args []string) int {
	fs := flag.NewFlagSet("node-agent", flag.ExitOnError)
	node := fs.String("node", os.Getenv("NODE_NAME"), "this node's name (the DaemonSet sets NODE_NAME)")
	namespace := fs.String("namespace", envOr("POD_NAMESPACE", firewall.Namespace), "namespace of the firewall ConfigMaps")
	hostRoot := fs.String("host-root", "/host", "directory with the host's /usr (and /etc/ld.so.cache); nft runs chrooted there. \"/\" runs nft directly")
	stateDir := fs.String("state-dir", "/var/lib/kwerft/firewall", "host directory for the agent's state (and the paused marker of install.sh --reset-firewall)")
	poll := fs.Duration("poll", 2*time.Second, "how often to read the desired rules")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("node", *node)
	if *node == "" {
		log.Error("--node (or NODE_NAME) is required")
		return 2
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		log.Error("cannot create the state directory", "dir", *stateDir, "err", err)
		return 1
	}
	if err := prepareChroot(*hostRoot); err != nil {
		log.Error("cannot prepare the chroot for the host's nft", "err", err)
		return 1
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error("no cluster configuration", "err", err)
		return 1
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error("cannot create the Kubernetes client", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	na := &nodeAgent{
		c: c, namespace: *namespace, node: *node, log: log,
		agent: &firewall.Agent{Node: *node, NFT: hostNFT{root: *hostRoot}, Dir: *stateDir, Log: log, Version: version.Version},
	}
	log.Info("kwerft node agent starting", "version", version.Version, "hostRoot", *hostRoot, "stateDir", *stateDir)
	// The state machine ticks every second (a pending change rolls back on
	// time); the desired rules are read every poll.
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastFetch time.Time
	for {
		fetch := time.Since(lastFetch) >= *poll
		if fetch {
			lastFetch = time.Now()
		}
		na.tick(ctx, fetch)
		select {
		case <-ctx.Done():
			// Nothing to undo: the state on the host lets the next agent
			// continue, including a pending change's rollback.
			log.Info("kwerft node agent stopped")
			return 0
		case <-t.C:
		}
	}
}

// nodeAgent connects the state machine to Kubernetes.
type nodeAgent struct {
	c         client.Client
	namespace string
	node      string
	agent     *firewall.Agent
	log       *slog.Logger

	desired    *firewall.Desired
	lastReport firewall.NodeStatus
	reportedAt time.Time
}

// heartbeat: the agent reports at least this often, so the console can tell
// a running agent from a gone one.
const heartbeat = 30 * time.Second

func (na *nodeAgent) tick(ctx context.Context, fetch bool) {
	var d *firewall.Desired
	if fetch {
		got, err := na.readDesired(ctx)
		if err != nil {
			na.log.Warn("cannot read the desired firewall rules; keeping time only", "err", err)
		}
		d = got
	}
	st := na.agent.Step(ctx, d)
	if d == nil && na.lastReport.Seen != "" {
		st.Seen = na.lastReport.Seen // nothing new read: still the last revision seen
	}
	if d != nil {
		na.desired = d
	}
	changed := st.State != na.lastReport.State || st.Seen != na.lastReport.Seen || st.Confirmed != na.lastReport.Confirmed ||
		st.Pending != na.lastReport.Pending || st.RolledBack != na.lastReport.RolledBack || st.Message != na.lastReport.Message
	if !changed && time.Since(na.reportedAt) < heartbeat {
		return
	}
	if err := na.report(ctx, st); err != nil {
		na.log.Warn("cannot report the firewall status", "err", err)
		return
	}
	if changed {
		na.log.Info("firewall status", "state", st.State, "revision", st.Seen, "confirmed", st.Confirmed, "pending", st.Pending, "message", st.Message)
	}
	na.lastReport, na.reportedAt = st, time.Now()
}

func (na *nodeAgent) readDesired(ctx context.Context) (*firewall.Desired, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var cm corev1.ConfigMap
	err := na.c.Get(ctx, client.ObjectKey{Namespace: na.namespace, Name: firewall.DesiredConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return nil, nil // no rules rendered yet
	}
	if err != nil {
		return nil, err
	}
	var d firewall.Desired
	if err := json.Unmarshal([]byte(cm.Data[firewall.DesiredKey]), &d); err != nil {
		return nil, fmt.Errorf("desired rules: %w", err)
	}
	if d.Revision == "" {
		return nil, nil
	}
	return &d, nil
}

func (na *nodeAgent) report(ctx context.Context, st firewall.NodeStatus) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{na.node: string(raw)}})
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: na.namespace, Name: firewall.StatusConfigMap}}
	return na.c.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch))
}

// hostNFT runs the host's nft, chrooted into root (the host's /usr mounted
// there), so the distroless image needs no nft of its own and the rules are
// read and written by the same nft the installer used.
type hostNFT struct{ root string }

func (h hostNFT) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/nft", args...)
	if h.root != "" && h.root != "/" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: h.root}
	}
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin", "LC_ALL=C"}
	cmd.Stdin = strings.NewReader(stdin)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		if msg == "" {
			return out.String(), err
		}
		return out.String(), fmt.Errorf("%w: %s", err, msg)
	}
	return out.String(), nil
}

// prepareChroot gives the host's /usr, mounted at root/usr, the merged-/usr
// links (lib, lib64, bin, sbin) the dynamic loader expects at the top.
func prepareChroot(root string) error {
	if root == "" || root == "/" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, "usr", "sbin", "nft")); err != nil {
		// Reported by the agent as an unprepared host; nothing to link.
		return nil
	}
	for _, d := range []string{"lib", "lib64", "bin", "sbin"} {
		if _, err := os.Stat(filepath.Join(root, "usr", d)); err != nil {
			continue
		}
		link := filepath.Join(root, d)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join("usr", d), link); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
