package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/templates"
)

const importCompose = `services:
  web:
    image: ghcr.io/acme/shop-web:2.3.1
    command: ["node", "server.js"]
    ports: ["8080:3000"]
    environment:
      DATABASE_URL: postgres://shop:${DB_PASSWORD}@db:5432/shop
      STRIPE_SECRET_KEY: sk_live_import_4242
      LOG_LEVEL: info
  db:
    image: postgres:17.6
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes:
      - db_data:/var/lib/postgresql/data
volumes:
  db_data:
`

const importPassword = "db-pw-from-dotenv-77"

func importBody(dryRun bool) map[string]any {
	return map[string]any{"compose": importCompose, "env": map[string]string{"DB_PASSWORD": importPassword}, "dryRun": dryRun}
}

func importPath(project string) string { return "/api/v1/projects/" + project + "/import/compose" }

func decodeInto(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// nothingIn fails when the project has any App, Volume or SecretSet.
func nothingIn(t *testing.T, project string) {
	t.Helper()
	ctx := context.Background()
	var apps kwerftv1.AppList
	var vols kwerftv1.VolumeList
	var sets kwerftv1.SecretSetList
	for _, l := range []client.ObjectList{&apps, &vols, &sets} {
		if err := cluster.admin.List(ctx, l, client.InNamespace(project)); err != nil {
			t.Fatal(err)
		}
	}
	if len(apps.Items)+len(vols.Items)+len(sets.Items) > 0 {
		t.Errorf("%s has %d apps, %d volumes, %d sets; want nothing", project, len(apps.Items), len(vols.Items), len(sets.Items))
	}
}

func TestComposeImportDryRunThenApply(t *testing.T) {
	c := newConsole(t)
	c.project(t, "imp-a")

	// The dry run: the plan, nothing created, no value in the answer.
	var plan importJSON
	code, body := c.dev.text(t, "POST", importPath("imp-a"), importBody(true))
	if code != http.StatusOK || strings.Contains(body, "sk_live_import_4242") || strings.Contains(body, importPassword) {
		t.Fatalf("dry run: %d %s", code, body)
	}
	decodeInto(t, body, &plan)
	if !plan.DryRun || len(plan.Problems) != 0 || len(plan.Apps) != 2 || len(plan.Volumes) != 1 || len(plan.SecretSets) != 2 {
		t.Fatalf("dry run plan %+v", plan)
	}
	nothingIn(t, "imp-a")

	// Apply.
	code, body = c.dev.text(t, "POST", importPath("imp-a"), importBody(false))
	if code != http.StatusCreated || strings.Contains(body, "sk_live_import_4242") || strings.Contains(body, importPassword) {
		t.Fatalf("apply: %d %s", code, body)
	}
	var out importJSON
	decodeInto(t, body, &out)
	want := []string{"Volume/db-data", "App/web", "App/db", "SecretSet/web-env", "SecretSet/db-env"}
	if !slices.Equal(out.Created, want) {
		t.Errorf("created %v, want %v", out.Created, want)
	}
	ctx := context.Background()
	var web kwerftv1.App
	if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "imp-a", Name: "web"}, &web); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(web.Spec.Args, []string{"node", "server.js"}) || web.Spec.Ports[0].Container != 3000 {
		t.Errorf("web spec %+v", web.Spec)
	}
	var set kwerftv1.SecretSet
	if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "imp-a", Name: "web-env"}, &set); err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(&set); owner == nil || owner.Kind != "App" || owner.Name != "web" || owner.UID != web.UID {
		t.Errorf("web-env owner %+v, want the App web", owner)
	}
	data := secretData(t, "imp-a", "web-env")
	if string(data["STRIPE_SECRET_KEY"]) != "sk_live_import_4242" || string(data["DATABASE_URL"]) != "postgres://shop:"+importPassword+"@db:5432/shop" {
		t.Errorf("web-env holds %v", data)
	}
	if got := secretData(t, "imp-a", "db-env")["POSTGRES_PASSWORD"]; string(got) != importPassword {
		t.Errorf("db-env POSTGRES_PASSWORD = %q", got)
	}
	setAt(t, c.viewer, "imp-a", "web-env", hasKey("STRIPE_SECRET_KEY", "developer@example.com", controllers.SourceSet))
	var vol kwerftv1.Volume
	if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "imp-a", Name: "db-data"}, &vol); err != nil || vol.Spec.Size.String() != "5Gi" {
		t.Errorf("volume %+v %v", vol.Spec, err)
	}

	// The audit log names what was created, never a value.
	entries, err := c.store.RecentAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range entries {
		if strings.Contains(e.Detail+e.Target, "sk_live") || strings.Contains(e.Detail+e.Target, importPassword) {
			t.Errorf("audit entry %+v carries a value", e)
		}
		if e.Actor == "developer@example.com" {
			actions = append(actions, e.Action+" "+e.Target)
		}
	}
	for _, a := range []string{"import.compose imp-a", "app.create imp-a/web", "volume.create imp-a/db-data", "secret_set.create imp-a/web-env", "secret.set imp-a/web-env/STRIPE_SECRET_KEY"} {
		if !slices.Contains(actions, a) {
			t.Errorf("no audit entry %q in %v", a, actions)
		}
	}

	// Again: every name is taken, so nothing is created and the answer says why.
	var again struct {
		importJSON
		Error string `json:"error"`
	}
	if code := c.dev.do(t, "POST", importPath("imp-a"), importBody(false), &again); code != http.StatusConflict ||
		len(again.Problems) != 5 || !strings.Contains(again.Error, "already exists") {
		t.Errorf("importing twice: %d %+v", code, again)
	}
}

func TestComposeImportIsForDevelopers(t *testing.T) {
	c := newConsole(t)
	c.project(t, "imp-v")
	for _, dry := range []bool{true, false} {
		if code, body := c.viewer.text(t, "POST", importPath("imp-v"), importBody(dry)); code != http.StatusForbidden {
			t.Errorf("viewer imports (dry run %v): %d %s", dry, code, body)
		}
	}
	nothingIn(t, "imp-v")
	// Invalid files answer for the field.
	var e apiError
	if code := c.dev.do(t, "POST", importPath("imp-v"), map[string]any{"compose": "services: [", "dryRun": true}, &e); code != http.StatusUnprocessableEntity || e.Field != "compose" {
		t.Errorf("invalid YAML: %d %+v", code, e)
	}
	// Owners import too.
	if code := c.owner.do(t, "POST", importPath("imp-v"), importBody(true), nil); code != http.StatusOK {
		t.Errorf("owner dry run: %d", code)
	}
}

func TestComposeImportRollsBack(t *testing.T) {
	failAt := "App/db"
	c := newConsole(t, func(cfg *Config) {
		cfg.importFault = func(object string) error {
			if object == failAt {
				return apierrors.NewInternalError(errors.New("injected"))
			}
			return nil
		}
	})
	c.project(t, "imp-r")
	var e apiError
	if code := c.dev.do(t, "POST", importPath("imp-r"), importBody(false), &e); code != http.StatusBadGateway || !strings.Contains(e.Error, "undone") {
		t.Fatalf("failed import: %d %+v", code, e)
	}
	// Volumes wait for their users to go (volume-protection); everything
	// goes in the end.
	eventually(t, func() error {
		ctx := context.Background()
		var apps kwerftv1.AppList
		var vols kwerftv1.VolumeList
		var sets kwerftv1.SecretSetList
		for _, l := range []client.ObjectList{&apps, &vols, &sets} {
			if err := cluster.admin.List(ctx, l, client.InNamespace("imp-r")); err != nil {
				return err
			}
		}
		if n := len(apps.Items) + len(vols.Items) + len(sets.Items); n > 0 {
			return fmt.Errorf("%d objects left", n)
		}
		return nil
	})
	// A failure while writing values undoes the Apps too.
	failAt = "SecretSet/db-env"
	if code := c.dev.do(t, "POST", importPath("imp-r"), importBody(false), &e); code != http.StatusBadGateway {
		t.Fatalf("failed import: %d %+v", code, e)
	}
	eventually(t, func() error {
		var apps kwerftv1.AppList
		if err := cluster.admin.List(context.Background(), &apps, client.InNamespace("imp-r")); err != nil || len(apps.Items) > 0 {
			return fmt.Errorf("apps left: %d %v", len(apps.Items), err)
		}
		return nil
	})
}

func templatePath(project, id string) string {
	return "/api/v1/projects/" + project + "/templates/" + id
}

func TestTemplates(t *testing.T) {
	c := newConsole(t)
	c.project(t, "tpl-a")

	var list []templates.Template
	if code := c.viewer.do(t, "GET", "/api/v1/templates", nil, &list); code != http.StatusOK || len(list) != len(templates.List()) {
		t.Fatalf("catalog: %d %d", code, len(list))
	}
	if list[0].Parameters[0].Key != "name" || list[0].Images == nil || list[0].Pinned == "" {
		t.Errorf("catalog entry %+v", list[0])
	}

	params := map[string]any{"parameters": map[string]string{"name": "main-db", "allowFrom": "api"}}
	if code := c.viewer.do(t, "POST", templatePath("tpl-a", "postgresql"), params, nil); code != http.StatusForbidden {
		t.Errorf("viewer applies a template: %d", code)
	}
	var e apiError
	if code := c.dev.do(t, "POST", templatePath("tpl-a", "postgresql"), map[string]any{"parameters": map[string]string{"name": "Main"}}, &e); code != http.StatusUnprocessableEntity || e.Field != "parameters.name" {
		t.Errorf("bad parameter: %d %+v", code, e)
	}
	if code := c.dev.do(t, "POST", templatePath("tpl-a", "oracle"), params, nil); code != http.StatusNotFound {
		t.Errorf("unknown template: %d", code)
	}
	// n8n needs a hostname: no apps domain here to suggest one.
	if code := c.dev.do(t, "POST", templatePath("tpl-a", "n8n"), map[string]any{"dryRun": true}, &e); code != http.StatusUnprocessableEntity || e.Field != "parameters.hostname" {
		t.Errorf("n8n without a hostname: %d %+v", code, e)
	}

	var out importJSON
	if code := c.dev.do(t, "POST", templatePath("tpl-a", "postgresql"), params, &out); code != http.StatusCreated {
		t.Fatalf("apply: %d %+v", code, out)
	}
	if !slices.Equal(out.Created, []string{"SecretSet/main-db", "App/main-db"}) || !strings.Contains(out.Notes, "DATABASE_URL") {
		t.Errorf("created %v notes %q", out.Created, out.Notes)
	}
	// The reconciler generates the password and derives the URL.
	setAt(t, c.dev, "tpl-a", "main-db", func(s secretSetJSON) error {
		if err := hasKey("PASSWORD", "", controllers.SourceGenerated)(s); err != nil {
			return err
		}
		return hasKey("DATABASE_URL", "", controllers.SourceDerived)(s)
	})
	data := secretData(t, "tpl-a", "main-db")
	if want := "postgres://app:" + string(data["PASSWORD"]) + "@main-db:5432/app"; string(data["DATABASE_URL"]) != want || len(data["PASSWORD"]) != 43 {
		t.Errorf("DATABASE_URL %q, want %q", data["DATABASE_URL"], want)
	}
	app := appAt(t, c.dev, "tpl-a", "main-db", func(a *kwerftv1.App) error { return nil })
	if !slices.Equal(app.Spec.AllowFrom, []string{"api"}) || app.Spec.Volumes[0].Size.String() != "10Gi" {
		t.Errorf("app spec %+v", app.Spec)
	}
}

// TestEveryTemplatePassesAppValidation applies each template in a project
// of its own, as a developer, and runs the pods its workloads would start
// past the namespace's Pod Security admission (baseline), as a dry run.
func TestEveryTemplatePassesAppValidation(t *testing.T) {
	c := newConsole(t)
	for _, tpl := range templates.List() {
		t.Run(tpl.ID, func(t *testing.T) {
			project := "tpl-" + tpl.ID
			c.project(t, project)
			params := map[string]string{}
			for _, p := range tpl.Parameters {
				if p.Type == templates.TypeHostname && p.Required {
					params[p.Key] = tpl.ID + ".example.com"
				}
			}
			// The check bites: a privileged pod is refused here.
			bad := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: project, Name: "privileged"}, Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "x", Image: "busybox:1.37", SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}}}}
			if err := cluster.admin.Create(context.Background(), bad, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatalf("a privileged pod: %v, want Forbidden by Pod Security", err)
			}
			var out importJSON
			if code := c.dev.do(t, "POST", templatePath(project, tpl.ID), map[string]any{"parameters": params}, &out); code != http.StatusCreated {
				t.Fatalf("apply: %d %+v", code, out)
			}
			for _, a := range out.Apps {
				tmpl := podTemplateOf(t, project, a.Name)
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: project, GenerateName: a.Name + "-"}, Spec: tmpl.Spec}
				if err := cluster.admin.Create(context.Background(), pod, client.DryRunAll); err != nil {
					t.Errorf("%s: a pod of it is refused: %v", a.Name, err)
				}
				for _, ctr := range tmpl.Spec.Containers {
					if ctr.Resources.Limits.Memory().IsZero() || ctr.Resources.Requests.Cpu().IsZero() {
						t.Errorf("%s: resources %+v", a.Name, ctr.Resources)
					}
				}
			}
		})
	}
}

// podTemplateOf waits for the App reconciler's Deployment or StatefulSet.
func podTemplateOf(t *testing.T, project, app string) corev1.PodTemplateSpec {
	t.Helper()
	var tmpl corev1.PodTemplateSpec
	eventually(t, func() error {
		ctx := context.Background()
		key := client.ObjectKey{Namespace: project, Name: app}
		var d appsv1.Deployment
		if err := cluster.admin.Get(ctx, key, &d); err == nil {
			tmpl = d.Spec.Template
			return nil
		}
		var s appsv1.StatefulSet
		if err := cluster.admin.Get(ctx, key, &s); err != nil {
			return fmt.Errorf("no workload for %s yet: %w", app, err)
		}
		tmpl = *s.Spec.Template.DeepCopy()
		// The StatefulSet adds each replica's claims to its pods.
		for _, claim := range s.Spec.VolumeClaimTemplates {
			tmpl.Spec.Volumes = append(tmpl.Spec.Volumes, corev1.Volume{Name: claim.Name, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name + "-" + app + "-0"}}})
		}
		return nil
	})
	return tmpl
}
