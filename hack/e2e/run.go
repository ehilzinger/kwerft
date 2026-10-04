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
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	labelE2E     = "kwerft-e2e"
	labelRun     = "run"
	labelCreated = "created"

	acmeStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

	whoamiImage  = "docker.io/traefik/whoami:v1.11.0"
	busyboxImage = "docker.io/library/busybox:1.37"
)

// Exit codes of install.sh (its public contract) by name, for messages.
var installerExit = map[int]string{2: "usage", 10: "preflight", 20: "network/DNS", 30: "Kubernetes", 40: "platform", 50: "Kwerft"}

type config struct {
	Token   string
	APIBase string

	Version string // under test, without "v"
	From    string // previous release for the upgrade path; "" = fresh install
	RunID   string

	ServerTypes []string
	Locations   []string
	Images      []string

	// InstallerURL has {version} replaced by the version to install.
	InstallerURL string
	GitRepo      string
	GitBranch    string

	Timeout        time.Duration // the whole run, cleanup excluded
	InstallTimeout time.Duration // one installer run
	Poll           time.Duration // between checks while waiting
}

func (c config) installerURL(version string) string {
	return strings.ReplaceAll(c.InstallerURL, "{version}", version)
}

func (c config) scenario() string {
	if c.From != "" {
		return fmt.Sprintf("Upgrade from v%s to v%s", c.From, c.Version)
	}
	return fmt.Sprintf("Fresh install of v%s", c.Version)
}

func (c config) serverName() string { return "kwerft-e2e-" + c.RunID }

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
	steps := []string{
		fmt.Sprintf("Generate an ed25519 SSH key and host key for this run; upload the public key as %q with labels %s=true, %s=%s, %s=<unix time>.", c.serverName(), labelE2E, labelRun, c.RunID, labelCreated),
		fmt.Sprintf("Create server %q: the first available of %s in %s, image %s (first that exists), same labels; cloud-init only installs the pinned host key.", c.serverName(), strings.Join(c.ServerTypes, ", "), strings.Join(c.Locations, ", "), strings.Join(c.Images, " or ")),
		"Wait for SSH (root, pinned host key) and for cloud-init to finish.",
	}
	install := func(v, what string) string {
		return fmt.Sprintf("%s: download %s on the server and run it with --domain <ip>.sslip.io --acme-server staging --version %s --yes (an installer without --acme-server gets its ClusterIssuer switched to staging right after); expect exit 0.", what, c.installerURL(v), v)
	}
	if c.From != "" {
		steps = append(steps,
			install(c.From, "Install v"+c.From),
			"Console on HTTPS (Let's Encrypt staging certificate), setup token → owner, sign in; project e2e-before with an image App on hello.<ip>.sslip.io answering over HTTPS.",
			install(c.Version, "Upgrade to v"+c.Version),
			"The console reports v"+c.Version+", the owner still signs in, hello.<ip>.sslip.io still answers.",
		)
	} else {
		steps = append(steps,
			install(c.Version, "Install v"+c.Version),
			"Console on HTTPS (Let's Encrypt staging certificate), /healthz, version v"+c.Version+", setup token → owner, sign in.",
		)
	}
	steps = append(steps,
		"Project e2e: image App web ("+whoamiImage+") on web.<ip>.sslip.io answers over HTTPS (budget: 10 min from SSH to here on a fresh install).",
		"A Task prints a marker and restarts web on success; it succeeds and web rolls over.",
		fmt.Sprintf("Git App git builds %s (%s, Dockerfile) with Build now; the build succeeds and git.<ip>.sslip.io answers (budget 3 min).", c.GitRepo, c.GitBranch),
		"Log search finds the Task's marker; the metrics explorer returns memory series for the project.",
		"App crash ("+busyboxImage+", exits 1) raises a crash-looping alert in the console's alerts API (budget 2 min).",
		"Always, also on failure, timeout or cancel: delete the server and the SSH key, and wait until the server is gone.",
	)
	return steps
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

// runner carries out one e2e run.
type runner struct {
	cfg   config
	cloud *hcloud
	dial  dialer
	// transport dials consoles and apps; tests point it at a fake.
	transport func(*certChecker) *http.Client
	log       io.Writer
	now       func() time.Time
	mask      func(string) // hides a secret in CI logs
	rep       *report
	logMu     sync.Mutex

	keys     *runKeys
	keyID    int64
	serverID int64
	ip       string
	remote   remote
	certs    *certChecker
	console  *console
	owner    owner
	sshAt    time.Time
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
	if err := r.step("Create server", func() (string, error) { return r.createServer(ctx) }); err != nil {
		return err
	}
	if err := r.step("SSH", func() (string, error) { return r.connect(ctx) }); err != nil {
		return err
	}
	if r.cfg.From != "" {
		if err := r.install(ctx, r.cfg.From, "Install v"+r.cfg.From); err != nil {
			return err
		}
		if err := r.firstSignIn(ctx, r.cfg.From); err != nil {
			return err
		}
		if err := r.step("App before the upgrade", func() (string, error) { return r.seedApp(ctx) }); err != nil {
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

// ---- server ----------------------------------------------------------------

func (r *runner) labels() map[string]string {
	return map[string]string{labelE2E: "true", labelRun: r.cfg.RunID, labelCreated: strconv.FormatInt(r.now().Unix(), 10)}
}

func (r *runner) createServer(ctx context.Context) (string, error) {
	keys, err := newRunKeys(r.cfg.serverName())
	if err != nil {
		return "", err
	}
	r.keys = keys
	key, err := r.cloud.createSSHKey(ctx, r.cfg.serverName(), keys.clientPub, r.labels())
	if err != nil {
		return "", fmt.Errorf("upload SSH key: %w", err)
	}
	r.keyID = key.ID

	image := ""
	for _, name := range r.cfg.Images {
		if _, err := r.cloud.image(ctx, name); err == nil {
			image = name
			break
		} else if !errors.Is(err, errNotFound) {
			return "", fmt.Errorf("look up image %s: %w", name, err)
		}
	}
	if image == "" {
		return "", fmt.Errorf("none of the images %s exists", strings.Join(r.cfg.Images, ", "))
	}

	var tried []string
	for _, typ := range r.cfg.ServerTypes {
		st, err := r.cloud.serverType(ctx, typ)
		if errors.Is(err, errNotFound) {
			tried = append(tried, typ+": no such server type")
			continue
		} else if err != nil {
			return "", fmt.Errorf("look up server type %s: %w", typ, err)
		}
		if st.Deprecation != nil && r.now().After(st.Deprecation.UnavailableAfter) {
			tried = append(tried, typ+": deprecated")
			continue
		}
		for _, loc := range r.cfg.Locations {
			req := createServerRequest{
				Name: r.cfg.serverName(), ServerType: typ, Location: loc, Image: image,
				SSHKeys: []int64{key.ID}, Labels: r.labels(), UserData: cloudInit(keys), StartAfterCreate: true,
			}
			req.PublicNet.EnableIPv4, req.PublicNet.EnableIPv6 = true, true
			srv, err := r.cloud.createServer(ctx, req)
			var ae *apiError
			if errors.As(err, &ae) && unavailable(ae) {
				tried = append(tried, fmt.Sprintf("%s in %s: %s", typ, loc, ae.Code))
				continue
			} else if err != nil {
				return "", fmt.Errorf("create server (%s in %s): %w", typ, loc, err)
			}
			r.serverID, r.ip = srv.ID, srv.PublicNet.IPv4.IP
			r.rep.PriceHourly = st.hourlyGross(loc)
			r.rep.Server = fmt.Sprintf("%s (%d vCPU, %g GB) in %s, %s, id %d, %s", typ, st.Cores, st.Memory, loc, image, srv.ID, r.ip)
			if len(tried) > 0 {
				r.rep.note("Server types or locations skipped: %s.", strings.Join(tried, "; "))
			}
			if err := r.waitRunning(ctx); err != nil {
				return r.rep.Server, err
			}
			return r.rep.Server, nil
		}
	}
	return "", fmt.Errorf("no server could be created: %s", strings.Join(tried, "; "))
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

func (r *runner) waitRunning(ctx context.Context) error {
	return r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		s, err := r.cloud.server(ctx, r.serverID)
		if err != nil {
			return false, err
		}
		if r.ip == "" {
			r.ip = s.PublicNet.IPv4.IP
		}
		return s.Status == "running" && r.ip != "", nil
	})
}

func (r *runner) connect(ctx context.Context) (string, error) {
	sctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	rem, err := waitSSH(sctx, r.dial, net.JoinHostPort(r.ip, "22"), r.keys, r.cfg.Poll)
	if err != nil {
		return "", err
	}
	r.remote = rem
	r.sshAt = r.now()
	// The installer must not race cloud-init (apt locks, host keys).
	var out bytes.Buffer
	if code, err := rem.run(ctx, "cloud-init status --wait >/dev/null 2>&1; . /etc/os-release && echo \"$PRETTY_NAME\"", &out, &out); err != nil || code != 0 {
		return "", fmt.Errorf("cloud-init: exit %d %v: %s", code, err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// destroy deletes the server and the SSH key. It runs on its own context:
// the run's may be cancelled or timed out by now.
func (r *runner) destroy() {
	if r.remote != nil {
		_ = r.remote.close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	start := r.now()
	ok := true
	if r.serverID != 0 {
		if err := r.cloud.deleteServer(ctx, r.serverID); err != nil {
			ok = false
			r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("server %d NOT deleted: %v", r.serverID, err))
		} else {
			err := r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
				_, err := r.cloud.server(ctx, r.serverID)
				if errors.Is(err, errNotFound) {
					return true, nil
				}
				return false, err
			})
			if err != nil {
				ok = false
				r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("server %d deletion requested but not confirmed: %v", r.serverID, err))
			} else {
				r.rep.Cleanup = append(r.rep.Cleanup, fmt.Sprintf("server %d deleted", r.serverID))
			}
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
	r.logf("cleanup (%s): %s", fmtDuration(r.now().Sub(start)), strings.Join(r.rep.Cleanup, "; "))
}

// ---- installer ---------------------------------------------------------------

func (r *runner) domain() string { return r.ip + ".sslip.io" }

// sh runs a short command and returns its combined output.
func (r *runner) sh(ctx context.Context, cmd string) (string, int, error) {
	var out bytes.Buffer
	code, err := r.remote.run(ctx, cmd, &out, &out)
	return strings.TrimSpace(out.String()), code, err
}

func (r *runner) install(ctx context.Context, version, name string) error {
	return r.step(name, func() (string, error) {
		script := "/root/kwerft-install-" + version + ".sh"
		url := r.cfg.installerURL(version)
		if out, code, err := r.sh(ctx, fmt.Sprintf("curl -fsSL --retry 5 --retry-all-errors -o %s %s", shellQuote(script), shellQuote(url))); err != nil || code != 0 {
			return "", fmt.Errorf("download %s: exit %d %v %s", url, code, err, out)
		}
		_, code, err := r.sh(ctx, "grep -q -e '--acme-server' "+shellQuote(script))
		if err != nil {
			return "", err
		}
		staging := code == 0
		args := []string{"--domain", r.domain(), "--version", version, "--yes"}
		if staging {
			args = append(args, "--acme-server", "staging")
		}
		cmd := "bash " + shellQuote(script)
		for _, a := range args {
			cmd += " " + shellQuote(a)
		}

		tail := newTail(60)
		w := io.MultiWriter(prefixWriter(r.log, "  │ "), tail)
		ictx, cancel := context.WithTimeout(ctx, r.cfg.InstallTimeout)
		defer cancel()
		start := r.now()
		code, err = r.remote.run(ictx, cmd, w, w)
		took := r.now().Sub(start)
		switch {
		case err != nil:
			r.rep.Log = tail.String() + r.diagnostics(ctx)
			return "", fmt.Errorf("installer did not finish: %w", err)
		case code != 0:
			r.rep.Log = tail.String() + r.diagnostics(ctx)
			return "", fmt.Errorf("installer exited %d (%s)", code, installerExit[code])
		}
		detail := "exit 0 in " + fmtDuration(took)
		if !staging {
			if out, code, err := r.sh(ctx, stagingFallback); err != nil || code != 0 {
				return detail, fmt.Errorf("switch the ClusterIssuer to Let's Encrypt staging: exit %d %v %s", code, err, out)
			}
			detail += "; this installer has no --acme-server, so the ClusterIssuer was switched to staging afterwards"
		}
		return detail, nil
	})
}

// stagingFallback points an installer's ClusterIssuer at Let's Encrypt
// staging when the installer (an older release) cannot, and has the
// Gateway's certificates requested again from there.
var stagingFallback = `set -e
kc() { k3s kubectl "$@"; }
kc patch clusterissuer letsencrypt --type merge -p '{"spec":{"acme":{"server":"` + acmeStaging + `"}}}'
kc -n kwerft-system delete certificates.cert-manager.io --all --ignore-not-found`

// diagnostics collects what helps to understand a failed install.
func (r *runner) diagnostics(ctx context.Context) string {
	if r.remote == nil {
		return ""
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 1*time.Minute)
	defer cancel()
	out, _, _ := r.sh(dctx, "echo '--- install.log'; tail -n 40 /var/log/kwerft/install.log 2>&1; echo '--- pods'; k3s kubectl get pods -A 2>&1 | grep -v -E 'Running|Completed' | head -n 40")
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
	r.console = &console{base: "https://" + r.domain(), http: r.transport(r.certs)}
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

func (r *runner) firstSignIn(ctx context.Context, version string) error {
	r.openConsole()
	if err := r.step("Console on HTTPS", func() (string, error) {
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
	}); err != nil {
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

func (r *runner) seedApp(ctx context.Context) (string, error) {
	host := "hello." + r.domain()
	if err := r.createProject(ctx, "e2e-before"); err != nil {
		return "", fmt.Errorf("create project: %w", err)
	}
	if err := r.createApp(ctx, "e2e-before", "hello", whoamiSpec(host)); err != nil {
		return "", fmt.Errorf("create app: %w", err)
	}
	if err := r.waitHTTPS(ctx, "https://"+host+"/", "Hostname", 10*time.Minute); err != nil {
		return "", err
	}
	return "https://" + host + " answers", nil
}

func (r *runner) afterUpgrade(ctx context.Context) (string, error) {
	err := r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		v, err := r.console.version(ctx)
		if err != nil {
			return false, err
		}
		if strings.TrimPrefix(v, "v") != r.cfg.Version {
			return false, fmt.Errorf("the console reports version %q, want %s", v, r.cfg.Version)
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	if err := r.console.signIn(ctx, r.owner); err != nil {
		return "", fmt.Errorf("the owner cannot sign in: %w", err)
	}
	host := "hello." + r.domain()
	if err := r.waitHTTPS(ctx, "https://"+host+"/", "Hostname", 5*time.Minute); err != nil {
		return "", fmt.Errorf("the app from before the upgrade: %w", err)
	}
	err = r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		a, err := r.console.app(ctx, "e2e-before", "hello")
		if err != nil {
			return false, err
		}
		if a.Phase != "running" {
			return false, fmt.Errorf("app hello is %s: %s %s", a.Phase, a.Reason, a.Message)
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return "version " + r.cfg.Version + ", owner signs in, " + host + " still answers and runs", nil
}

// ---- the suite -------------------------------------------------------------------

const project = "e2e"

// suite creates what it checks up front, then waits for the checks side by
// side, so each one's time is measured from what it waits for: an App on
// HTTPS from its creation, a build from "Build now", an alert from the
// crashing App's creation.
func (r *runner) suite(ctx context.Context) error {
	web, gitHost := "web."+r.domain(), "git."+r.domain()
	marker := "kwerft-e2e-marker-" + r.cfg.RunID

	var createdAt, crashAt, buildAt time.Time
	var buildName string
	if err := r.step("Project and apps created", func() (string, error) {
		if err := r.createProject(ctx, project); err != nil {
			return "", fmt.Errorf("project: %w", err)
		}
		if err := r.createApp(ctx, project, "web", whoamiSpec(web)); err != nil {
			return "", fmt.Errorf("app web: %w", err)
		}
		createdAt = r.now()
		crash := kwerftv1.AppSpec{
			Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: busyboxImage}},
			Command: []string{"sh", "-c", "echo crashing on purpose; exit 1"},
		}
		if err := r.createApp(ctx, project, "crash", crash); err != nil {
			return "", fmt.Errorf("app crash: %w", err)
		}
		crashAt = r.now()
		gitSpec := kwerftv1.AppSpec{
			Source: kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: r.cfg.GitRepo, Branch: r.cfg.GitBranch, Builder: "dockerfile"}},
			Ports:  []kwerftv1.AppPort{{Container: 80, Public: gitHost}},
		}
		if err := r.createApp(ctx, project, "git", gitSpec); err != nil {
			return "", fmt.Errorf("app git: %w", err)
		}
		b, err := r.console.buildNow(ctx, project, "git")
		if err != nil {
			return "", fmt.Errorf("build now: %w", err)
		}
		buildAt, buildName = r.now(), b.Name
		return "web, crash, git (build " + b.Name + ")", nil
	}); err != nil {
		return err
	}

	// Each lane runs its checks in order; lanes run side by side. Results are
	// reported in this order whatever finishes first.
	lanes := [][]check{
		{
			{"App on HTTPS", createdAt, 0, func() (string, error) { return r.checkWeb(ctx, web) }},
			{"Task runs and restarts web", time.Time{}, 0, func() (string, error) { return r.checkTask(ctx, web, marker) }},
			{"Log search", time.Time{}, 0, func() (string, error) { return r.checkLogs(ctx, marker) }},
		},
		{{"Git build deploys", buildAt, 3 * time.Minute, func() (string, error) { return r.checkBuild(ctx, buildName, buildAt, gitHost) }}},
		{{"Metrics", time.Time{}, 0, func() (string, error) { return r.checkMetrics(ctx) }}},
		{{"Crash loop alert", crashAt, 2 * time.Minute, func() (string, error) { return r.checkAlert(ctx, crashAt) }}},
	}
	results := make([][]result, len(lanes))
	var wg sync.WaitGroup
	for i, lane := range lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, c := range lane {
				if ctx.Err() != nil {
					results[i] = append(results[i], result{Name: c.name, Status: skip, Detail: "the run timed out or was cancelled"})
					continue
				}
				results[i] = append(results[i], r.measure(c.name, c.since, c.budget, c.fn))
			}
		}()
	}
	wg.Wait()
	failed := false
	for _, lane := range results {
		for _, res := range lane {
			r.rep.add(res)
			failed = failed || res.Status == fail || res.Status == skip
		}
	}
	if failed {
		return errors.New("checks failed")
	}
	return nil
}

type check struct {
	name   string
	since  time.Time
	budget time.Duration
	fn     func() (string, error)
}

func (r *runner) checkWeb(ctx context.Context, web string) (string, error) {
	if err := r.waitHTTPS(ctx, "https://"+web+"/", "Hostname", 10*time.Minute); err != nil {
		return "", err
	}
	detail := fmt.Sprintf("https://%s, certificate from %s", web, r.certs.issuerOf(web))
	if r.cfg.From == "" {
		took := r.now().Sub(r.sshAt)
		detail += fmt.Sprintf("; fresh server to app on HTTPS in %s", fmtDuration(took))
		if took > 10*time.Minute {
			r.rep.note("Fresh server to app on HTTPS took %s, over the 10 min of the Phase 1 exit criterion.", fmtDuration(took))
		}
	}
	return detail, nil
}

func (r *runner) checkTask(ctx context.Context, web, marker string) (string, error) {
	before, err := r.console.appPods(ctx, project, "web")
	if err != nil {
		return "", err
	}
	name, err := r.console.createTask(ctx, project, kwerftv1.TaskSpec{
		Source:    &kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: busyboxImage}},
		Command:   []string{"sh", "-c", "echo " + marker},
		Env:       []corev1.EnvVar{{Name: "E2E_RUN", Value: r.cfg.RunID}},
		Timeout:   &metav1.Duration{Duration: 5 * time.Minute},
		OnSuccess: &kwerftv1.TaskOnSuccess{Restart: []string{"web"}},
	})
	if err != nil {
		return "", err
	}
	err = r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		t, err := r.console.task(ctx, project, name)
		if err != nil {
			return false, err
		}
		switch t.Status.Phase {
		case kwerftv1.TaskSucceeded:
			return true, nil
		case kwerftv1.TaskFailed:
			code := "unknown"
			if t.Status.ExitCode != nil {
				code = strconv.Itoa(int(*t.Status.ExitCode))
			}
			return true, fmt.Errorf("task %s failed (exit %s)", name, code)
		}
		return false, fmt.Errorf("task %s is %s", name, cmpOr(string(t.Status.Phase), "pending"))
	})
	if err != nil {
		return "", err
	}
	old := map[string]bool{}
	for _, p := range before {
		old[p.Name] = true
	}
	err = r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		pods, err := r.console.appPods(ctx, project, "web")
		if err != nil {
			return false, err
		}
		for _, p := range pods {
			if !old[p.Name] && p.Ready {
				return true, nil
			}
		}
		return false, errors.New("web has not been restarted")
	})
	if err != nil {
		return "task " + name + " succeeded", err
	}
	if err := r.waitHTTPS(ctx, "https://"+web+"/", "Hostname", 3*time.Minute); err != nil {
		return "task " + name + " succeeded; web restarted", err
	}
	return "task " + name + " succeeded; web restarted and answers", nil
}

func (r *runner) checkBuild(ctx context.Context, name string, since time.Time, host string) (string, error) {
	err := r.waitFor(ctx, 15*time.Minute, func(ctx context.Context) (bool, error) {
		b, err := r.console.build(ctx, project, name)
		if err != nil {
			return false, err
		}
		switch b.Phase {
		case "succeeded":
			return true, nil
		case "failed", "cancelled":
			return true, fmt.Errorf("build %s %s: %s", name, b.Phase, b.StatusMessage)
		}
		return false, fmt.Errorf("build %s is %s", name, b.Phase)
	})
	if err != nil {
		return "", err
	}
	built := r.now().Sub(since)
	if err := r.waitHTTPS(ctx, "https://"+host+"/", "Hostname", 10*time.Minute); err != nil {
		return "build succeeded in " + fmtDuration(built), err
	}
	return fmt.Sprintf("%s built in %s; https://%s answered %s after Build now", r.cfg.GitRepo, fmtDuration(built), host, fmtDuration(r.now().Sub(since))), nil
}

func (r *runner) checkLogs(ctx context.Context, marker string) (string, error) {
	var n int
	err := r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		entries, err := r.console.searchLogs(ctx, project, marker)
		if err != nil {
			return false, err
		}
		n = 0
		for _, e := range entries {
			if strings.Contains(e.Line, marker) {
				n++
			}
		}
		if n == 0 {
			return false, errors.New("the task's marker is not in the logs yet")
		}
		return true, nil
	})
	return fmt.Sprintf("%d line(s) with the task's marker", n), err
}

func (r *runner) checkMetrics(ctx context.Context) (string, error) {
	q := `kwerft:container_memory_working_set_bytes{namespace="` + project + `"}`
	var n int
	err := r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		var err error
		if n, err = r.console.metricSeries(ctx, q); err != nil {
			return false, err
		}
		if n == 0 {
			return false, errors.New("no series yet")
		}
		return true, nil
	})
	return fmt.Sprintf("%d series for %s", n, q), err
}

func (r *runner) checkAlert(ctx context.Context, since time.Time) (string, error) {
	var found alert
	err := r.waitFor(ctx, 8*time.Minute, func(ctx context.Context) (bool, error) {
		list, err := r.console.alerts(ctx)
		if err != nil {
			return false, err
		}
		for _, a := range list {
			if a.Project == project && a.App == "crash" && a.State == "firing" && (a.Rule == "crash-looping" || a.Rule == "restarts") {
				found = a
				return true, nil
			}
		}
		return false, errors.New("no crash alert for app crash yet")
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s firing %s after the app was created: %s", found.Rule, fmtDuration(r.now().Sub(since)), found.Summary), nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
