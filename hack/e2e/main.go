// Command e2e installs a published Kwerft release on a fresh Hetzner Cloud
// server, checks that it works the way a user would use it, and always
// deletes the server again. The e2e workflow (.github/workflows/e2e.yml)
// runs it per release tag and nightly; see RELEASING.md.
//
//	HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.5.0 [-from 0.4.0]
//	go run ./hack/e2e run -version 0.5.0 -dry-run      # print the plan only
//	HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.6.0 -from 0.5.0 -via-console   # upgrade through the console
//	HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.6.0 -from 0.5.0 -fault         # … failing: RolledBack
//	HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.6.0 -k3s                       # 3 nodes, k3s patch upgrade
//	HCLOUD_TOKEN=… E2E_S3_…=… go run ./hack/e2e run -version 0.6.0 -restore        # backup, new server, --restore
//	HCLOUD_TOKEN=… go run ./hack/e2e sweep [-max-age 3h] [-run <id> [-s3]] [-dry-run]
//	gh release list --json tagName -q '.[].tagName' | go run ./hack/e2e previous -version 0.5.0
//
// The token belongs to a Hetzner Cloud project used for nothing else.
// Everything the harness creates carries the label kwerft-e2e=true, and the
// sweeper deletes only what carries it. The restore run writes to an S3
// bucket under the prefix <run id>/ only, and deletes that prefix again.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(ctx, os.Args[2:], os.Stdout)
	case "sweep":
		err = cmdSweep(ctx, os.Args[2:], os.Stdout)
	case "previous", "latest":
		err = cmdVersions(os.Args[1], os.Args[2:], os.Stdin, os.Stdout)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		var u usageError
		if errors.As(err, &u) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ error }

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: e2e <command> [flags]

  run       install a release on a new Hetzner Cloud server, check it, delete the server
            (-via-console, -fault, -k3s, -restore: the Phase 6 runs)
  sweep     delete e2e servers, networks and SSH keys older than -max-age (or of one -run)
  previous  print the newest stable release older than -version (tags on stdin)
  latest    print the newest stable release (tags on stdin)

HCLOUD_TOKEN (environment only) is the Hetzner Cloud API token of the test
project. "e2e <command> -h" lists a command's flags.
`)
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// envOr is the environment variable name's value, or def.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func cmdRun(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stdout)
	var (
		cfg     config
		types   = fs.String("server-types", envOr("E2E_SERVER_TYPES", "cx33,cx43"), "server types to try in order (comma-separated)")
		locs    = fs.String("locations", envOr("E2E_LOCATIONS", "nbg1,fsn1,hel1"), "locations to try in order")
		images  = fs.String("images", "ubuntu-26.04,ubuntu-24.04", "images to try in order")
		summary = fs.String("summary", os.Getenv("GITHUB_STEP_SUMMARY"), "append a Markdown report to this file")
		dryRun  = fs.Bool("dry-run", false, "print the plan; create nothing (no token needed)")
	)
	fs.StringVar(&cfg.Version, "version", "", "release under test, e.g. 0.5.0 (required)")
	fs.StringVar(&cfg.From, "from", "", "previous release to install first and upgrade from (default: a fresh install)")
	fs.StringVar(&cfg.RunID, "run-id", "local-"+strconv.FormatInt(time.Now().Unix(), 10), "names and labels this run's resources")
	fs.StringVar(&cfg.InstallerURL, "installer-url", "https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/v{version}/install.sh", "published installer; {version} is replaced")
	fs.StringVar(&cfg.GitRepo, "git-repo", "https://github.com/traefik/whoami", "public repository with a Dockerfile serving HTTP on port 80")
	fs.StringVar(&cfg.GitBranch, "git-branch", "master", "branch of -git-repo")
	fs.StringVar(&cfg.APIBase, "api", hcloudAPI, "Hetzner Cloud API")
	fs.BoolVar(&cfg.ViaConsole, "via-console", false, "upgrade -from → -version through the console's API (Settings › Updates) instead of re-running the installer")
	fs.BoolVar(&cfg.Fault, "fault", false, "like -via-console, with a failure injected after the Kwerft stage (chart value e2e.faults): expect RolledBack")
	fs.BoolVar(&cfg.K3s, "k3s", false, "3 nodes (-workers + 1) on a k3s one patch behind, then a Kubernetes upgrade through the console")
	fs.StringVar(&cfg.K3sFrom, "k3s-from", "", "-k3s: the k3s to install (KWERFT_K3S_VERSION; default: one patch behind the installer's pin)")
	fs.StringVar(&cfg.K3sTo, "k3s-to", "", "-k3s: the k3s to upgrade to (default: the installer's pin)")
	fs.IntVar(&cfg.Workers, "workers", 2, "-k3s: worker nodes")
	fs.BoolVar(&cfg.Restore, "restore", false, "back up to the -s3-* bucket, delete the server, install.sh --restore latest on a new one, check (needs E2E_S3_ACCESS_KEY, E2E_S3_SECRET_KEY)")
	fs.StringVar(&cfg.S3.Endpoint, "s3-endpoint", os.Getenv("E2E_S3_ENDPOINT"), "-restore: the bucket's S3 endpoint, https://…")
	fs.StringVar(&cfg.S3.Bucket, "s3-bucket", os.Getenv("E2E_S3_BUCKET"), "-restore: the bucket; the run uses the prefix <run id>/ and deletes it at the end")
	fs.StringVar(&cfg.S3.Region, "s3-region", os.Getenv("E2E_S3_REGION"), "-restore: the bucket's region (default: from a Hetzner endpoint)")
	fs.StringVar(&cfg.ChartRef, "chart", defaultChartRef, "the Kwerft chart, for chart values a run sets (-fault)")
	staging := fs.String("staging-roots", strings.Join(defaultStagingRoots, ","), "-k3s: Let's Encrypt staging root certificates the joining nodes trust")
	fs.DurationVar(&cfg.Timeout, "timeout", 0, "the whole run; cleanup comes on top (default 80m, 120m with -k3s or -restore)")
	fs.DurationVar(&cfg.InstallTimeout, "install-timeout", 30*time.Minute, "one installer run")
	fs.DurationVar(&cfg.Poll, "poll", 5*time.Second, "interval between checks while waiting")
	fs.DurationVar(&cfg.ProbeEvery, "probe-every", 2*time.Second, "interval between requests to an App during an upgrade")
	fs.DurationVar(&cfg.MaxGap, "max-gap", 0, "fail when an App did not answer for longer during an upgrade (0: only report the longest gap)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	cfg.Version, cfg.From = strings.TrimPrefix(cfg.Version, "v"), strings.TrimPrefix(cfg.From, "v")
	cfg.ServerTypes, cfg.Locations, cfg.Images = splitList(*types), splitList(*locs), splitList(*images)
	cfg.StagingRoots = splitList(*staging)
	if cfg.Fault {
		cfg.ViaConsole = true
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 80 * time.Minute
		if cfg.K3s || cfg.Restore {
			cfg.Timeout = 120 * time.Minute
		}
	}
	if err := cfg.validate(); err != nil {
		return usageError{err}
	}

	if *dryRun {
		fmt.Fprintf(stdout, "Dry run — %s, run %s. Nothing is created.\n\n", cfg.scenario(), cfg.RunID)
		for i, s := range cfg.plan() {
			fmt.Fprintf(stdout, "%2d. %s\n", i+1, s)
		}
		return nil
	}
	cfg.Token = os.Getenv("HCLOUD_TOKEN")
	if cfg.Token == "" {
		return usageError{errors.New("HCLOUD_TOKEN is not set")}
	}
	if cfg.Restore {
		cfg.S3.AccessKey, cfg.S3.SecretKey = os.Getenv("E2E_S3_ACCESS_KEY"), os.Getenv("E2E_S3_SECRET_KEY")
		if cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
			return usageError{errors.New("-restore needs E2E_S3_ACCESS_KEY and E2E_S3_SECRET_KEY in the environment")}
		}
		ghMask(stdout)(cfg.S3.SecretKey)
	}

	r := &runner{
		cfg:       cfg,
		cloud:     newHcloud(cfg.Token, cfg.APIBase),
		dial:      dialSSH,
		transport: newHTTPClient,
		log:       stdout,
		now:       time.Now,
		mask:      ghMask(stdout),
		rep:       &report{Title: "Kwerft e2e"},
	}
	fmt.Fprintf(stdout, "%s, run %s:\n", cfg.scenario(), cfg.RunID)
	for i, s := range cfg.plan() {
		fmt.Fprintf(stdout, "%2d. %s\n", i+1, s)
	}
	r.run(ctx)
	if err := writeSummary(*summary, r.rep.markdown); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: write summary:", err)
	}
	r.rep.markdown(stdout)
	if !r.rep.passed() {
		return errors.New("the e2e run failed")
	}
	return nil
}

// ghMask hides secrets in GitHub Actions logs.
func ghMask(w io.Writer) func(string) {
	return func(s string) {
		if os.Getenv("GITHUB_ACTIONS") == "true" && s != "" {
			fmt.Fprintf(w, "::add-mask::%s\n", s)
		}
	}
}

func writeSummary(path string, render func(io.Writer)) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	render(f)
	fmt.Fprintln(f)
	return f.Close()
}

func cmdSweep(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.SetOutput(stdout)
	maxAge := fs.Duration("max-age", 3*time.Hour, "delete e2e resources older than this")
	run := fs.String("run", "", "delete every resource of this run, whatever its age")
	dryRun := fs.Bool("dry-run", false, "list what would be deleted")
	api := fs.String("api", hcloudAPI, "Hetzner Cloud API")
	summary := fs.String("summary", os.Getenv("GITHUB_STEP_SUMMARY"), "append a Markdown report to this file")
	bucketToo := fs.Bool("s3", false, "with -run: also delete the objects under <run>/ in the restore runs' bucket (E2E_S3_ENDPOINT, E2E_S3_BUCKET, E2E_S3_ACCESS_KEY, E2E_S3_SECRET_KEY)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if *run != "" && !validLabelValue(*run) {
		return usageError{fmt.Errorf("-run %q is not a label value", *run)}
	}
	if *bucketToo && *run == "" {
		return usageError{errors.New("-s3 needs -run: only one run's prefix is ever deleted")}
	}
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return usageError{errors.New("HCLOUD_TOKEN is not set")}
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := sweep(sctx, newHcloud(token, *api), time.Now(), *maxAge, *run, *dryRun)
	if *bucketToo {
		res, err = sweepBucket(sctx, s3FromEnv(), nil, *run, *dryRun, res, err)
	}
	render := func(w io.Writer) {
		scope := fmt.Sprintf("older than %s", *maxAge)
		if *run != "" {
			scope = "of run " + *run
		}
		fmt.Fprintf(w, "## Kwerft e2e sweeper: resources %s\n\n", scope)
		if len(res.Deleted) == 0 {
			fmt.Fprintln(w, "Nothing to delete.")
		}
		for _, d := range res.Deleted {
			fmt.Fprintf(w, "- %s\n", d)
		}
		for _, e := range res.Errors {
			fmt.Fprintf(w, "- **failed**: %s\n", e)
		}
		if res.Kept > 0 {
			fmt.Fprintf(w, "\n%d younger e2e resource(s) left alone (a run in progress).\n", res.Kept)
		}
	}
	render(stdout)
	if werr := writeSummary(*summary, render); werr != nil {
		fmt.Fprintln(os.Stderr, "e2e: write summary:", werr)
	}
	return err
}

func s3FromEnv() s3Config {
	return s3Config{Endpoint: os.Getenv("E2E_S3_ENDPOINT"), Region: os.Getenv("E2E_S3_REGION"), Bucket: os.Getenv("E2E_S3_BUCKET"),
		AccessKey: os.Getenv("E2E_S3_ACCESS_KEY"), SecretKey: os.Getenv("E2E_S3_SECRET_KEY")}
}

// sweepBucket deletes a run's prefix in the bucket after the servers'
// sweep (res, err), adding to its result.
func sweepBucket(ctx context.Context, cfg s3Config, hc *http.Client, run string, dryRun bool, res sweepResult, err error) (sweepResult, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		res.Errors = append(res.Errors, "bucket: E2E_S3_ENDPOINT, E2E_S3_BUCKET, E2E_S3_ACCESS_KEY and E2E_S3_SECRET_KEY are needed for -s3")
		return res, errors.Join(err, errors.New("the bucket is not configured"))
	}
	b, prefix := newBucket(cfg, hc), run+"/"
	if dryRun {
		keys, lerr := b.list(ctx, prefix)
		if lerr != nil {
			res.Errors = append(res.Errors, "bucket: "+lerr.Error())
			return res, errors.Join(err, lerr)
		}
		if len(keys) > 0 {
			res.Deleted = append(res.Deleted, fmt.Sprintf("would delete %d object(s) under %s/%s", len(keys), cfg.Bucket, prefix))
		}
		return res, err
	}
	n, derr := b.deletePrefix(ctx, prefix)
	if n > 0 {
		res.Deleted = append(res.Deleted, fmt.Sprintf("deleted %d object(s) under %s/%s", n, cfg.Bucket, prefix))
	}
	if derr != nil {
		res.Errors = append(res.Errors, "bucket: "+derr.Error())
		return res, errors.Join(err, derr)
	}
	return res, err
}

func cmdVersions(cmd string, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stdout)
	version := fs.String("version", "", "the release under test (previous only)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	var (
		v   string
		err error
	)
	if cmd == "latest" {
		v, err = latestStable(stdin)
	} else {
		if !validVersion(*version) {
			return usageError{fmt.Errorf("-version %q is not a release version", *version)}
		}
		v, err = previousStable(stdin, *version)
	}
	if err != nil {
		return err
	}
	if v != "" {
		fmt.Fprintln(stdout, v)
	}
	return nil
}
