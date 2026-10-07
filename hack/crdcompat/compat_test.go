// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/utils/ptr"
)

type crds = map[string]*apiextv1.CustomResourceDefinition

func mustLoad(t *testing.T, dir string) crds {
	t.Helper()
	m, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// edit applies fn to the schema at path (property names; "[]" for array
// items, "[*]" for map values) of the first version of crd.
func edit(crd *apiextv1.CustomResourceDefinition, path string, fn func(*apiextv1.JSONSchemaProps)) {
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	var parts []string
	if path != "" {
		parts = strings.Split(path, ".")
	}
	editAt(root, parts, fn)
}

func editAt(s *apiextv1.JSONSchemaProps, parts []string, fn func(*apiextv1.JSONSchemaProps)) {
	if len(parts) == 0 {
		fn(s)
		return
	}
	switch parts[0] {
	case "[]":
		editAt(s.Items.Schema, parts[1:], fn)
	case "[*]":
		editAt(s.AdditionalProperties.Schema, parts[1:], fn)
	default:
		p := s.Properties[parts[0]]
		editAt(&p, parts[1:], fn)
		s.Properties[parts[0]] = p
	}
}

func str(desc string) apiextv1.JSONSchemaProps {
	return apiextv1.JSONSchemaProps{Type: "string", Description: desc}
}

func enum(vals ...string) []apiextv1.JSON {
	out := make([]apiextv1.JSON, len(vals))
	for i, v := range vals {
		out[i] = apiextv1.JSON{Raw: []byte(`"` + v + `"`)}
	}
	return out
}

func TestCompare(t *testing.T) {
	const w = "widgets.kwerft.dev v1alpha1 "
	cases := []struct {
		name   string
		change func(all crds, crd *apiextv1.CustomResourceDefinition)
		errs   []string // incompatible findings, exactly
		warns  []string // warnings, exactly
	}{
		// Compatible: the schema only gains.
		{name: "unchanged", change: func(crds, *apiextv1.CustomResourceDefinition) {}},
		{name: "new optional field", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) { s.Properties["shape"] = str("") })
		}},
		{name: "required fields inside a new optional field", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) {
				s.Properties["window"] = apiextv1.JSONSchemaProps{Type: "object", Required: []string{"start"},
					Properties: map[string]apiextv1.JSONSchemaProps{"start": str("")}}
			})
		}},
		{name: "enum widened", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.mode", func(s *apiextv1.JSONSchemaProps) { s.Enum = enum("Fast", "Medium", "Slow") })
		}},
		{name: "bounds widened or dropped", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.name", func(s *apiextv1.JSONSchemaProps) { s.MaxLength = ptr.To[int64](253) })
			edit(c, "spec.size", func(s *apiextv1.JSONSchemaProps) { s.Maximum = ptr.To(100.0); s.Minimum = nil })
			edit(c, "spec.tags", func(s *apiextv1.JSONSchemaProps) { s.MaxItems = nil })
		}},
		{name: "a required field becomes optional", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) { s.Required = nil })
		}},
		{name: "descriptions and defaults", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.color", func(s *apiextv1.JSONSchemaProps) {
				s.Description = "The colour."
				s.Default = &apiextv1.JSON{Raw: []byte(`"red"`)}
			})
		}},
		{name: "a new version and a new CRD", change: func(all crds, c *apiextv1.CustomResourceDefinition) {
			v := *c.Spec.Versions[0].DeepCopy()
			v.Name, v.Storage = "v1beta1", false
			c.Spec.Versions = append(c.Spec.Versions, v)
			n := c.DeepCopy()
			n.Name, n.Spec.Names.Kind, n.Spec.Names.ListKind = "sprockets.kwerft.dev", "Sprocket", "SprocketList"
			all[n.Name] = n
		}},

		// Incompatible.
		{name: "field removed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) { delete(s.Properties, "size") })
		}, errs: []string{w + ".spec.size: field removed"}},
		{name: "field renamed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) {
				s.Properties["colour"] = s.Properties["color"]
				delete(s.Properties, "color")
			})
		}, errs: []string{w + `.spec.color: field removed (renamed to "colour"?)`}},
		{name: "nested field removed in array items", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.rules.[]", func(s *apiextv1.JSONSchemaProps) { delete(s.Properties, "path") })
		}, errs: []string{w + ".spec.rules[].path: field removed"}},
		{name: "new required field", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) {
				s.Properties["owner"] = str("")
				s.Required = append(s.Required, "owner")
			})
		}, errs: []string{w + ".spec.owner: new required field"}},
		{name: "existing field becomes required", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) { s.Required = append(s.Required, "color") })
		}, errs: []string{w + ".spec.color: field became required"}},
		{name: "enum narrowed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.mode", func(s *apiextv1.JSONSchemaProps) { s.Enum = enum("Fast", "Turbo") })
		}, errs: []string{w + `.spec.mode: enum narrowed ("Slow" no longer allowed)`}},
		{name: "enum added", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "status.phase", func(s *apiextv1.JSONSchemaProps) { s.Enum = enum("Ready", "Failed") })
		}, errs: []string{w + `.status.phase: enum added (only "Ready", "Failed" allowed now)`}},
		{name: "type changed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.size", func(s *apiextv1.JSONSchemaProps) { s.Type = "string" })
		}, errs: []string{w + ".spec.size: type changed from integer to string"}},
		{name: "int-or-string dropped", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.port", func(s *apiextv1.JSONSchemaProps) { s.XIntOrString = false })
		}, errs: []string{w + ".spec.port: no longer accepts both integers and strings"}},
		{name: "map values narrowed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.labels.[*]", func(s *apiextv1.JSONSchemaProps) { s.Type = "integer" })
		}, errs: []string{w + ".spec.labels[*]: type changed from string to integer"}},
		{name: "map closed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.labels", func(s *apiextv1.JSONSchemaProps) { s.AdditionalProperties = nil })
		}, errs: []string{w + ".spec.labels: no longer accepts arbitrary keys"}},
		{name: "bounds narrowed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.name", func(s *apiextv1.JSONSchemaProps) {
				s.MaxLength = ptr.To[int64](32)
				s.MinLength = ptr.To[int64](3)
				s.Pattern = "^[a-z]+$"
			})
			edit(c, "spec.size", func(s *apiextv1.JSONSchemaProps) { s.Minimum = ptr.To(2.0) })
			edit(c, "spec.tags", func(s *apiextv1.JSONSchemaProps) { s.MaxItems = ptr.To[int64](4) })
		}, errs: []string{
			w + ".spec.name: maxLength lowered to 32",
			w + ".spec.name: minLength raised to 3",
			w + `.spec.name: pattern added: "^[a-z]+$"`,
			w + ".spec.size: minimum raised to 2",
			w + ".spec.tags: maxItems lowered to 4",
		}},
		{name: "version removed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			c.Spec.Versions[0].Name = "v1beta1"
		}, errs: []string{"widgets.kwerft.dev v1alpha1: version removed"}},
		{name: "version no longer served", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			c.Spec.Versions[0].Served = false
		}, errs: []string{"widgets.kwerft.dev v1alpha1: version no longer served"}},
		{name: "status subresource removed", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			c.Spec.Versions[0].Subresources = nil
		}, errs: []string{"widgets.kwerft.dev v1alpha1: status subresource removed"}},
		{name: "names and scope", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			c.Spec.Scope = apiextv1.ClusterScoped
			c.Spec.Names.Kind = "Gizmo"
			c.Spec.Names.ShortNames = nil
		}, errs: []string{
			"widgets.kwerft.dev: scope changed from Namespaced to Cluster",
			"widgets.kwerft.dev: kind renamed from Widget to Gizmo",
			"widgets.kwerft.dev: short name wg removed",
		}},
		{name: "CRD removed", change: func(all crds, c *apiextv1.CustomResourceDefinition) {
			delete(all, c.Name)
		}, errs: []string{"widgets.kwerft.dev: CRD removed"}},

		// Cannot tell: warn, do not fail.
		{name: "CEL rule added", change: func(_ crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec", func(s *apiextv1.JSONSchemaProps) {
				s.XValidations = apiextv1.ValidationRules{{Rule: "self.size <= 5"}}
			})
		}, warns: []string{w + `.spec: new validation rule "self.size <= 5": check that objects of the old release pass it`}},
		{name: "pattern changed", change: func(all crds, c *apiextv1.CustomResourceDefinition) {
			edit(c, "spec.color", func(s *apiextv1.JSONSchemaProps) { s.Pattern = "^[a-z]+$" })
			o := all["old"]
			edit(o, "spec.color", func(s *apiextv1.JSONSchemaProps) { s.Pattern = "^[a-z0-9]+$" })
		}, warns: []string{w + `.spec.color: pattern changed from "^[a-z0-9]+$" to "^[a-z]+$": check that it accepts every value the old one did`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldCRDs := mustLoad(t, "testdata/old")
			newCRDs := crds{}
			for k, v := range oldCRDs {
				newCRDs[k] = v.DeepCopy()
			}
			// "old" lets a case patch the old side too; removed before comparing.
			newCRDs["old"] = oldCRDs["widgets.kwerft.dev"]
			tc.change(newCRDs, newCRDs["widgets.kwerft.dev"])
			delete(newCRDs, "old")

			var errs, warns []string
			for _, f := range Compare(oldCRDs, newCRDs) {
				if f.Warning {
					warns = append(warns, strings.TrimPrefix(f.String(), "warning: "))
				} else {
					errs = append(errs, f.String())
				}
			}
			slices.Sort(errs)
			slices.Sort(warns)
			want, wantWarns := slices.Sorted(slices.Values(tc.errs)), slices.Sorted(slices.Values(tc.warns))
			if !slices.Equal(errs, want) {
				t.Errorf("findings:\n  %s\nwant:\n  %s", strings.Join(errs, "\n  "), strings.Join(want, "\n  "))
			}
			if !slices.Equal(warns, wantWarns) {
				t.Errorf("warnings:\n  %s\nwant:\n  %s", strings.Join(warns, "\n  "), strings.Join(wantWarns, "\n  "))
			}
		})
	}
}

// The chart's own CRDs load and are compatible with themselves.
func TestChartCRDs(t *testing.T) {
	chart := mustLoad(t, "../../charts/kwerft/crds")
	if len(chart) < 10 {
		t.Fatalf("only %d CRDs in charts/kwerft/crds", len(chart))
	}
	if _, ok := chart["upgrades.kwerft.dev"]; !ok {
		t.Error("upgrades.kwerft.dev not loaded")
	}
	if f := Compare(chart, chart); len(f) > 0 {
		t.Errorf("chart CRDs differ from themselves: %v", f)
	}
}

func TestRun(t *testing.T) {
	cases := []struct {
		args   []string
		code   int
		stdout []string
		stderr string
	}{
		{args: []string{"testdata/old", "testdata/old"}, code: 0, stderr: "2 CRDs compatible with the 2 in testdata/old"},
		{args: []string{"-name", "v0.5.0", "testdata/old", "testdata/old"}, code: 0, stderr: "2 CRDs compatible with the 2 in v0.5.0 (0 warning(s))"},
		{args: []string{"testdata/old", "testdata/new"}, code: 1, stdout: []string{
			`widgets.kwerft.dev v1alpha1 .spec.color: field removed (renamed to "colour"?)`,
			`widgets.kwerft.dev v1alpha1 .spec.mode: enum narrowed ("Slow" no longer allowed)`,
		}, stderr: "2 incompatible change(s)"},
		// The other way round: Sprockets disappear, colour is removed, and
		// the enum of spec.mode widens, which is fine.
		{args: []string{"testdata/new", "testdata/old"}, code: 1, stdout: []string{
			"sprockets.kwerft.dev: CRD removed",
			`widgets.kwerft.dev v1alpha1 .spec.colour: field removed (renamed to "color"?)`,
		}},
		{args: []string{"testdata/old", "testdata/missing"}, code: 2, stderr: "testdata/missing"},
		{args: []string{"testdata/old", "testdata"}, code: 2, stderr: "no CRDs"},
		{args: []string{"testdata/old"}, code: 2, stderr: "usage"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, &stdout, &stderr)
			}
			got := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(tc.stdout) > 0 && !slices.Equal(got, tc.stdout) {
				t.Errorf("stdout:\n%s\nwant:\n%s", &stdout, strings.Join(tc.stdout, "\n"))
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("stderr %q does not contain %q", &stderr, tc.stderr)
			}
		})
	}
}
