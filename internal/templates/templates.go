// Package templates is the catalog of ready-made Apps the New App wizard
// offers (PostgreSQL, Redis, MinIO, n8n, Plausible, …). Each template is one
// YAML file compiled into the binary: its parameters (with defaults and
// validation) and a Go text/template that renders the Apps, Volumes and
// SecretSets to create. Passwords are never in a template: SecretSets
// generate them, and derive connection URLs from them.
//
// A rendered template is an importplan.Plan, which the API checks and
// applies like a Compose import, as the user, with the same validation as
// an App made by hand.
package templates

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/ehilzinger/kwerft/internal/importplan"
)

//go:embed catalog/*.yaml
var files embed.FS

// Parameter types.
const (
	TypeName       = "name"       // an App name (DNS-1035 label)
	TypeIdentifier = "identifier" // a database or user name: [a-z_][a-z0-9_]*
	TypeText       = "text"       // free text matching Pattern
	TypeEnum       = "enum"       // one of Options
	TypeSize       = "size"       // a disk size, between Min and Max
	TypeHostname   = "hostname"   // a public hostname; "" keeps the app inside the cluster unless Required
	TypeApps       = "apps"       // apps that may connect: "api, shop/web"
)

// Parameter is one field of a template's form.
type Parameter struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
	Hint  string `json:"hint,omitempty"`
	// Default is the value when the request leaves the parameter out. For a
	// hostname without one, the default is <name><Suffix>.<apps domain>.
	Default  string   `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
	// Pattern (text), MaxLength (name, text), Min and Max (size).
	Pattern   string `json:"pattern,omitempty"`
	MaxLength int    `json:"maxLength,omitempty"`
	Min       string `json:"min,omitempty"`
	Max       string `json:"max,omitempty"`
	// Suffix of a hostname's suggestion: "-console" for console.<…>.
	Suffix string `json:"suffix,omitempty"`
}

// Template is one catalog entry.
type Template struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Description string `json:"description"`
	// Images the template runs, pinned to a version on Pinned (a date).
	Images     []string    `json:"images"`
	Pinned     string      `json:"pinned"`
	Parameters []Parameter `json:"parameters"`
	// Objects is the text/template of the objects (YAML: secretSets,
	// volumes, apps); Notes is shown once they are created. Both see the
	// parameters by key, and .appsDomain.
	Objects string `json:"-"`
	Notes   string `json:"-"`

	objects *template.Template
	notes   *template.Template
}

var (
	catalog []*Template
	byID    = map[string]*Template{}
)

func init() {
	entries, err := fs.Glob(files, "catalog/*.yaml")
	if err != nil {
		panic(err)
	}
	for _, name := range entries {
		raw, err := files.ReadFile(name)
		if err != nil {
			panic(err)
		}
		t, err := parse(raw)
		if err != nil {
			panic(fmt.Sprintf("template %s: %v", name, err))
		}
		catalog = append(catalog, t)
		byID[t.ID] = t
	}
	sort.SliceStable(catalog, func(i, j int) bool { return strings.ToLower(catalog[i].Title) < strings.ToLower(catalog[j].Title) })
}

var funcs = template.FuncMap{
	// quote makes a value a YAML (JSON) string.
	"quote": func(s string) string { b, _ := json.Marshal(s); return string(b) },
	// apps renders apps parameters (and names) as a YAML list.
	"apps": func(lists ...string) string {
		out := []string{}
		for _, l := range lists {
			for _, a := range splitApps(l) {
				if !slices.Contains(out, a) {
					out = append(out, a)
				}
			}
		}
		b, _ := json.Marshal(out)
		return string(b)
	},
}

func parse(raw []byte) (*Template, error) {
	var doc struct {
		Template
		Objects string `json:"objects"`
		Notes   string `json:"notes"`
	}
	if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
		return nil, err
	}
	t := doc.Template
	t.Objects, t.Notes = doc.Objects, doc.Notes
	if t.ID == "" || t.Title == "" || t.Objects == "" || len(t.Images) == 0 || t.Pinned == "" {
		return nil, fmt.Errorf("id, title, images, pinned and objects are required")
	}
	if len(t.Parameters) == 0 || t.Parameters[0].Key != "name" || t.Parameters[0].Type != TypeName {
		return nil, fmt.Errorf("the first parameter is name, of type name")
	}
	for _, p := range t.Parameters {
		switch p.Type {
		case TypeName, TypeIdentifier, TypeEnum, TypeSize, TypeHostname, TypeApps:
		case TypeText:
			if p.Pattern == "" {
				return nil, fmt.Errorf("text parameter %s needs a pattern", p.Key)
			}
		default:
			return nil, fmt.Errorf("parameter %s has the unknown type %q", p.Key, p.Type)
		}
	}
	var err error
	if t.objects, err = template.New(t.ID).Funcs(funcs).Option("missingkey=error").Parse(t.Objects); err != nil {
		return nil, err
	}
	if t.notes, err = template.New(t.ID + "-notes").Funcs(funcs).Option("missingkey=error").Parse(t.Notes); err != nil {
		return nil, err
	}
	return &t, nil
}

// List is the catalog, by title.
func List() []*Template { return catalog }

// Get is the template with the id, nil if there is none.
func Get(id string) *Template { return byID[id] }

// FieldError is a parameter value that is not valid, for the form.
type FieldError struct {
	Field   string // "parameters.<key>"
	Message string
}

func (e *FieldError) Error() string { return e.Message }

var (
	identifierRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	hostnameRE   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`)
	appRefRE     = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?/)?[a-z]([-a-z0-9]*[a-z0-9])?$`)
)

func splitApps(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// Values resolves the request's parameters: defaults for those left out
// (hostnames under appsDomain), each checked by its type. Unknown keys are
// an error.
func (t *Template) Values(given map[string]string, appsDomain string) (map[string]string, error) {
	for k := range given {
		if !slices.ContainsFunc(t.Parameters, func(p Parameter) bool { return p.Key == k }) {
			return nil, &FieldError{Field: "parameters." + k, Message: fmt.Sprintf("%s has no parameter %q.", t.Title, k)}
		}
	}
	out := map[string]string{}
	for _, p := range t.Parameters {
		v, ok := given[p.Key]
		v = strings.TrimSpace(v)
		if !ok {
			v = p.Default
			if p.Type == TypeHostname && v == "" && appsDomain != "" {
				v = out["name"] + p.Suffix + "." + appsDomain
			}
		}
		if err := p.check(v, appsDomain); err != nil {
			return nil, err
		}
		if p.Type == TypeHostname {
			v = strings.ToLower(v)
		}
		out[p.Key] = v
	}
	return out, nil
}

func (p *Parameter) check(v, appsDomain string) error {
	fail := func(format string, args ...any) error {
		return &FieldError{Field: "parameters." + p.Key, Message: fmt.Sprintf(format, args...)}
	}
	if v == "" {
		if p.Required {
			if p.Type == TypeHostname && appsDomain == "" {
				return fail("Enter the public hostname, like app.example.com: no apps domain is set in Settings to suggest one.")
			}
			return fail("%s is required.", p.Label)
		}
		if p.Type == TypeHostname || p.Type == TypeApps || p.Type == TypeText {
			return nil
		}
		return fail("%s is required.", p.Label)
	}
	max := p.MaxLength
	switch p.Type {
	case TypeName:
		if max == 0 {
			max = importplan.MaxAppName
		}
		if len(v) > max || len(validation.IsDNS1035Label(v)) > 0 {
			return fail("Use lowercase letters, digits and dashes, starting with a letter (at most %d).", max)
		}
	case TypeIdentifier:
		if !identifierRE.MatchString(v) {
			return fail("Use lowercase letters, digits and _, starting with a letter (at most 63).")
		}
	case TypeText:
		if max == 0 {
			max = 200
		}
		if len(v) > max || !regexp.MustCompile("^(?:"+p.Pattern+")$").MatchString(v) {
			return fail("%q is not valid here.", v)
		}
	case TypeEnum:
		if !slices.Contains(p.Options, v) {
			return fail("Choose one of %s.", strings.Join(p.Options, ", "))
		}
	case TypeSize:
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			return fail("Enter a size like 10Gi.")
		}
		if p.Min != "" && q.Cmp(resource.MustParse(p.Min)) < 0 {
			return fail("At least %s.", p.Min)
		}
		if p.Max != "" && q.Cmp(resource.MustParse(p.Max)) > 0 {
			return fail("At most %s.", p.Max)
		}
	case TypeHostname:
		v = strings.ToLower(v)
		if len(v) > 253 || !hostnameRE.MatchString(v) {
			return fail("%q is not a hostname. Use lowercase, like app.example.com.", v)
		}
	case TypeApps:
		for _, a := range splitApps(v) {
			if !appRefRE.MatchString(a) {
				return fail("%q is not an app (api, or project/api).", a)
			}
		}
	}
	return nil
}

// Render resolves the parameters and renders the template's objects.
func (t *Template) Render(given map[string]string, appsDomain string) (*importplan.Plan, error) {
	values, err := t.Values(given, appsDomain)
	if err != nil {
		return nil, err
	}
	data := map[string]any{"appsDomain": appsDomain}
	for k, v := range values {
		data[k] = v
	}
	var buf bytes.Buffer
	if err := t.objects.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("template %s: %w", t.ID, err)
	}
	var objs struct {
		SecretSets []importplan.SecretSet `json:"secretSets"`
		Volumes    []importplan.Volume    `json:"volumes"`
		Apps       []importplan.App       `json:"apps"`
	}
	if err := yaml.UnmarshalStrict(buf.Bytes(), &objs); err != nil {
		return nil, fmt.Errorf("template %s renders invalid objects: %w", t.ID, err)
	}
	p := &importplan.Plan{Apps: objs.Apps, Volumes: objs.Volumes, SecretSets: objs.SecretSets}
	for _, s := range p.SecretSets {
		if s.App != "" || len(s.Keys) > 0 || len(s.Values) > 0 {
			return nil, fmt.Errorf("template %s: set %s: templates make shared sets with generate and derived keys only", t.ID, s.Name)
		}
	}
	buf.Reset()
	if err := t.notes.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("template %s notes: %w", t.ID, err)
	}
	p.Notes = strings.TrimSpace(buf.String())
	p.Normalize()
	return p, nil
}
