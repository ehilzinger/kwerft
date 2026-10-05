package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	labelE2E     = "kwerft-e2e"
	labelRun     = "run"
	labelCreated = "created"

	acmeStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

	whoamiImage  = "docker.io/traefik/whoami:v1.11.0"
	busyboxImage = "docker.io/library/busybox:1.37"

	// The Kwerft chart (install.sh KWERFT_CHART_REPO).
	defaultChartRef = "oci://ghcr.io/ehilzinger/charts/kwerft"
	// Where a console looks for releases by default
	// (upgrades.DefaultInstallBaseURL).
	defaultInstallBase = "https://raw.githubusercontent.com/ehilzinger/kwerft-install/main"
)

// Let's Encrypt's staging roots: joined nodes fetch their join material
// from the console over HTTPS with curl, which must trust the staging
// certificate of the e2e console
// (https://letsencrypt.org/docs/staging-environment/).
var defaultStagingRoots = []string{
	"https://letsencrypt.org/certs/staging/letsencrypt-stg-root-x1.pem",
	"https://letsencrypt.org/certs/staging/letsencrypt-stg-root-x2.pem",
}

// Exit codes of install.sh (its public contract) by name, for messages.
var installerExit = map[int]string{2: "usage", 10: "preflight", 20: "network/DNS", 30: "Kubernetes", 40: "platform", 50: "Kwerft", 60: "restore"}

// What a run does.
const (
	modeFresh   = "fresh"   // install -version
	modeUpgrade = "upgrade" // install -from, re-run the installer of -version
	modeConsole = "console" // install -from, upgrade through the console's API
	modeFault   = "fault"   // as console, with a failure injected after the Kwerft stage
	modeK3s     = "k3s"     // 1 + workers nodes, a Kubernetes upgrade through the console
	modeRestore = "restore" // back up, delete the server, install.sh --restore on a new one
)

type config struct {
	Token   string
	APIBase string

	Version string // under test, without "v"
	From    string // previous release for the upgrade paths; "" = fresh install
	RunID   string

	ViaConsole bool // upgrade From → Version through POST /api/v1/upgrades
	Fault      bool // ViaConsole with the runner's fault injection: expect RolledBack
	K3s        bool // a Kubernetes upgrade on 1 + Workers nodes
	K3sFrom    string
	K3sTo      string
	Workers    int
	Restore    bool // the backup exit criterion
	S3         s3Config

	ServerTypes []string
	Locations   []string
	Images      []string

	// InstallerURL has {version} replaced by the version to install.
	InstallerURL string
	GitRepo      string
	GitBranch    string
	ChartRef     string
	StagingRoots []string

	Timeout        time.Duration // the whole run, cleanup excluded
	InstallTimeout time.Duration // one installer run
	Poll           time.Duration // between checks while waiting
	ProbeEvery     time.Duration // between requests to an App during an upgrade
	MaxGap         time.Duration // fail when an App was away longer; 0 only reports
}

func (c config) installerURL(version string) string {
	return strings.ReplaceAll(c.InstallerURL, "{version}", version)
}

// installBase is the install repository's base URL the console reads
// releases from, as the installer URL implies; "" when it cannot tell.
func (c config) installBase() string {
	base, ok := strings.CutSuffix(c.InstallerURL, "/v{version}/install.sh")
	if !ok {
		return ""
	}
	return base
}

func (c config) mode() string {
	switch {
	case c.Restore:
		return modeRestore
	case c.K3s:
		return modeK3s
	case c.Fault:
		return modeFault
	case c.ViaConsole:
		return modeConsole
	case c.From != "":
		return modeUpgrade
	}
	return modeFresh
}

func (c config) scenario() string {
	switch c.mode() {
	case modeUpgrade:
		return fmt.Sprintf("Upgrade from v%s to v%s", c.From, c.Version)
	case modeConsole:
		return fmt.Sprintf("Upgrade from v%s to v%s through the console", c.From, c.Version)
	case modeFault:
		return fmt.Sprintf("Upgrade from v%s to v%s through the console with a failure injected after the Kwerft stage", c.From, c.Version)
	case modeK3s:
		return fmt.Sprintf("Kubernetes upgrade through the console on %d nodes with v%s (k3s %s → %s)", c.Workers+1, c.Version,
			cmpOr(c.K3sFrom, "one patch behind the pin"), cmpOr(c.K3sTo, "the release's pin"))
	case modeRestore:
		return fmt.Sprintf("Backup of v%s restored onto a new server", c.Version)
	}
	return fmt.Sprintf("Fresh install of v%s", c.Version)
}

func (c config) serverName() string { return "kwerft-e2e-" + c.RunID }

func (c config) workerName(i int) string { return fmt.Sprintf("%s-w%d", c.serverName(), i) }

// restoredName is the server the restore run installs from the backup.
func (c config) restoredName() string { return c.serverName() + "-b" }

var k3sVersionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?\+k3s[0-9]+$`)

func (c config) validate() error {
	switch {
	case c.Version == "":
		return errors.New("-version is required")
	case !validVersion(c.Version):
		return fmt.Errorf("-version %q is not a release version like 0.5.0", c.Version)
	case c.From != "" && !validVersion(c.From):
		return fmt.Errorf("-from %q is not a release version like 0.4.0", c.From)
	case c.From != "" && compareVersions(c.From, c.Version) >= 0:
		return fmt.Errorf("-from %s must be older than -version %s", c.From, c.Version)
	case !validLabelValue(c.RunID) || len(c.serverName()) > 63:
		return fmt.Errorf("-run-id %q must be a short label value (letters, digits, -, _ and .)", c.RunID)
	case len(c.ServerTypes) == 0 || len(c.Locations) == 0 || len(c.Images) == 0:
		return errors.New("-server-types, -locations and -images must not be empty")
	case (c.ViaConsole || c.Fault) && c.From == "":
		return errors.New("-via-console and -fault upgrade from the release -from: give it")
	case c.K3s && (c.From != "" || c.Restore):
		return errors.New("-k3s installs -version fresh: it does not combine with -from, -via-console, -fault or -restore")
	case c.Restore && c.From != "":
		return errors.New("-restore installs -version fresh: it does not combine with -from, -via-console or -fault")
	case !c.K3s && (c.K3sFrom != "" || c.K3sTo != ""):
		return errors.New("-k3s-from and -k3s-to need -k3s")
	case c.K3s && (c.Workers < 1 || c.Workers > 5):
		return fmt.Errorf("-workers %d: from 1 to 5", c.Workers)
	case c.K3sFrom != "" && !k3sVersionRE.MatchString(c.K3sFrom):
		return fmt.Errorf("-k3s-from %q is not a k3s version like v1.37.0+k3s1", c.K3sFrom)
	case c.K3sTo != "" && !k3sVersionRE.MatchString(c.K3sTo):
		return fmt.Errorf("-k3s-to %q is not a k3s version like v1.37.1+k3s1", c.K3sTo)
	case c.K3s && len(c.workerName(c.Workers)) > 63, c.Restore && len(c.restoredName()) > 63:
		return fmt.Errorf("-run-id %q is too long for this run's server names", c.RunID)
	case c.Restore && (!strings.HasPrefix(c.S3.Endpoint, "https://") || c.S3.Bucket == ""):
		return errors.New("-restore needs the bucket: -s3-endpoint https://… and -s3-bucket (E2E_S3_ENDPOINT, E2E_S3_BUCKET)")
	}
	return nil
}

func validLabelValue(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || i == len(s)-1 || !strings.ContainsRune("-_.", r)) {
			return false
		}
	}
	return true
}

// plan describes what a run does, for -dry-run and the log.
func (c config) plan() []string {
	servers := fmt.Sprintf("Create server %q", c.serverName())
	if c.K3s {
		servers = fmt.Sprintf("Create a Cloud Network (10.0.0.0/16) and servers %q and %q … %q in it", c.serverName(), c.workerName(1), c.workerName(c.Workers))
	}
	steps := []string{
		fmt.Sprintf("Generate an ed25519 SSH key and host key for this run; upload the public key as %q with labels %s=true, %s=%s, %s=<unix time>.", c.serverName(), labelE2E, labelRun, c.RunID, labelCreated),
		fmt.Sprintf("%s: the first available of %s in %s, image %s (first that exists), same labels; cloud-init only installs the pinned host key.", servers, strings.Join(c.ServerTypes, ", "), strings.Join(c.Locations, ", "), strings.Join(c.Images, " or ")),
		"Wait for SSH (root, pinned host key) and for cloud-init to finish.",
	}
	install := func(v, what string) string {
		return fmt.Sprintf("%s: download %s on the server and run it with --domain <ip>.sslip.io --acme-server staging --version %s --yes (an installer without --acme-server gets its ClusterIssuer switched to staging right after); expect exit 0.", what, c.installerURL(v), v)
	}
	fresh := func() {
		steps = append(steps,
			install(c.Version, "Install v"+c.Version),
			"Console on HTTPS (Let's Encrypt staging certificate), /healthz, version v"+c.Version+", setup token → owner, sign in.",
		)
	}
	before := "Console on HTTPS (Let's Encrypt staging certificate), setup token → owner, sign in; project e2e-before with an image App on hello.<ip>.sslip.io answering over HTTPS."
	switch c.mode() {
	case modeUpgrade:
		steps = append(steps,
			install(c.From, "Install v"+c.From), before,
			install(c.Version, "Upgrade to v"+c.Version),
			"The console reports v"+c.Version+", the owner still signs in, hello.<ip>.sslip.io still answers.",
		)
	case modeConsole, modeFault:
		steps = append(steps, install(c.From, "Install v"+c.From), before)
		values := []string{}
		if c.Fault {
			values = append(values, "e2e.faults=true")
		}
		if b := c.installBase(); b != "" && b != defaultInstallBase {
			values = append(values, "upgrades.installBaseURL="+b)
		}
		if len(values) > 0 {
			steps = append(steps, fmt.Sprintf("helm upgrade kwerft %s --version %s --reuse-values --set %s; the console Deployment must carry the flags.", c.ChartRef, c.From, strings.Join(values, ",")))
		}
		upgrade := "POST /api/v1/upgrades {component: Kwerft, version: " + c.Version + "} with the owner's password (the console's preflight must pass); follow /api/v1/upgrades/<name>/events across the console's restart, while hello.<ip>.sslip.io is requested every few seconds (the longest gap is reported)."
		if c.Fault {
			steps = append(steps, upgrade+" Annotate the Upgrade kwerft.dev/e2e-fault=install and make sure its runner Job carries --fault=install (re-created if it was made before the annotation).",
				"The Upgrade ends RolledBack; the console reports v"+c.From+" again, the owner signs in, hello.<ip>.sslip.io answers with the same pods (untouched).")
		} else {
			steps = append(steps, upgrade, "The Upgrade ends Succeeded; the console reports v"+c.Version+", the owner still signs in, hello.<ip>.sslip.io still answers.")
		}
	case modeK3s:
		from := cmpOr(c.K3sFrom, "one patch behind the release's K3S_VERSION")
		steps = append(steps,
			install(c.Version, "Install v"+c.Version)+" With KWERFT_K3S_VERSION="+from+".",
			"Console on HTTPS, setup token → owner, sign in.",
			fmt.Sprintf("On each of the %d workers: trust Let's Encrypt's staging roots, download the same installer and run it with --join https://<console> --token <a join token from POST /api/v1/clusters/local/join-command> --role worker --yes, KWERFT_K3S_VERSION as above.", c.Workers),
			fmt.Sprintf("GET /api/v1/clusters/local/nodes: %d nodes Ready on k3s %s.", c.Workers+1, from),
			"Project e2e-before with an image App (2 replicas) on hello.<ip>.sslip.io answering over HTTPS.",
			"POST /api/v1/upgrades {component: Kubernetes, version: "+cmpOr(c.K3sTo, "<the pin>")+"} with the password; follow the events: every node goes through its states to Done, one at a time, the control plane first; hello.<ip>.sslip.io is requested throughout (longest gap reported).",
			"Every node reports the target, Ready and schedulable; the owner signs in, hello.<ip>.sslip.io answers.",
		)
	case modeRestore:
		fresh()
		steps = append(steps,
			fmt.Sprintf("Bucket %s at %s: the prefix %s/ must be empty. PUT /api/v1/settings/backups with prefix %s; keep the recovery key from the answer (masked, never printed); wait for the target to be Ready.", c.S3.Bucket, c.S3.Endpoint, c.RunID, c.RunID),
			"Project e2e-restore: Volume data (1Gi) served by App files (busybox httpd) on files.<ip>.sslip.io, a Task writes a marker file into it; SecretSet e2e with a random E2E_SECRET, App secret answers its SHA-256 on secret.<ip>.sslip.io; both checked.",
			"POST /api/v1/backups/plans/cluster/run; wait for the Backup to be Completed.",
			"Delete the server; create "+c.restoredName()+", upload kwerft.yaml with the backups block and the key files (0600, over SSH's stdin), run install.sh --config … --restore latest --version "+c.Version+" --acme-server staging --yes; expect exit 0.",
			"Through the new address with the old names (they still point at the deleted server): the console reports v"+c.Version+", no setup, the owner signs in with the same password, both Apps run, the marker file is back, the secret's hash matches (the value is never printed); the domain and DNS situation is reported.",
		)
	default:
		fresh()
	}
	if c.mode() != modeRestore && c.mode() != modeFault {
		steps = append(steps,
			"Project e2e: image App web ("+whoamiImage+") on web.<ip>.sslip.io answers over HTTPS (budget: 10 min from SSH to here on a fresh install).",
			"A Task prints a marker and restarts web on success; it succeeds and web rolls over.",
			fmt.Sprintf("Git App git builds %s (%s, Dockerfile) with Build now; the build succeeds and git.<ip>.sslip.io answers (budget 3 min).", c.GitRepo, c.GitBranch),
			"Log search finds the Task's marker; the metrics explorer returns memory series for the project.",
			"App crash ("+busyboxImage+", exits 1) raises a crash-looping alert in the console's alerts API (budget 2 min).",
		)
	}
	cleanup := "Always, also on failure, timeout or cancel: delete the server and the SSH key, and wait until the server is gone."
	switch c.mode() {
	case modeK3s:
		cleanup = "Always, also on failure, timeout or cancel: delete the servers, then the network and the SSH key, and wait until the servers are gone."
	case modeRestore:
		cleanup = fmt.Sprintf("Always, also on failure, timeout or cancel: delete the servers, the SSH key and the objects under %s/ in the bucket, and wait until the servers are gone.", c.RunID)
	}
	return append(steps, cleanup)
}

// owner is the console's first account, made up per run.
type owner struct {
	Name, Email, Password string
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

// machine is one server of the run.
type machine struct {
	name     string
	id       int64
	ip       string
	location string
	price    string // gross EUR per hour
	remote   remote
	created  time.Time
	deleted  time.Time // zero while it exists
}

// runner carries out one e2e run.
type runner struct {
	cfg   config
	cloud *hcloud
	dial  dialer
	// transport dials consoles and apps; tests point it at a fake.
	transport func(*certChecker) *http.Client
	// bucketHTTP talks to the restore run's bucket; nil: a default client.
	bucketHTTP *http.Client
	log        io.Writer
	now        func() time.Time
	mask       func(string) // hides a secret in CI logs
	rep        *report
	logMu      sync.Mutex

	keys      *runKeys
	keyID     int64
	image     string
	networkID int64
	machines  []*machine // every server created, in order
	cur       *machine   // the server the console runs on
	// host is the console's hostname once it is fixed (restore: the old
	// server's); "" means <cur ip>.sslip.io.
	host string
	// pinIP: connect to this address for the console's names (restore).
	pinIP   string
	certs   *certChecker
	console *console
	owner   owner
	sshAt   time.Time
	bucket  *bucket
	// restoreDNS: what the restoring installer said about DNS.
	restoreDNS []string
	// recoveryKey from Settings › Backups, for the restore (masked).
	recoveryKey string
}

func (r *runner) logf(format string, args ...any) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.log, "%s %s\n", r.now().UTC().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// step runs fn and records its result.
func (r *runner) step(name string, fn func() (string, error)) error {
	res := r.measure(name, time.Time{}, 0, fn)
	r.rep.add(res)
	if res.Status == fail {
		return errors.New(res.Detail)
	}
	return nil
}

// measure runs fn and returns its result. With a budget, a pass that ends
// later than budget after since (or the start) is recorded as slow.
func (r *runner) measure(name string, since time.Time, budget time.Duration, fn func() (string, error)) result {
	r.logf("▸ %s", name)
	start := r.now()
	if since.IsZero() {
		since = start
	}
	detail, err := fn()
	res := result{Name: name, Status: pass, Duration: r.now().Sub(start), Detail: detail}
	switch {
	case err != nil:
		res.Status = fail
		res.Detail = strings.TrimSpace(strings.Join([]string{detail, err.Error()}, " — "))
		res.Detail = strings.TrimPrefix(res.Detail, "— ")
		r.logf("✗ %s: %s", name, res.Detail)
	case budget > 0 && r.now().Sub(since) > budget:
		res.Status = warn
		res.Detail = strings.TrimSpace(detail + fmt.Sprintf(" (over the %s budget)", fmtDuration(budget)))
		r.logf("! %s: %s", name, res.Detail)
	default:
		r.logf("✓ %s %s", name, detail)
	}
	return res
}

// waitFor polls fn until it reports done, fails for good, or timeout passes.
func (r *runner) waitFor(ctx context.Context, timeout time.Duration, fn func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := r.now()
	var last error
	for {
		done, err := fn(ctx)
		if done {
			return err
		}
		// An error caused by the deadline itself says less than the one before.
		if err != nil && (ctx.Err() == nil || last == nil) {
			last = err
		}
		if serr := sleepCtx(ctx, r.cfg.Poll); serr != nil {
			if last != nil {
				return fmt.Errorf("gave up after %s: %w", fmtDuration(r.now().Sub(start)), last)
			}
			return fmt.Errorf("gave up after %s", fmtDuration(r.now().Sub(start)))
		}
	}
}

// run is the whole e2e run. Cleanup always happens, whatever went wrong.
func (r *runner) run(ctx context.Context) {
	r.rep.Started = r.now()
	r.rep.Scenario = r.cfg.scenario()
	defer func() { r.rep.Finished = r.now() }()
	defer r.destroy()

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	if err := r.execute(ctx); err != nil && ctx.Err() != nil {
		r.rep.note("The run stopped early: %v.", ctx.Err())
	}
}

func (r *runner) execute(ctx context.Context) error {
	switch r.cfg.mode() {
	case modeConsole, modeFault:
		return r.executeViaConsole(ctx)
	case modeK3s:
		return r.executeK3s(ctx)
	case modeRestore:
		return r.executeRestore(ctx)
	}
	if err := r.step("Create server", func() (string, error) { return r.createPrimary(ctx) }); err != nil {
		return err
	}
	if err := r.step("SSH", func() (string, error) { return r.connect(ctx, r.cur) }); err != nil {
		return err
	}
	if r.cfg.From != "" {
		if err := r.install(ctx, r.cfg.From, "Install v"+r.cfg.From); err != nil {
			return err
		}
		if err := r.firstSignIn(ctx, r.cfg.From); err != nil {
			return err
		}
		if err := r.step("App before the upgrade", func() (string, error) { return r.seedApp(ctx, 1) }); err != nil {
			return err
		}
		if err := r.install(ctx, r.cfg.Version, "Upgrade to v"+r.cfg.Version); err != nil {
			return err
		}
		if err := r.step("After the upgrade", func() (string, error) { return r.afterUpgrade(ctx) }); err != nil {
			return err
		}
	} else {
		if err := r.install(ctx, r.cfg.Version, "Install v"+r.cfg.Version); err != nil {
			return err
		}
		if err := r.firstSignIn(ctx, r.cfg.Version); err != nil {
			return err
		}
	}
	return r.suite(ctx)
}

// ---- servers ---------------------------------------------------------------------

func (r *runner) labels() map[string]string {
	return map[string]string{labelE2E: "true", labelRun: r.cfg.RunID, labelCreated: strconv.FormatInt(r.now().Unix(), 10)}
}

// ensureKey generates the run's keys and uploads the SSH key, once.
func (r *runner) ensureKey(ctx context.Context) error {
	if r.keyID != 0 {
		return nil
	}
	if r.keys == nil {
		keys, err := newRunKeys(r.cfg.serverName())
		if err != nil {
			return err
		}
		r.keys = keys
	}
	key, err := r.cloud.createSSHKey(ctx, r.cfg.serverName(), r.keys.clientPub, r.labels())
	if err != nil {
		return fmt.Errorf("upload SSH key: %w", err)
	}
	r.keyID = key.ID
	return nil
}

func (r *runner) pickImage(ctx context.Context) (string, error) {
	if r.image != "" {
		return r.image, nil
	}
	for _, name := range r.cfg.Images {
		if _, err := r.cloud.image(ctx, name); err == nil {
			r.image = name
			return name, nil
		} else if !errors.Is(err, errNotFound) {
			return "", fmt.Errorf("look up image %s: %w", name, err)
		}
	}
	return "", fmt.Errorf("none of the images %s exists", strings.Join(r.cfg.Images, ", "))
}

// createPrimary creates the server the console is installed on.
func (r *runner) createPrimary(ctx context.Context) (string, error) {
	m, err := r.createMachine(ctx, r.cfg.serverName(), r.cfg.Locations)
	if m != nil {
		r.cur = m
	}
	return r.rep.Server, err
}

// createMachine creates a server: the first available server type in the
// first location that takes it, attached to the run's network if there is
// one, and waits until it runs. A server that was created is returned (for
// cleanup) also when waiting fails.
func (r *runner) createMachine(ctx context.Context, name string, locations []string) (*machine, error) {
	if err := r.ensureKey(ctx); err != nil {
		return nil, err
	}
	image, err := r.pickImage(ctx)
	if err != nil {
		return nil, err
	}
	var networks []int64
	if r.networkID != 0 {
		networks = []int64{r.networkID}
	}
	var tried []string
	for _, typ := range r.cfg.ServerTypes {
		st, err := r.cloud.serverType(ctx, typ)
		if errors.Is(err, errNotFound) {
			tried = append(tried, typ+": no such server type")
			continue
		} else if err != nil {
			return nil, fmt.Errorf("look up server type %s: %w", typ, err)
		}
		if st.Deprecation != nil && r.now().After(st.Deprecation.UnavailableAfter) {
			tried = append(tried, typ+": deprecated")
			continue
		}
		for _, loc := range locations {
			req := createServerRequest{
				Name: name, ServerType: typ, Location: loc, Image: image,
				SSHKeys: []int64{r.keyID}, Labels: r.labels(), UserData: cloudInit(r.keys), StartAfterCreate: true,
				Networks: networks,
			}
			req.PublicNet.EnableIPv4, req.PublicNet.EnableIPv6 = true, true
			srv, err := r.cloud.createServer(ctx, req)
			var ae *apiError
			if errors.As(err, &ae) && unavailable(ae) {
				tried = append(tried, fmt.Sprintf("%s in %s: %s", typ, loc, ae.Code))
				continue
			} else if err != nil {
				return nil, fmt.Errorf("create server %s (%s in %s): %w", name, typ, loc, err)
			}
			m := &machine{name: name, id: srv.ID, ip: srv.PublicNet.IPv4.IP, location: loc, price: st.hourlyGross(loc), created: r.now()}
			r.machines = append(r.machines, m)
			desc := fmt.Sprintf("%s (%d vCPU, %g GB) in %s, %s, id %d", typ, st.Cores, st.Memory, loc, image, srv.ID)
			if len(tried) > 0 {
				r.rep.note("Server types or locations skipped for %s: %s.", name, strings.Join(tried, "; "))
			}
			if err := r.waitRunning(ctx, m); err != nil {
				r.describe(m, desc)
				return m, err
			}
			r.describe(m, desc)
			return m, nil
		}
	}
	return nil, fmt.Errorf("no server could be created: %s", strings.Join(tried, "; "))
}

// describe adds a server to the report's server line.
func (r *runner) describe(m *machine, desc string) {
	desc += ", " + m.ip
	if r.rep.Server == "" {
		r.rep.Server = desc
		r.rep.PriceHourly = m.price
		return
	}
	r.rep.Server += "; " + m.name + ": " + desc
}

// unavailable: this type or location cannot take the server right now; try
// the next one.
func unavailable(e *apiError) bool {
	switch e.Code {
	case "resource_unavailable", "placement_error", "server_type_unavailable", "unsupported_location_for_server_type", "invalid_input":
		return true
	}
	return e.Status == http.StatusPreconditionFailed
}

func (r *runner) waitRunning(ctx context.Context, m *machine) error {
	return r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		s, err := r.cloud.server(ctx, m.id)
		if err != nil {
			return false, err
		}
		if m.ip == "" {
			m.ip = s.PublicNet.IPv4.IP
		}
		return s.Status == "running" && m.ip != "", nil
	})
}

// connect opens SSH to m and waits for cloud-init.
func (r *runner) connect(ctx context.Context, m *machine) (string, error) {
	sctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	rem, err := waitSSH(sctx, r.dial, net.JoinHostPort(m.ip, "22"), r.keys, r.cfg.Poll)
	if err != nil {
		return "", err
	}
	m.remote = rem
	if m == r.cur {
		r.sshAt = r.now()
	}
	// The installer must not race cloud-init (apt locks, host keys).
	var out bytes.Buffer
	if code, err := rem.run(ctx, "cloud-init status --wait >/dev/null 2>&1; . /etc/os-release && echo \"$PRETTY_NAME\"", &out, &out); err != nil || code != 0 {
		return "", fmt.Errorf("cloud-init: exit %d %v: %s", code, err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// deleteMachine deletes a server and waits until Hetzner confirms.
func (r *runner) deleteMachine(ctx context.Context, m *machine) error {
	if m.remote != nil {
		_ = m.remote.close()
		m.remote = nil
	}
	if err := r.cloud.deleteServer(ctx, m.id); err != nil {
		return fmt.Errorf("server %d NOT deleted: %w", m.id, err)
	}
	err := r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		_, err := r.cloud.server(ctx, m.id)
		if errors.Is(err, errNotFound) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		return fmt.Errorf("server %d deletion requested but not confirmed: %w", m.id, err)
	}
	m.deleted = r.now()
	return nil
}

// destroy deletes the servers, the network, the SSH key and the bucket
// prefix. It runs on its own context: the run's may be cancelled or timed
// out by now.
func (r *runner) destroy() {
	for _, m := range r.machines {
		if m.remote != nil {
			_ = m.remote.close()
			m.remote = nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	start := r.now()
	ok := true
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, m := range r.machines {
		if !m.deleted.IsZero() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.deleteMachine(ctx, m)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				ok = false
				r.rep.Cleanup = append(r.rep.Cleanup, err.Error())
				return
			}
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("server %d deleted", m.id))
		}()
	}
	wg.Wait()
	if r.networkID != 0 {
		// Hetzner detaches deleted servers in the background.
		err := r.waitFor(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
			err := r.cloud.deleteNetwork(ctx, r.networkID)
			var ae *apiError
			if errors.As(err, &ae) && (ae.Status == http.StatusConflict || ae.Status == http.StatusLocked) {
				return false, err
			}
			return true, err
		})
		if err != nil {
			ok = false
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("network %d NOT deleted: %v", r.networkID, err))
		} else {
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("network %d deleted", r.networkID))
		}
	}
	if r.keyID != 0 {
		if err := r.cloud.deleteSSHKey(ctx, r.keyID); err != nil {
			ok = false
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("SSH key %d NOT deleted: %v", r.keyID, err))
		} else {
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("SSH key %d deleted", r.keyID))
		}
	}
	if r.bucket != nil {
		prefix := r.cfg.RunID + "/"
		n, err := r.bucket.deletePrefix(ctx, prefix)
		if err != nil {
			ok = false
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("bucket %s, prefix %s NOT emptied (%d object(s) deleted): %v", r.cfg.S3.Bucket, prefix, n, err))
		} else {
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("bucket %s, prefix %s: %d object(s) deleted", r.cfg.S3.Bucket, prefix, n))
		}
	}
	// Anything else with this run's label: a create whose answer was lost.
	if res, err := sweep(ctx, r.cloud, r.now(), 0, r.cfg.RunID, false); err != nil {
		ok = false
		r.rep.Cleanup = append(r.rep.Cleanup, "looking for other resources of this run: "+err.Error())
	} else {
		r.rep.Cleanup = append(r.rep.Cleanup, res.Deleted...)
	}
	if len(r.rep.Cleanup) == 0 {
		r.rep.Cleanup = append(r.rep.Cleanup, "nothing was created")
	}
	r.rep.CleanupOK = ok
	for _, m := range r.machines {
		end := m.deleted
		if end.IsZero() {
			end = r.now()
		}
		r.rep.Billing = append(r.rep.Billing, billed{Price: m.price, Start: m.created, End: end})
	}
	r.logf("cleanup (%s): %s", fmtDuration(r.now().Sub(start)), strings.Join(r.rep.Cleanup, "; "))
}

// ---- installer ---------------------------------------------------------------

// domain is the console's hostname.
func (r *runner) domain() string {
	if r.host != "" {
		return r.host
	}
	return r.cur.ip + ".sslip.io"
}

// sh runs a short command on the console's server and returns its
// combined output.
func (r *runner) sh(ctx context.Context, cmd string) (string, int, error) {
	return r.shOn(ctx, r.cur, cmd)
}

func (r *runner) shOn(ctx context.Context, m *machine, cmd string) (string, int, error) {
	var out bytes.Buffer
	code, err := m.remote.run(ctx, cmd, &out, &out)
	return strings.TrimSpace(out.String()), code, err
}

// installOpts changes how the installer runs.
type installOpts struct {
	on      *machine // nil: the console's server
	env     []string // NAME=value for the installer's environment
	join    []string // join mode: these arguments instead of --domain, --version, --acme-server
	config  string   // --config FILE
	restore string   // --restore B (no --domain: it comes from the backup)
}

func (r *runner) install(ctx context.Context, version, name string) error {
	return r.installWith(ctx, version, name, installOpts{})
}

func (r *runner) installWith(ctx context.Context, version, name string, o installOpts) error {
	return r.step(name, func() (string, error) { return r.runInstaller(ctx, version, o) })
}

func installerPath(version string) string { return "/root/kwerft-install-" + version + ".sh" }

// downloadInstaller puts the published installer of version on m.
func (r *runner) downloadInstaller(ctx context.Context, m *machine, version string) error {
	url := r.cfg.installerURL(version)
	if out, code, err := r.shOn(ctx, m, fmt.Sprintf("curl -fsSL --retry 5 --retry-all-errors -o %s %s", shellQuote(installerPath(version)), shellQuote(url))); err != nil || code != 0 {
		return fmt.Errorf("download %s: exit %d %v %s", url, code, err, out)
	}
	return nil
}

func (r *runner) runInstaller(ctx context.Context, version string, o installOpts) (string, error) {
	m := cmpOr2(o.on, r.cur)
	script := installerPath(version)
	if err := r.downloadInstaller(ctx, m, version); err != nil {
		return "", err
	}
	staging := false
	var args []string
	if o.join != nil {
		args = append(append(args, o.join...), "--yes")
	} else {
		_, code, err := r.shOn(ctx, m, "grep -q -e '--acme-server' "+shellQuote(script))
		if err != nil {
			return "", err
		}
		staging = code == 0
		if o.restore == "" {
			args = append(args, "--domain", r.domain())
		}
		args = append(args, "--version", version, "--yes")
		if o.config != "" {
			args = append(args, "--config", o.config)
		}
		if o.restore != "" {
			args = append(args, "--restore", o.restore)
		}
		if staging {
			args = append(args, "--acme-server", "staging")
		}
	}
	cmd := ""
	for _, kv := range o.env {
		k, v, _ := strings.Cut(kv, "=")
		cmd += k + "=" + shellQuote(v) + " "
	}
	cmd += "bash " + shellQuote(script)
	for _, a := range args {
		cmd += " " + shellQuote(a)
	}

	tail := newTail(60)
	w := io.MultiWriter(prefixWriter(r.log, "  │ "), tail)
	ictx, cancel := context.WithTimeout(ctx, r.cfg.InstallTimeout)
	defer cancel()
	start := r.now()
	code, err := m.remote.run(ictx, cmd, w, w)
	took := r.now().Sub(start)
	switch {
	case err != nil:
		r.rep.Log = tail.String() + r.diagnostics(ctx, m)
		return "", fmt.Errorf("installer did not finish: %w", err)
	case code != 0:
		r.rep.Log = tail.String() + r.diagnostics(ctx, m)
		return "", fmt.Errorf("installer exited %d (%s)", code, installerExit[code])
	}
	detail := "exit 0 in " + fmtDuration(took)
	if o.restore != "" {
		// What the installer says about DNS, for the restore run's report.
		for _, l := range strings.Split(tail.String(), "\n") {
			if strings.Contains(l, "DNS") {
				r.restoreDNS = append(r.restoreDNS, strings.TrimSpace(stripANSI(l)))
			}
		}
	}
	if !staging && o.join == nil {
		if out, code, err := r.shOn(ctx, m, stagingFallback); err != nil || code != 0 {
			return detail, fmt.Errorf("switch the ClusterIssuer to Let's Encrypt staging: exit %d %v %s", code, err, out)
		}
		detail += "; this installer has no --acme-server, so the ClusterIssuer was switched to staging afterwards"
	}
	return detail, nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func cmpOr2[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// stagingFallback points an installer's ClusterIssuer at Let's Encrypt
// staging when the installer (an older release) cannot, and has the
// Gateway's certificates requested again from there.
var stagingFallback = `set -e
kc() { k3s kubectl "$@"; }
kc patch clusterissuer letsencrypt --type merge -p '{"spec":{"acme":{"server":"` + acmeStaging + `"}}}'
kc -n kwerft-system delete certificates.cert-manager.io --all --ignore-not-found`

// diagnostics collects what helps to understand a failed install.
func (r *runner) diagnostics(ctx context.Context, m *machine) string {
	if m == nil || m.remote == nil {
		return ""
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 1*time.Minute)
	defer cancel()
	out, _, _ := r.shOn(dctx, m, "echo '--- install.log'; tail -n 40 /var/log/kwerft/install.log 2>&1; echo '--- pods'; k3s kubectl get pods -A 2>&1 | grep -v -E 'Running|Completed' | head -n 40")
	r.logf("diagnostics:\n%s", out)
	return "\n" + out
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ---- console -------------------------------------------------------------------

func (r *runner) openConsole() {
	if r.console != nil {
		return
	}
	r.certs = &certChecker{}
	hc := r.transport(r.certs)
	if r.pinIP != "" {
		hc = pinHost(hc, r.domain(), r.pinIP)
	}
	r.console = &console{base: "https://" + r.domain(), http: hc}
}

// pinHost sends connections for domain and its subdomains to ip, as DNS
// would once the records moved: after a restore the console keeps the old
// server's names. TLS still checks the certificate for the name.
func pinHost(c *http.Client, domain, ip string) *http.Client {
	tr := c.Transport.(*http.Transport).Clone()
	next := tr.DialContext
	if next == nil {
		next = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil && (host == domain || strings.HasSuffix(host, "."+domain)) {
			addr = net.JoinHostPort(ip, port)
		}
		return next(ctx, network, addr)
	}
	out := *c
	out.Transport = tr
	return &out
}

// waitHTTPS waits for url to answer 200 with a body containing want.
func (r *runner) waitHTTPS(ctx context.Context, url, want string, timeout time.Duration) error {
	return r.waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		code, body, err := r.console.get(ctx, url)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK || !strings.Contains(body, want) {
			return false, fmt.Errorf("HTTP %d: %s", code, truncate(strings.TrimSpace(body), 120))
		}
		return true, nil
	})
}

// consoleUp waits for /healthz and checks the version.
func (r *runner) consoleUp(ctx context.Context, version string) (string, error) {
	err := r.waitFor(ctx, 10*time.Minute, func(ctx context.Context) (bool, error) {
		err := r.console.healthz(ctx)
		return err == nil, err
	})
	if err != nil {
		return "", err
	}
	v, err := r.console.version(ctx)
	if err != nil {
		return "", err
	}
	detail := fmt.Sprintf("https://%s, certificate from %s, version %s", r.domain(), r.certs.issuerOf(r.domain()), v)
	if strings.TrimPrefix(v, "v") != version {
		return detail, fmt.Errorf("the console reports version %q, want %s", v, version)
	}
	return detail, nil
}

func (r *runner) firstSignIn(ctx context.Context, version string) error {
	r.openConsole()
	if err := r.step("Console on HTTPS", func() (string, error) { return r.consoleUp(ctx, version) }); err != nil {
		return err
	}
	return r.step("Owner from the setup token, sign in", func() (string, error) {
		token, code, err := r.sh(ctx, "cat /etc/kwerft/setup-token")
		if err != nil || code != 0 || token == "" {
			return "", fmt.Errorf("read the setup token: exit %d %v", code, err)
		}
		r.mask(token)
		r.owner = owner{Name: "e2e owner", Email: "owner@e2e.kwerft.dev", Password: randomString(24)}
		r.mask(r.owner.Password)
		if err := r.console.createOwner(ctx, token, r.owner); err != nil {
			return "", err
		}
		if done, err := r.console.setupComplete(ctx); err != nil || !done {
			return "", fmt.Errorf("setup not complete after creating the owner (%v)", err)
		}
		if err := r.console.signIn(ctx, r.owner); err != nil {
			return "", fmt.Errorf("sign in: %w", err)
		}
		return r.owner.Email, nil
	})
}

func (r *runner) createProject(ctx context.Context, name string) error {
	return r.waitFor(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		err := r.console.createProject(ctx, name)
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusConflict {
			return true, nil // created by an earlier attempt whose answer got lost
		}
		return err == nil, err
	})
}

// createApp retries while the project's namespace is being set up.
func (r *runner) createApp(ctx context.Context, project, name string, spec kwerftv1.AppSpec) error {
	return r.waitFor(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		err := r.console.createApp(ctx, project, name, spec)
		var he *httpError
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &he) && he.Status == http.StatusConflict:
			return true, nil
		case errors.As(err, &he) && he.Status == http.StatusBadRequest:
			return true, err
		}
		return false, err
	})
}

func whoamiSpec(host string) kwerftv1.AppSpec {
	return kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: whoamiImage}},
		Ports:  []kwerftv1.AppPort{{Container: 80, Public: host}},
	}
}

// helloURL is the App that must keep serving through an upgrade.
func (r *runner) helloURL() string { return "https://hello." + r.domain() + "/" }

func (r *runner) seedApp(ctx context.Context, replicas int32) (string, error) {
	host := "hello." + r.domain()
	if err := r.createProject(ctx, "e2e-before"); err != nil {
		return "", fmt.Errorf("create project: %w", err)
	}
	spec := whoamiSpec(host)
	if replicas > 1 {
		spec.Replicas = &replicas
	}
	if err := r.createApp(ctx, "e2e-before", "hello", spec); err != nil {
		return "", fmt.Errorf("create app: %w", err)
	}
	if err := r.waitHTTPS(ctx, r.helloURL(), "Hostname", 10*time.Minute); err != nil {
		return "", err
	}
	if replicas > 1 {
		return fmt.Sprintf("https://%s answers (%d replicas)", host, replicas), nil
	}
	return "https://" + host + " answers", nil
}

// waitVersion waits for the console to report version.
func (r *runner) waitVersion(ctx context.Context, version string) error {
	return r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		v, err := r.console.version(ctx)
		if err != nil {
			return false, err
		}
		if strings.TrimPrefix(v, "v") != version {
			return false, fmt.Errorf("the console reports version %q, want %s", v, version)
		}
		return true, nil
	})
}

// helloRuns: the App from before still answers and the console says it runs.
func (r *runner) helloRuns(ctx context.Context) error {
	if err := r.waitHTTPS(ctx, r.helloURL(), "Hostname", 5*time.Minute); err != nil {
		return fmt.Errorf("the app from before the upgrade: %w", err)
	}
	return r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		a, err := r.console.app(ctx, "e2e-before", "hello")
		if err != nil {
			return false, err
		}
		if a.Phase != "running" {
			return false, fmt.Errorf("app hello is %s: %s %s", a.Phase, a.Reason, a.Message)
		}
		return true, nil
	})
}

func (r *runner) afterUpgrade(ctx context.Context) (string, error) {
	if err := r.waitVersion(ctx, r.cfg.Version); err != nil {
		return "", err
	}
	if err := r.console.signIn(ctx, r.owner); err != nil {
		return "", fmt.Errorf("the owner cannot sign in: %w", err)
	}
	if err := r.helloRuns(ctx); err != nil {
		return "", err
	}
	return "version " + r.cfg.Version + ", owner signs in, hello." + r.domain() + " still answers and runs", nil
}

// prefer puts first at the front of list.
func prefer(first string, list []string) []string {
	out := []string{first}
	for _, l := range list {
		if l != first {
			out = append(out, l)
		}
	}
	return out
}

// sameZone keeps the locations in the network zone of the first.
func sameZone(locations []string) []string {
	if len(locations) == 0 {
		return nil
	}
	zone := networkZone(locations[0])
	return slices.DeleteFunc(slices.Clone(locations), func(l string) bool { return networkZone(l) != zone })
}
