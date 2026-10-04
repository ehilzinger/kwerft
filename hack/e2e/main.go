// Command e2e installs a published Kwerft release on a fresh Hetzner Cloud
// server, checks that it works the way a user would use it, and always
// deletes the server again. The e2e workflow (.github/workflows/e2e.yml)
// runs it per release tag and nightly; see RELEASING.md.
//
//	HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.5.0 [-from 0.4.0]
//	go run ./hack/e2e run -version 0.5.0 -dry-run      # print the plan only
//	HCLOUD_TOKEN=… go run ./hack/e2e sweep [-max-age 3h] [-run <id>] [-dry-run]
//	gh release list --json tagName -q '.[].tagName' | go run ./hack/e2e previous -version 0.5.0
//
// The token belongs to a Hetzner Cloud project used for nothing else.
// Everything the harness creates carries the label kwerft-e2e=true, and the
// sweeper deletes only what carries it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
  sweep     delete e2e servers and SSH keys older than -max-age (or of one -run)
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
	fs.DurationVar(&cfg.Timeout, "timeout", 80*time.Minute, "the whole run; cleanup comes on top")
	fs.DurationVar(&cfg.InstallTimeout, "install-timeout", 30*time.Minute, "one installer run")
	fs.DurationVar(&cfg.Poll, "poll", 5*time.Second, "interval between checks while waiting")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	cfg.Version, cfg.From = strings.TrimPrefix(cfg.Version, "v"), strings.TrimPrefix(cfg.From, "v")
	cfg.ServerTypes, cfg.Locations, cfg.Images = splitList(*types), splitList(*locs), splitList(*images)
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
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if *run != "" && !validLabelValue(*run) {
		return usageError{fmt.Errorf("-run %q is not a label value", *run)}
	}
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return usageError{errors.New("HCLOUD_TOKEN is not set")}
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := sweep(sctx, newHcloud(token, *api), time.Now(), *maxAge, *run, *dryRun)
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
