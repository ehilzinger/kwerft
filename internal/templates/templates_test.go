package templates

import (
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/importplan"
)

func TestCatalog(t *testing.T) {
	want := []string{"minio", "n8n", "plausible", "postgresql", "redis"}
	var got []string
	for _, tpl := range List() {
		got = append(got, tpl.ID)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("catalog %v, want %v", got, want)
	}
	if Get("postgresql") == nil || Get("nope") != nil {
		t.Error("Get")
	}
}

// TestEveryTemplateRenders renders each template with its defaults and
// checks what the API cannot: the objects fit together.
func TestEveryTemplateRenders(t *testing.T) {
	for _, tpl := range List() {
		t.Run(tpl.ID, func(t *testing.T) {
			p, err := tpl.Render(nil, "apps.example.com")
			if err != nil {
				t.Fatal(err)
			}
			checkPlan(t, tpl, p)
			if p.Notes == "" {
				t.Error("no notes")
			}
		})
	}
}

func checkPlan(t *testing.T, tpl *Template, p *importplan.Plan) {
	t.Helper()
	if len(p.Apps) == 0 {
		t.Fatal("no apps")
	}
	sets := map[string][]string{} // set → its keys
	for _, s := range p.SecretSets {
		if s.App != "" || len(s.Values) > 0 {
			t.Errorf("set %s carries values or an owner", s.Name)
		}
		keys := slices.Clone(s.Generate)
		for _, d := range s.Derived {
			for _, ref := range controllers.TemplateKeys(d.Template) {
				if !slices.Contains(s.Generate, ref) && !slices.ContainsFunc(s.Derived, func(o kwerftv1.DerivedSecretKey) bool { return o.Key == ref }) {
					t.Errorf("set %s: %s refers to %s, which nothing sets", s.Name, d.Key, ref)
				}
			}
			keys = append(keys, d.Key)
		}
		sets[s.Name] = keys
	}
	volumes := map[string]bool{}
	for _, v := range p.Volumes {
		if !importplan.ValidAppName(v.Name) || v.Spec.Size.Sign() <= 0 {
			t.Errorf("volume %+v", v)
		}
		volumes[v.Name] = true
	}
	var images []string
	for _, a := range p.Apps {
		if !importplan.ValidAppName(a.Name) {
			t.Errorf("app name %q", a.Name)
		}
		s := a.Spec
		if s.Source.Image == nil {
			t.Errorf("%s: no image", a.Name)
			continue
		}
		ref := s.Source.Image.Ref
		images = append(images, ref)
		if tag := ref[strings.LastIndex(ref, ":")+1:]; strings.Contains(tag, "/") || tag == "latest" || !strings.ContainsAny(tag, "0123456789") {
			t.Errorf("%s: image %s is not pinned to a version", a.Name, ref)
		}
		// The same resources as an App made in the wizard: a size preset.
		if !slices.Contains([]string{"small", "medium", "large"}, s.Size) || s.Resources != nil {
			t.Errorf("%s: size %q resources %v, want a preset", a.Name, s.Size, s.Resources)
		}
		if s.Replicas == nil || *s.Replicas != 1 {
			t.Errorf("%s: replicas %v, want 1", a.Name, s.Replicas)
		}
		for _, e := range s.Env {
			if e.ValueFrom == nil {
				continue
			}
			ref := e.ValueFrom.SecretKeyRef
			if ref == nil || !slices.Contains(sets[ref.Name], ref.Key) {
				t.Errorf("%s: %s refers to %+v, which the template's sets do not have", a.Name, e.Name, ref)
			}
		}
		for _, v := range s.Volumes {
			if v.Volume != "" && !volumes[v.Volume] {
				t.Errorf("%s: mounts the Volume %s, which the template does not make", a.Name, v.Volume)
			}
		}
		for _, port := range s.Ports {
			if port.Public != "" && !strings.HasSuffix(port.Public, ".apps.example.com") {
				t.Errorf("%s: public %s, want under the apps domain", a.Name, port.Public)
			}
		}
		for _, from := range s.AllowFrom {
			if !slices.ContainsFunc(p.Apps, func(o importplan.App) bool { return o.Name == from }) {
				t.Errorf("%s: allows %s, which the template does not make", a.Name, from)
			}
		}
	}
	slices.Sort(images)
	listed := slices.Clone(tpl.Images)
	slices.Sort(listed)
	if !slices.Equal(slices.Compact(images), listed) {
		t.Errorf("images %v, the catalog lists %v", images, listed)
	}
}

func TestParameters(t *testing.T) {
	pg := Get("postgresql")
	p, err := pg.Render(map[string]string{"name": "main-db", "database": "shop", "user": "shop", "disk": "50Gi", "class": "hcloud-volume", "size": "medium", "allowFrom": "api, worker shop/web"}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := p.Apps[0]
	if a.Name != "main-db" || a.Spec.Size != "medium" || !slices.Equal(a.Spec.AllowFrom, []string{"api", "worker", "shop/web"}) {
		t.Errorf("app %+v", a)
	}
	if v := a.Spec.Volumes[0]; v.Size.String() != "50Gi" || v.Class != "hcloud-volume" {
		t.Errorf("disk %+v", v)
	}
	if d := p.SecretSets[0].Derived[0]; d.Key != "DATABASE_URL" || d.Template != "postgres://shop:${PASSWORD}@main-db:5432/shop" {
		t.Errorf("derived %+v", d)
	}
	if !strings.Contains(p.Notes, "main-db") {
		t.Errorf("notes %q", p.Notes)
	}

	for _, tc := range []struct {
		params map[string]string
		field  string
	}{
		{map[string]string{"name": "Main_DB"}, "parameters.name"},
		{map[string]string{"name": "x" + strings.Repeat("y", 60)}, "parameters.name"},
		{map[string]string{"database": "drop table"}, "parameters.database"},
		{map[string]string{"disk": "lots"}, "parameters.disk"},
		{map[string]string{"disk": "100Mi"}, "parameters.disk"},
		{map[string]string{"size": "huge"}, "parameters.size"},
		{map[string]string{"allowFrom": "a b/c/d"}, "parameters.allowFrom"},
		{map[string]string{"color": "blue"}, "parameters.color"},
		{map[string]string{"name": ""}, "parameters.name"},
	} {
		_, err := pg.Render(tc.params, "")
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != tc.field {
			t.Errorf("%v: error %v, want one for %s", tc.params, err, tc.field)
		}
	}
}

func TestHostnames(t *testing.T) {
	// Required hostnames: suggested under the apps domain, else asked for.
	for _, id := range []string{"n8n", "plausible"} {
		tpl := Get(id)
		var fe *FieldError
		if _, err := tpl.Render(nil, ""); !errors.As(err, &fe) || fe.Field != "parameters.hostname" {
			t.Errorf("%s without an apps domain: %v, want a hostname error", id, err)
		}
		p, err := tpl.Render(map[string]string{"hostname": "Stats.Example.org"}, "")
		if err != nil {
			t.Fatal(err)
		}
		main := p.Apps[len(p.Apps)-1]
		if main.Spec.Ports[0].Public != "stats.example.org" {
			t.Errorf("%s public %q", id, main.Spec.Ports[0].Public)
		}
		p, err = tpl.Render(map[string]string{"name": "team"}, "apps.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Apps[len(p.Apps)-1].Spec.Ports[0].Public; got != "team.apps.example.com" {
			t.Errorf("%s suggested %q", id, got)
		}
	}
	// Optional ones: suggested, or left out (inside the cluster) when empty.
	minio := Get("minio")
	p, err := minio.Render(nil, "apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ports := p.Apps[0].Spec.Ports; ports[0].Public != "minio.apps.example.com" || ports[1].Public != "minio-console.apps.example.com" {
		t.Errorf("minio ports %+v", ports)
	}
	p, err = minio.Render(map[string]string{"hostname": "", "consoleHostname": ""}, "apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, port := range p.Apps[0].Spec.Ports {
		if port.Public != "" {
			t.Errorf("minio port %d public %q, want none", port.Container, port.Public)
		}
	}
	if slices.ContainsFunc(p.Apps[0].Spec.Env, func(e corev1.EnvVar) bool { return e.Name == "MINIO_SERVER_URL" }) {
		t.Error("MINIO_SERVER_URL without a hostname")
	}
	if _, err := minio.Render(map[string]string{"hostname": "not a host"}, ""); err == nil {
		t.Error("an invalid hostname renders")
	}
}
