package compose

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/importplan"
)

func convertFile(t *testing.T, name string, opt Options) *importplan.Plan {
	t.Helper()
	src, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Convert(src, opt)
	if err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	return p
}

func app(t *testing.T, p *importplan.Plan, name string) *kwerftv1.AppSpec {
	t.Helper()
	for i := range p.Apps {
		if p.Apps[i].Name == name {
			return &p.Apps[i].Spec
		}
	}
	t.Fatalf("no app %s in %v", name, appNames(p))
	return nil
}

func appNames(p *importplan.Plan) []string {
	var out []string
	for _, a := range p.Apps {
		out = append(out, a.Name)
	}
	return out
}

func env(spec *kwerftv1.AppSpec, name string) *corev1.EnvVar {
	for i := range spec.Env {
		if spec.Env[i].Name == name {
			return &spec.Env[i]
		}
	}
	return nil
}

// warned reports whether a warning about service and key exists whose
// message contains text.
func warned(p *importplan.Plan, service, key, text string) bool {
	return slices.ContainsFunc(p.Warnings, func(w importplan.Warning) bool {
		return w.Service == service && (key == "" || w.Key == key) && strings.Contains(w.Message, text)
	})
}

func secretRef(t *testing.T, spec *kwerftv1.AppSpec, name, set string) {
	t.Helper()
	e := env(spec, name)
	if e == nil || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.Value != "" ||
		e.ValueFrom.SecretKeyRef.Name != set || e.ValueFrom.SecretKeyRef.Key != name {
		t.Errorf("%s = %+v, want a reference to %s/%s", name, e, set, name)
	}
}

func plain(t *testing.T, spec *kwerftv1.AppSpec, name, value string) {
	t.Helper()
	if e := env(spec, name); e == nil || e.ValueFrom != nil || e.Value != value {
		t.Errorf("%s = %+v, want %q", name, e, value)
	}
}

func TestWebAndDatabase(t *testing.T) {
	p := convertFile(t, "web-db.yaml", Options{AppsDomain: "apps.example.com", Env: map[string]string{"DB_PASSWORD": "pw-123"}})
	if got := appNames(p); !slices.Equal(got, []string{"web", "worker", "db", "cache"}) {
		t.Fatalf("apps %v, want the file's order", got)
	}

	web := app(t, p, "web")
	if web.Source.Image == nil || web.Source.Image.Ref != "ghcr.io/acme/shop-web:2.3.1" {
		t.Errorf("web source %+v", web.Source)
	}
	if web.Replicas == nil || *web.Replicas != 2 || web.Size != "medium" {
		t.Errorf("web replicas %v size %s, want 2 and medium (512M limit)", web.Replicas, web.Size)
	}
	if len(web.Command) != 0 || !slices.Equal(web.Args, []string{"node", "server.js", "--port", "3000"}) {
		t.Errorf("web command %q args %q: a Compose command is the args of the image's entrypoint", web.Command, web.Args)
	}
	if len(web.Ports) != 1 || web.Ports[0].Container != 3000 || web.Ports[0].Public != "web.apps.example.com" {
		t.Errorf("web ports %+v, want 3000 on web.apps.example.com", web.Ports)
	}
	if hc := web.HealthCheck; hc == nil || hc.HTTP != "/healthz" || hc.Port != 3000 {
		t.Errorf("web health check %+v", hc)
	}
	plain(t, web, "NODE_ENV", "production") // from the x-env anchor
	plain(t, web, "LOG_LEVEL", "info")      // ${LOG_LEVEL:-info}
	plain(t, web, "REDIS_HOST", "cache")
	plain(t, web, "GREETING", "costs $$5") // $$5 in Compose is $5; Kubernetes needs $$
	secretRef(t, web, "STRIPE_SECRET_KEY", "web-env")
	secretRef(t, web, "SESSION_SECRET", "web-env")
	secretRef(t, web, "DATABASE_URL", "web-env") // a password in the URL
	if !slices.Equal(web.AllowFrom, []string{"cache", "db", "worker"}) {
		t.Errorf("web allowFrom %v: services on one network reach each other", web.AllowFrom)
	}
	if !warned(p, "web", "depends_on", "cache, db") {
		t.Error("no warning about depends_on")
	}
	if !warned(p, "web", "environment", "SESSION_SECRET looks secret but has no value") {
		t.Error("no warning about the secret without a value")
	}

	worker := app(t, p, "worker")
	if !slices.Equal(worker.Command, []string{"node"}) || !slices.Equal(worker.Args, []string{"worker.js", "--queue", "high priority"}) {
		t.Errorf("worker command %q args %q", worker.Command, worker.Args)
	}
	if len(worker.Ports) != 0 || len(worker.AllowFrom) != 0 {
		t.Errorf("worker ports %v allowFrom %v: nothing connects to it", worker.Ports, worker.AllowFrom)
	}

	db := app(t, p, "db")
	if len(db.Ports) != 1 || db.Ports[0].Container != 5432 || db.Ports[0].Public != "" {
		t.Errorf("db ports %+v: 5432 inside the cluster, as web's DATABASE_URL names it", db.Ports)
	}
	if hc := db.HealthCheck; hc == nil || hc.HTTP != "" || hc.Port != 5432 {
		t.Errorf("db health check %+v: pg_isready is a TCP check", hc)
	}
	secretRef(t, db, "POSTGRES_PASSWORD", "db-env")
	plain(t, db, "POSTGRES_USER", "shop")
	if len(db.Volumes) != 1 || db.Volumes[0].Volume != "db-data" || db.Volumes[0].Path != "/var/lib/postgresql/data" {
		t.Errorf("db volumes %+v", db.Volumes)
	}

	cache := app(t, p, "cache")
	if !slices.Equal(cache.Args, []string{"redis-server", "--appendonly", "yes"}) {
		t.Errorf("cache args %q", cache.Args)
	}
	if len(cache.Ports) != 1 || cache.Ports[0].Container != 6379 {
		t.Errorf("cache ports %+v: the image's usual port", cache.Ports)
	}

	if len(p.Volumes) != 2 {
		t.Fatalf("volumes %+v", p.Volumes)
	}
	if v := p.Volumes[0]; v.Name != "db-data" || v.Spec.Size.String() != "20Gi" || v.Spec.Class != "hcloud-volume" {
		t.Errorf("db volume %+v, want x-kwerft's 20Gi hcloud-volume", v)
	}
	if v := p.Volumes[1]; v.Name != "cache-data" || v.Spec.Size.String() != "5Gi" || v.Spec.Class != "local-nvme" {
		t.Errorf("cache volume %+v, want the default 5Gi local-nvme", v)
	}
	if !slices.Contains(p.Renames, importplan.Rename{Kind: "Volume", From: "db_data", To: "db-data"}) {
		t.Errorf("renames %+v", p.Renames)
	}

	if len(p.SecretSets) != 3 {
		t.Fatalf("sets %+v", p.SecretSets)
	}
	ws := p.SecretSets[0]
	if ws.Name != "web-env" || ws.App != "web" || ws.Values["STRIPE_SECRET_KEY"] != "sk_test_123" ||
		ws.Values["DATABASE_URL"] != "postgres://shop:pw-123@db:5432/shop" || !slices.Equal(ws.Missing, []string{"SESSION_SECRET"}) {
		t.Errorf("web's set %+v", ws)
	}
	secretRef(t, worker, "DATABASE_URL", "worker-env")
	if ds := p.SecretSets[2]; ds.Name != "db-env" || ds.Values["POSTGRES_PASSWORD"] != "pw-123" {
		t.Errorf("db's set %+v", ds)
	}
	// The plan as the API answers it: key names, never values.
	raw, _ := json.Marshal(p)
	for _, v := range []string{"sk_test_123", "pw-123"} {
		if strings.Contains(string(raw), v) {
			t.Errorf("the plan's JSON carries the value %q", v)
		}
	}
	if !strings.Contains(string(raw), "STRIPE_SECRET_KEY") {
		t.Error("the plan's JSON lacks the key names")
	}
	if got := p.Objects(); !slices.Equal(got, []string{"Volume/db-data", "Volume/cache-data", "App/web", "App/worker", "App/db", "App/cache", "SecretSet/web-env", "SecretSet/worker-env", "SecretSet/db-env"}) {
		t.Errorf("objects %v", got)
	}
}

func TestNoAppsDomainKeepsPortsInside(t *testing.T) {
	p := convertFile(t, "web-db.yaml", Options{Env: map[string]string{"DB_PASSWORD": "x"}})
	if web := app(t, p, "web"); web.Ports[0].Public != "" {
		t.Errorf("public %q without an apps domain", web.Ports[0].Public)
	}
	if !warned(p, "web", "ports", "No apps domain") {
		t.Error("no warning about the missing apps domain")
	}
	// LOG_LEVEL has a default; DB_PASSWORD is given: nothing reported missing.
	if warned(p, "", "", "Not set") {
		t.Errorf("warnings %+v", p.Warnings)
	}
	p = convertFile(t, "web-db.yaml", Options{})
	if !warned(p, "", "", "Not set, so empty: DB_PASSWORD.") {
		t.Errorf("no warning about DB_PASSWORD: %+v", p.Warnings)
	}
	// Without a value, the database password is a key to set later.
	if db := p.SecretSets[len(p.SecretSets)-1]; db.Name != "db-env" || len(db.Values) != 0 || !slices.Equal(db.Missing, []string{"POSTGRES_PASSWORD"}) {
		t.Errorf("db's set %+v", db)
	}
}

func TestWordPress(t *testing.T) {
	p := convertFile(t, "wordpress.yaml", Options{AppsDomain: "apps.example.com"})
	db, wp := app(t, p, "db"), app(t, p, "wordpress")
	if db.Size != "medium" || wp.Size != "medium" {
		t.Errorf("sizes %s %s, want medium for MariaDB and WordPress", db.Size, wp.Size)
	}
	if !slices.Equal(db.Args, []string{"--default-authentication-plugin=mysql_native_password"}) {
		t.Errorf("db args %q", db.Args)
	}
	for _, port := range db.Ports {
		if port.Public != "" {
			t.Errorf("db port %d is public", port.Container)
		}
	}
	if len(db.Ports) != 2 {
		t.Errorf("db ports %+v, want 3306 and 33060 (expose)", db.Ports)
	}
	if len(wp.Ports) != 1 || wp.Ports[0].Container != 80 || wp.Ports[0].Public != "wordpress.apps.example.com" {
		t.Errorf("wordpress ports %+v", wp.Ports)
	}
	secretRef(t, db, "MYSQL_ROOT_PASSWORD", "db-env")
	secretRef(t, db, "MYSQL_PASSWORD", "db-env")
	plain(t, db, "MYSQL_USER", "wordpress")
	secretRef(t, wp, "WORDPRESS_DB_PASSWORD", "wordpress-env")
	plain(t, wp, "WORDPRESS_DB_HOST", "db")
	if !slices.Equal(db.AllowFrom, []string{"wordpress"}) || !slices.Equal(wp.AllowFrom, []string{"db"}) {
		t.Errorf("allowFrom %v %v", db.AllowFrom, wp.AllowFrom)
	}
	if len(p.Volumes) != 2 || p.Volumes[0].Name != "db-data" || p.Volumes[1].Name != "wp-data" {
		t.Errorf("volumes %+v", p.Volumes)
	}
	if !warned(p, "wordpress", "image", "no version tag") {
		t.Error("no hint to pin wordpress:latest")
	}
	for _, w := range p.Warnings {
		if w.Level == importplan.Warn {
			t.Errorf("unexpected warning %+v: the file maps completely", w)
		}
	}
}

func TestUnsupportedKeysWarn(t *testing.T) {
	p := convertFile(t, "unsupported.yaml", Options{AppsDomain: "apps.example.com"})
	if got := appNames(p); !slices.Equal(got, []string{"legacy-app", "from-git", "db", "app-3d-render"}) {
		t.Fatalf("apps %v", got)
	}
	for _, r := range []importplan.Rename{{Kind: "App", From: "Legacy_App", To: "legacy-app"}, {Kind: "App", From: "3d-render", To: "app-3d-render"}} {
		if !slices.Contains(p.Renames, r) {
			t.Errorf("no rename %+v in %+v", r, p.Renames)
		}
	}
	for _, key := range []string{"network_mode", "privileged", "cap_add", "devices", "pid", "ipc", "user", "working_dir",
		"security_opt", "sysctls", "ulimits", "shm_size", "extra_hosts", "dns", "tmpfs", "volumes_from", "secrets", "configs",
		"env_file", "links", "external_links", "extends", "restart", "read_only", "gpus", "frobnicate", "depends_on",
		"healthcheck", "deploy.mode", "deploy.placement", "volumes[0]", "volumes[1]", "volumes[2]", "volumes[3]", "ports[2]"} {
		if !warned(p, "Legacy_App", key, "") {
			t.Errorf("no warning for %s", key)
		}
	}
	for _, key := range []string{"cap_drop", "container_name", "stop_grace_period", "logging"} {
		if warned(p, "Legacy_App", key, "") {
			t.Errorf("a warning for %s, which changes nothing", key)
		}
	}
	if !warned(p, "Legacy_App", "volumes[0]", "bind mount ./html") || !warned(p, "Legacy_App", "privileged", "Pod Security baseline") {
		t.Error("warnings do not say what happens")
	}
	for _, key := range []string{"secrets", "configs", "include"} {
		if !warned(p, "", key, "") {
			t.Errorf("no warning for the top-level %s", key)
		}
	}
	if !warned(p, "", "volumes.logs", "driver nfs") {
		t.Error("no warning for the volume driver")
	}

	legacy := app(t, p, "legacy-app")
	if legacy.Source.Image == nil || legacy.Source.Image.Ref != "nginx" || !warned(p, "Legacy_App", "build", "deploys the image nginx") {
		t.Errorf("legacy source %+v: build without a Git URL deploys the image", legacy.Source)
	}
	public := map[int32]string{}
	for _, port := range legacy.Ports {
		public[port.Container] = port.Public
	}
	if len(public) != 3 || public[443] != "" || public[9000] != "" || public[8080] != "legacy-app.apps.example.com" {
		t.Errorf("legacy ports %+v: 443 (TLS inside) and 9000 (localhost) stay inside, 8080 (http) is public", legacy.Ports)
	}
	if len(legacy.Volumes) != 1 || legacy.Volumes[0].Volume != "logs" || legacy.HealthCheck != nil {
		t.Errorf("legacy volumes %+v health %+v", legacy.Volumes, legacy.HealthCheck)
	}

	git := app(t, p, "from-git").Source.Git
	if git == nil || git.Repository != "https://github.com/acme/api.git" || git.Branch != "release" || git.Path != "/services/api" || git.Dockerfile != "Dockerfile.prod" {
		t.Errorf("from-git source %+v", git)
	}
	if !warned(p, "local-build", "build", "Not imported") || !warned(p, "local-build", "build.args", "") {
		t.Error("local-build is not reported as skipped")
	}
	if !warned(p, "debug-tools", "profiles", "Not imported") {
		t.Error("debug-tools (a profile) is not reported as skipped")
	}
	if !warned(p, "db", "volumes", "lost when the app restarts") {
		t.Error("no warning that db keeps its data in the container")
	}
	render := app(t, p, "app-3d-render")
	if render.Egress != "none" || render.HealthCheck != nil || !warned(p, "3d-render", "healthcheck", "2m") {
		t.Errorf("3d-render egress %q health %+v: internal network, slow start", render.Egress, render.HealthCheck)
	}
	if !slices.Equal(render.AllowFrom, nil) || len(app(t, p, "db").AllowFrom) != 2 {
		t.Errorf("allowFrom across networks: render %v db %v", render.AllowFrom, app(t, p, "db").AllowFrom)
	}
}

func TestPlausibleCommunityEdition(t *testing.T) {
	p := convertFile(t, "plausible.yaml", Options{AppsDomain: "apps.example.com", Env: map[string]string{"BASE_URL": "https://plausible.example.com"}})
	if got := appNames(p); !slices.Equal(got, []string{"plausible-db", "plausible-events-db", "plausible"}) {
		t.Fatalf("apps %v", got)
	}
	pl := app(t, p, "plausible")
	if len(pl.Args) != 3 || pl.Args[0] != "sh" || pl.Args[2] != "/entrypoint.sh db createdb && /entrypoint.sh db migrate && /entrypoint.sh run" {
		t.Errorf("plausible args %q", pl.Args)
	}
	plain(t, pl, "BASE_URL", "https://plausible.example.com")
	if env(pl, "DATABASE_URL") != nil || !warned(p, "plausible", "environment", "DATABASE_URL has no value") {
		t.Error("DATABASE_URL without a value is not left out with a warning")
	}
	if !warned(p, "plausible", "environment", "SECRET_KEY_BASE looks secret") {
		t.Error("SECRET_KEY_BASE without a value is not reported")
	}
	if ev := app(t, p, "plausible-events-db"); ev.Size != "large" || len(ev.Volumes) != 2 {
		t.Errorf("events db size %s volumes %+v", ev.Size, ev.Volumes)
	}
	if n := len(p.Volumes); n != 4 {
		t.Errorf("%d volumes, want 4", n)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"", "empty"},
		{"services: [", "not valid YAML"},
		{"- a\n- b\n", "mapping"},
		{"version: '3'\n", "no services"},
		{"services:\n  a:\n    build: .\n", "No service of the file can become an app"},
		{"services:\n  a:\n    image: x:${TAG:?set the tag}\n", "TAG is not set: set the tag"},
		{"services:\n  a:\n    image: x:${TAG\n", "closing }"},
	} {
		if _, err := Convert([]byte(tc.src), Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error %v, want %q", tc.src, err, tc.want)
		}
	}
	if _, err := Convert(make([]byte, MaxSize+1), Options{}); err == nil {
		t.Error("an oversized file is read")
	}
}

func TestLongAndClashingNames(t *testing.T) {
	src := "services:\n" +
		"  " + strings.Repeat("very_long_service_name_", 4) + ":\n    image: a:1\n" +
		"  web_1:\n    image: a:1\n" +
		"  web-1:\n    image: a:1\n"
	p, err := Convert([]byte(src), Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := appNames(p)
	if len(got[0]) > importplan.MaxAppName || !importplan.ValidAppName(got[0]) {
		t.Errorf("%q is not a valid App name of at most %d characters", got[0], importplan.MaxAppName)
	}
	if got[1] != "web-1" || got[2] != "web-1-2" {
		t.Errorf("names %v, want web-1 and web-1-2", got)
	}
}

func TestInterpolation(t *testing.T) {
	in := &interpolator{env: map[string]string{"A": "a", "E": ""}, missing: map[string]bool{}}
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"$$HOME", "$HOME"},
		{"$A/$A", "a/a"},
		{"${A}", "a"},
		{"${E:-d}", "d"},
		{"${E-d}", ""},
		{"${U-d}", "d"},
		{"${U:-${A}-x}", "a-x"},
		{"${A:+set}", "set"},
		{"${E:+set}", ""},
		{"${E+set}", "set"},
		{"${U}", ""},
		{"cost: 5$", "cost: 5$"},
	} {
		got, err := in.expand(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("expand(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if !in.missing["U"] || in.missing["E"] || in.missing["A"] {
		t.Errorf("missing %v, want only U", in.missing)
	}
	for _, bad := range []string{"${E:?empty}", "${U?}", "${}", "${A!}"} {
		if _, err := in.expand(bad); err == nil {
			t.Errorf("expand(%q) succeeds", bad)
		}
	}
}

func TestSplitShell(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"redis-server --appendonly yes", []string{"redis-server", "--appendonly", "yes"}},
		{`sh -c "echo \"hi\" && exit 0"`, []string{"sh", "-c", `echo "hi" && exit 0`}},
		{`a 'b c' d\ e ""`, []string{"a", "b c", "d e", ""}},
		{"  ", nil},
	} {
		got, err := splitShell(tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("splitShell(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := splitShell(`echo "open`); err == nil {
		t.Error("an unclosed quote splits")
	}
}

func TestLooksSecret(t *testing.T) {
	for name, want := range map[string]bool{
		"POSTGRES_PASSWORD": true, "DB_PASS": true, "SECRET_KEY_BASE": true, "GITHUB_TOKEN": true, "API_KEY": true,
		"AWS_SECRET_ACCESS_KEY": true, "MINIO_ROOT_PASSWORD": true, "stripe.key": true, "SMTP_PWD": true,
		"POSTGRES_USER": false, "KEYCLOAK_URL": false, "PASSENGER_COUNT": false, "LOG_LEVEL": false, "MONKEY": false,
	} {
		if got := LooksSecret(name); got != want {
			t.Errorf("LooksSecret(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestPortSyntax(t *testing.T) {
	for _, tc := range []struct {
		in    string
		port  int32
		udp   bool
		local bool
	}{
		{"80", 80, false, false},
		{"8080:80", 80, false, false},
		{"127.0.0.1:8080:80", 80, false, true},
		{"[::1]:8080:80", 80, false, true},
		{"53:53/udp", 53, true, false},
	} {
		p, err := parsePort(tc.in)
		if err != nil || p.container != tc.port || (p.protocol == corev1.ProtocolUDP) != tc.udp || p.local != tc.local {
			t.Errorf("parsePort(%q) = %+v, %v", tc.in, p, err)
		}
	}
	for _, bad := range []string{"8000-8010:8000-8010", "http", "70000"} {
		if _, err := parsePort(bad); err == nil {
			t.Errorf("parsePort(%q) succeeds", bad)
		}
	}
}

func TestXKwerft(t *testing.T) {
	src := `services:
  site:
    image: caddy:2.10
    ports: ["80:80", "8443:8000"]
    x-kwerft:
      public: www.example.org
      size: large
      egress: all
  admin:
    image: acme/admin:1
    ports: ["9000:9000"]
    x-kwerft:
      public: false
`
	p, err := Convert([]byte(src), Options{AppsDomain: "apps.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	site := app(t, p, "site")
	if site.Size != "large" || site.Egress != "all" || site.Ports[0].Public != "www.example.org" || site.Ports[1].Public != "site-8000.apps.example.com" {
		t.Errorf("site %+v", site)
	}
	if admin := app(t, p, "admin"); admin.Ports[0].Public != "" {
		t.Errorf("admin is public despite public: false")
	}
}
