// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Finding is one change that a console of the previous release could not
// live with after a rollback: CRDs are not rolled back, so the old controller
// and API meet objects written against the new schema, and the new schema
// must still accept everything the old release wrote.
type Finding struct {
	CRD     string // upgrades.kwerft.dev
	Version string // v1alpha1; empty for the CRD as a whole
	Path    string // .spec.component; empty for the version as a whole
	Message string
	// Warning: a change the tool cannot judge (a CEL rule, a changed
	// pattern). Printed, but does not fail the check.
	Warning bool
}

func (f Finding) String() string {
	var b strings.Builder
	if f.Warning {
		b.WriteString("warning: ")
	}
	b.WriteString(f.CRD)
	if f.Version != "" {
		b.WriteString(" " + f.Version)
	}
	if f.Path != "" {
		b.WriteString(" " + f.Path)
	}
	b.WriteString(": " + f.Message)
	return b.String()
}

// LoadDir reads every CustomResourceDefinition in the YAML files of dir,
// keyed by name. Other documents and files (README.md) are ignored.
func LoadDir(dir string) (map[string]*apiextv1.CustomResourceDefinition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	crds := map[string]*apiextv1.CustomResourceDefinition{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if err := decodeCRDs(data, crds); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return crds, nil
}

func decodeCRDs(data []byte, into map[string]*apiextv1.CustomResourceDefinition) error {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var crd apiextv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(doc, &crd); err != nil {
			return err
		}
		if crd.Kind != "CustomResourceDefinition" {
			continue
		}
		if crd.Name == "" {
			return errors.New("a CustomResourceDefinition without a name")
		}
		if _, dup := into[crd.Name]; dup {
			return fmt.Errorf("%s is defined twice", crd.Name)
		}
		into[crd.Name] = &crd
	}
}

// Compare lists what makes newCRDs incompatible with oldCRDs. Compatible
// means: every CRD and served version is still there with the same names and
// scope, and within each version the schema only gained optional fields —
// nothing removed or renamed, no field newly required, no enum, type or
// bound that rejects a value the old schema accepted.
func Compare(oldCRDs, newCRDs map[string]*apiextv1.CustomResourceDefinition) []Finding {
	var out []Finding
	for _, name := range sortedKeys(oldCRDs) {
		o, n := oldCRDs[name], newCRDs[name]
		if n == nil {
			out = append(out, Finding{CRD: name, Message: "CRD removed"})
			continue
		}
		out = append(out, compareCRD(name, o, n)...)
	}
	return out
}

func compareCRD(name string, o, n *apiextv1.CustomResourceDefinition) []Finding {
	var out []Finding
	add := func(msg string, args ...any) {
		out = append(out, Finding{CRD: name, Message: fmt.Sprintf(msg, args...)})
	}
	if o.Spec.Scope != n.Spec.Scope {
		add("scope changed from %s to %s", o.Spec.Scope, n.Spec.Scope)
	}
	if o.Spec.Names.Kind != n.Spec.Names.Kind {
		add("kind renamed from %s to %s", o.Spec.Names.Kind, n.Spec.Names.Kind)
	}
	if o.Spec.Names.ListKind != n.Spec.Names.ListKind {
		add("list kind renamed from %s to %s", o.Spec.Names.ListKind, n.Spec.Names.ListKind)
	}
	for _, sn := range o.Spec.Names.ShortNames {
		if !slices.Contains(n.Spec.Names.ShortNames, sn) {
			add("short name %s removed", sn)
		}
	}
	for _, ov := range o.Spec.Versions {
		if !ov.Served {
			continue
		}
		var nv *apiextv1.CustomResourceDefinitionVersion
		for i := range n.Spec.Versions {
			if n.Spec.Versions[i].Name == ov.Name {
				nv = &n.Spec.Versions[i]
			}
		}
		vf := func(path, msg string, args ...any) {
			out = append(out, Finding{CRD: name, Version: ov.Name, Path: path, Message: fmt.Sprintf(msg, args...)})
		}
		switch {
		case nv == nil:
			vf("", "version removed")
			continue
		case !nv.Served:
			vf("", "version no longer served")
			continue
		}
		if ov.Subresources != nil && ov.Subresources.Status != nil &&
			(nv.Subresources == nil || nv.Subresources.Status == nil) {
			vf("", "status subresource removed")
		}
		if ov.Subresources != nil && ov.Subresources.Scale != nil &&
			(nv.Subresources == nil || nv.Subresources.Scale == nil) {
			vf("", "scale subresource removed")
		}
		var oldSchema, newSchema *apiextv1.JSONSchemaProps
		if ov.Schema != nil {
			oldSchema = ov.Schema.OpenAPIV3Schema
		}
		if nv.Schema != nil {
			newSchema = nv.Schema.OpenAPIV3Schema
		}
		if oldSchema == nil {
			continue
		}
		if newSchema == nil {
			vf("", "schema removed")
			continue
		}
		w := walker{
			report: func(path, msg string) { vf(path, "%s", msg) },
			warn: func(path, msg string) {
				out = append(out, Finding{CRD: name, Version: ov.Name, Path: path, Message: msg, Warning: true})
			},
		}
		w.walk("", oldSchema, newSchema)
	}
	return out
}

type walker struct {
	report func(path, msg string) // incompatible
	warn   func(path, msg string) // cannot tell; a human checks
}

func (w walker) walk(path string, o, n *apiextv1.JSONSchemaProps) {
	at := path
	if at == "" {
		at = "."
	}
	if o.Type != n.Type {
		w.report(at, fmt.Sprintf("type changed from %s to %s", typeName(o.Type), typeName(n.Type)))
		return
	}
	if o.XIntOrString && !n.XIntOrString {
		w.report(at, "no longer accepts both integers and strings")
	}
	if o.XPreserveUnknownFields != nil && *o.XPreserveUnknownFields &&
		(n.XPreserveUnknownFields == nil || !*n.XPreserveUnknownFields) {
		w.report(at, "no longer preserves unknown fields")
	}
	if o.Nullable && !n.Nullable {
		w.report(at, "no longer nullable")
	}
	w.enum(at, o, n)
	w.bounds(at, o, n)
	// CEL rules cannot be compared, so a new or rewritten rule is a warning:
	// the author checks that objects of the old release still pass it.
	for _, r := range n.XValidations {
		if !slices.ContainsFunc(o.XValidations, func(x apiextv1.ValidationRule) bool { return x.Rule == r.Rule }) {
			w.warn(at, fmt.Sprintf("new validation rule %q: check that objects of the old release pass it", r.Rule))
		}
	}

	// Fields: none removed (a rename is a removal plus an addition), none
	// newly required. Required fields inside a new optional field are fine:
	// objects of the old release do not have that field at all.
	for _, req := range n.Required {
		if !slices.Contains(o.Required, req) {
			if _, existed := o.Properties[req]; existed {
				w.report(path+"."+req, "field became required")
			} else {
				w.report(path+"."+req, "new required field")
			}
		}
	}
	for _, name := range sortedKeys(o.Properties) {
		op := o.Properties[name]
		np, ok := n.Properties[name]
		if !ok {
			if n.XPreserveUnknownFields != nil && *n.XPreserveUnknownFields && len(n.Properties) == 0 {
				continue // the new schema accepts anything here
			}
			msg := "field removed"
			if to := renamedTo(op, o, n); to != "" {
				msg = fmt.Sprintf("field removed (renamed to %q?)", to)
			}
			w.report(path+"."+name, msg)
			continue
		}
		w.walk(path+"."+name, &op, &np)
	}
	if o.Items != nil && o.Items.Schema != nil {
		if n.Items == nil || n.Items.Schema == nil {
			w.report(at, "item schema removed")
		} else {
			w.walk(path+"[]", o.Items.Schema, n.Items.Schema)
		}
	}
	if o.AdditionalProperties != nil && (o.AdditionalProperties.Allows || o.AdditionalProperties.Schema != nil) {
		switch {
		case n.AdditionalProperties == nil || (!n.AdditionalProperties.Allows && n.AdditionalProperties.Schema == nil):
			w.report(at, "no longer accepts arbitrary keys")
		case o.AdditionalProperties.Schema != nil && n.AdditionalProperties.Schema != nil:
			w.walk(path+"[*]", o.AdditionalProperties.Schema, n.AdditionalProperties.Schema)
		}
	}
}

// enum: every value the old schema allowed must still be allowed.
func (w walker) enum(at string, o, n *apiextv1.JSONSchemaProps) {
	if len(n.Enum) == 0 {
		return
	}
	newVals := map[string]bool{}
	for _, v := range n.Enum {
		newVals[string(v.Raw)] = true
	}
	if len(o.Enum) == 0 {
		w.report(at, "enum added (only "+joinEnum(n.Enum)+" allowed now)")
		return
	}
	var gone []apiextv1.JSON
	for _, v := range o.Enum {
		if !newVals[string(v.Raw)] {
			gone = append(gone, v)
		}
	}
	if len(gone) > 0 {
		w.report(at, "enum narrowed ("+joinEnum(gone)+" no longer allowed)")
	}
}

// bounds: limits may only widen.
func (w walker) bounds(at string, o, n *apiextv1.JSONSchemaProps) {
	maxi := func(what string, ov, nv *int64) {
		if nv != nil && (ov == nil || *nv < *ov) {
			w.report(at, fmt.Sprintf("%s lowered to %d", what, *nv))
		}
	}
	mini := func(what string, ov, nv *int64) {
		if nv != nil && *nv > 0 && (ov == nil || *nv > *ov) {
			w.report(at, fmt.Sprintf("%s raised to %d", what, *nv))
		}
	}
	maxi("maxLength", o.MaxLength, n.MaxLength)
	maxi("maxItems", o.MaxItems, n.MaxItems)
	maxi("maxProperties", o.MaxProperties, n.MaxProperties)
	mini("minLength", o.MinLength, n.MinLength)
	mini("minItems", o.MinItems, n.MinItems)
	mini("minProperties", o.MinProperties, n.MinProperties)
	if n.Maximum != nil && (o.Maximum == nil || *n.Maximum < *o.Maximum ||
		(n.ExclusiveMaximum && !o.ExclusiveMaximum && *n.Maximum == *o.Maximum)) {
		w.report(at, fmt.Sprintf("maximum lowered to %v", *n.Maximum))
	}
	if n.Minimum != nil && (o.Minimum == nil || *n.Minimum > *o.Minimum ||
		(n.ExclusiveMinimum && !o.ExclusiveMinimum && *n.Minimum == *o.Minimum)) {
		w.report(at, fmt.Sprintf("minimum raised to %v", *n.Minimum))
	}
	// A new pattern or format narrows; a changed one may or may not.
	switch {
	case n.Pattern == "" || n.Pattern == o.Pattern:
	case o.Pattern == "":
		w.report(at, fmt.Sprintf("pattern added: %q", n.Pattern))
	default:
		w.warn(at, fmt.Sprintf("pattern changed from %q to %q: check that it accepts every value the old one did", o.Pattern, n.Pattern))
	}
	switch {
	case n.Format == "" || n.Format == o.Format:
	case o.Format == "":
		w.report(at, fmt.Sprintf("format added: %q", n.Format))
	default:
		w.warn(at, fmt.Sprintf("format changed from %q to %q", o.Format, n.Format))
	}
	if o.UniqueItems != n.UniqueItems && n.UniqueItems {
		w.report(at, "items must be unique now")
	}
}

// renamedTo guesses the new name of a removed field: the only field the new
// schema added next to it with the same type.
func renamedTo(removed apiextv1.JSONSchemaProps, o, n *apiextv1.JSONSchemaProps) string {
	var candidates []string
	for name, p := range n.Properties {
		if _, old := o.Properties[name]; !old && p.Type == removed.Type {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func joinEnum(vals []apiextv1.JSON) string {
	s := make([]string, len(vals))
	for i, v := range vals {
		s[i] = string(v.Raw)
	}
	return strings.Join(s, ", ")
}

func typeName(t string) string {
	if t == "" {
		return "any"
	}
	return t
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
