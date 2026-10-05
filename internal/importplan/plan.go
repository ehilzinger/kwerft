// Package importplan is what a Compose import (internal/compose) and a
// template (internal/templates) turn into: the Kwerft objects to create in
// one project, with the warnings and renames to show before anything is
// created. The API (internal/server/api_import.go) checks a plan with a
// server-side dry run and applies it as the user.
package importplan

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Plan is everything an import creates, in the order it is created:
// Volumes, shared SecretSets, Apps, then each App's own SecretSet with its
// values.
type Plan struct {
	Apps       []App       `json:"apps"`
	Volumes    []Volume    `json:"volumes"`
	SecretSets []SecretSet `json:"secretSets"`
	Warnings   []Warning   `json:"warnings"`
	Renames    []Rename    `json:"renames"`
	// Notes say what to do next (a template's: where to sign in, which key
	// other apps connect with).
	Notes string `json:"notes,omitempty"`
}

// App is one App to create.
type App struct {
	Name string `json:"name"`
	// Service is the Compose service it comes from.
	Service string           `json:"service,omitempty"`
	Spec    kwerftv1.AppSpec `json:"spec"`
}

// Volume is one shared Volume to create.
type Volume struct {
	Name string              `json:"name"`
	Spec kwerftv1.VolumeSpec `json:"spec"`
}

// SecretSet is one set to create. With App, it is that App's own set
// (<app>-env, owned by the App, created right after it); otherwise a shared
// set of the project, created before the Apps that reference it.
type SecretSet struct {
	Name        string                      `json:"name"`
	App         string                      `json:"app,omitempty"`
	Description string                      `json:"description,omitempty"`
	Generate    []string                    `json:"generate,omitempty"`
	Derived     []kwerftv1.DerivedSecretKey `json:"derived,omitempty"`
	// Keys are the keys written from the import (Values), by name. The
	// values themselves never leave the console in an answer.
	Keys   []string          `json:"keys,omitempty"`
	Values map[string]string `json:"-"`
	// Missing are keys Apps reference that nobody gave a value: the Apps
	// wait (SecretMissing) until someone sets them on the Secrets page.
	Missing []string `json:"missing,omitempty"`
}

// Level of a warning: Warn for something Kwerft does differently or not at
// all, Info for a choice the import made that the user may want to change.
const (
	Warn = "warning"
	Info = "info"
)

// Warning is shown with the plan. Service (or Object) says what it is about,
// Key the Compose key ("cap_add", "volumes[1]").
type Warning struct {
	Level   string `json:"level"`
	Service string `json:"service,omitempty"`
	Key     string `json:"key,omitempty"`
	Message string `json:"message"`
}

// Rename is a name that had to change to be a valid Kubernetes name.
type Rename struct {
	Kind string `json:"kind"` // App | Volume
	From string `json:"from"`
	To   string `json:"to"`
}

// Add appends a warning.
func (p *Plan) Add(level, service, key, format string, args ...any) {
	p.Warnings = append(p.Warnings, Warning{Level: level, Service: service, Key: key, Message: fmt.Sprintf(format, args...)})
}

// Empty slices instead of nil, for the JSON answer.
func (p *Plan) Normalize() {
	if p.Apps == nil {
		p.Apps = []App{}
	}
	if p.Volumes == nil {
		p.Volumes = []Volume{}
	}
	if p.SecretSets == nil {
		p.SecretSets = []SecretSet{}
	}
	if p.Warnings == nil {
		p.Warnings = []Warning{}
	}
	if p.Renames == nil {
		p.Renames = []Rename{}
	}
}

// Objects lists the plan's objects as Kind/name, in creation order.
func (p *Plan) Objects() []string {
	var out []string
	for _, v := range p.Volumes {
		out = append(out, "Volume/"+v.Name)
	}
	for _, s := range p.SecretSets {
		if s.App == "" {
			out = append(out, "SecretSet/"+s.Name)
		}
	}
	for _, a := range p.Apps {
		out = append(out, "App/"+a.Name)
	}
	for _, s := range p.SecretSets {
		if s.App != "" {
			out = append(out, "SecretSet/"+s.Name)
		}
	}
	return out
}

// MaxAppName leaves room for the App's own set, <app>-env, within 63.
const MaxAppName = 63 - len(controllers.AppEnvSetSuffix)

// AppEnvSet is the name of an App's own set.
func AppEnvSet(app string) string { return controllers.AppEnvSet(app) }

var (
	invalidRun = regexp.MustCompile(`[^a-z0-9-]+`)
	dashes     = regexp.MustCompile(`-{2,}`)
)

// SafeName turns s into a DNS-1035 label (lowercase letters, digits and
// dashes, starting with a letter) of at most max characters. An App's name
// is also its hostname in the project, and Volume names follow the same
// rule. fallback is used when nothing of s is left.
func SafeName(s string, max int, fallback string) string {
	n := strings.ToLower(strings.TrimSpace(s))
	n = invalidRun.ReplaceAllString(n, "-")
	n = dashes.ReplaceAllString(n, "-")
	n = strings.Trim(n, "-")
	if n != "" && (n[0] < 'a' || n[0] > 'z') {
		n = fallback + "-" + n
	}
	if n == "" {
		n = fallback
	}
	if len(n) > max {
		n = strings.TrimRight(n[:max], "-")
	}
	return n
}

// ValidAppName reports whether name can be an App's name (and its own set's).
func ValidAppName(name string) bool {
	return len(name) <= MaxAppName && len(validation.IsDNS1035Label(name)) == 0
}

// Unique returns name, or name-2, name-3, … (within max characters) when
// taken says it is used.
func Unique(name string, max int, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("-%d", i)
		base := name
		if len(base)+len(suffix) > max {
			base = strings.TrimRight(base[:max-len(suffix)], "-")
		}
		if c := base + suffix; !taken(c) {
			return c
		}
	}
}
