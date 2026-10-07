// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package compose turns a Docker Compose file into Kwerft objects
// (importplan.Plan): every service becomes an App, named volumes become
// Volumes, environment values whose names look secret go into the App's own
// SecretSet, published HTTP ports get a public hostname under the apps
// domain. What Kwerft does not do (bind mounts, privileged containers,
// startup ordering, …) becomes a warning instead of being dropped silently.
//
// It reads the part of the Compose specification
// (https://compose-spec.io) that maps onto an App, with a YAML parser and
// Compose's variable interpolation; it does not pull in a full Compose
// implementation, which would bring Docker's dependency tree with it.
package compose

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/importplan"
)

// MaxSize is the largest Compose file Convert reads.
const MaxSize = 256 << 10

// Options are what the file does not say.
type Options struct {
	// AppsDomain gives published HTTP ports a public hostname
	// <app>.<AppsDomain>; empty keeps every port inside the cluster.
	AppsDomain string
	// Env holds the values of ${VAR} references, like a .env file next to
	// the Compose file.
	Env map[string]string
}

// Convert reads a Compose file. Errors are for the user: the file cannot be
// read at all, or no service can become an App.
func Convert(src []byte, opt Options) (*importplan.Plan, error) {
	if len(strings.TrimSpace(string(src))) == 0 {
		return nil, errors.New("The Compose file is empty.")
	}
	if len(src) > MaxSize {
		return nil, fmt.Errorf("The Compose file is larger than %d KiB.", MaxSize>>10)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, fmt.Errorf("This is not valid YAML: %s.", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("A Compose file is a YAML mapping with services: at the top.")
	}
	in := &interpolator{env: opt.Env, missing: map[string]bool{}}
	if err := in.interpolate(&root); err != nil {
		return nil, fmt.Errorf("Variable interpolation failed: %v.", err)
	}
	var doc map[string]any
	if err := root.Decode(&doc); err != nil {
		return nil, fmt.Errorf("This is not a Compose file: %s.", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	order := serviceOrder(root.Content[0])
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return nil, errors.New("The file has no services.")
	}
	c := &converter{opt: opt, plan: &importplan.Plan{}, doc: doc, apps: map[string]bool{}, volumes: map[string]string{}}
	if names := in.missingNames(); len(names) > 0 {
		c.plan.Add(importplan.Warn, "", "", "Not set, so empty: %s. Give their values with the file (like a .env file) to use them.", strings.Join(names, ", "))
	}
	c.topLevel()
	for _, key := range order {
		raw, ok := services[key].(map[string]any)
		if !ok {
			c.plan.Add(importplan.Warn, key, "", "The service is not a mapping; skipped.")
			continue
		}
		c.service(key, raw)
	}
	if len(c.svcs) == 0 {
		return nil, errors.New("No service of the file can become an app; see the warnings: " + c.warningText())
	}
	c.crossReferences()
	for _, s := range c.svcs {
		c.plan.Apps = append(c.plan.Apps, s.app)
		if s.set != nil {
			c.plan.SecretSets = append(c.plan.SecretSets, *s.set)
		}
	}
	c.plan.Add(importplan.Info, "", "", "Apps reach the internet over HTTPS only (Kwerft's default); set x-kwerft: {egress: all} on a service, or change it in the app's settings.")
	c.plan.Normalize()
	return c.plan, nil
}

func (c *converter) warningText() string {
	var parts []string
	for _, w := range c.plan.Warnings {
		if w.Service != "" {
			parts = append(parts, w.Service+": "+w.Message)
		}
	}
	return strings.Join(parts, " ")
}

// serviceOrder is the order the services are written in.
func serviceOrder(top *yaml.Node) []string {
	var out []string
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "services" {
			continue
		}
		m := top.Content[i+1]
		if m.Kind == yaml.AliasNode {
			m = m.Alias
		}
		for j := 0; j+1 < len(m.Content); j += 2 {
			if k := m.Content[j].Value; k != "<<" {
				out = append(out, k)
			}
		}
	}
	return out
}

type converter struct {
	opt  Options
	plan *importplan.Plan
	doc  map[string]any
	svcs []*service
	// apps: App names taken; volumes: Compose volume name → Volume name.
	apps    map[string]bool
	volumes map[string]string
	// Top-level definitions.
	volumeDefs map[string]map[string]any
	internal   map[string]bool // network name → internal: true
}

// service is one Compose service on its way to an App.
type service struct {
	key      string // the Compose name
	app      importplan.App
	set      *importplan.SecretSet
	ports    []port
	networks []string
	// text is everything other services may name this one in (env values,
	// command), to find ports nobody declared.
	text   []string
	public any // x-kwerft.public: nil (default), a hostname, or false
}

type port struct {
	container int32
	protocol  corev1.Protocol
	published bool
	local     bool // published on 127.0.0.1 only
	http      bool // app_protocol says http
}

// ---- top level ----------------------------------------------------------------

func (c *converter) topLevel() {
	c.volumeDefs = map[string]map[string]any{}
	c.internal = map[string]bool{}
	keys := make([]string, 0, len(c.doc))
	for k := range c.doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := c.doc[key]
		switch key {
		case "services", "version", "name":
		case "volumes":
			m, _ := v.(map[string]any)
			for name, def := range m {
				d, _ := def.(map[string]any)
				if d == nil {
					d = map[string]any{}
				}
				c.volumeDefs[name] = d
			}
		case "networks":
			m, _ := v.(map[string]any)
			for name, def := range m {
				if d, ok := def.(map[string]any); ok && truthy(d["internal"]) {
					c.internal[name] = true
				}
			}
		case "secrets", "configs":
			c.plan.Add(importplan.Warn, "", key, "Top-level %s are not imported: Kwerft keeps secret values in secret sets and files in Volumes or Secrets mounted as files.", key)
		case "include":
			c.plan.Add(importplan.Warn, "", key, "include: is not followed; paste the included services into this file.")
		default:
			if !strings.HasPrefix(key, "x-") {
				c.plan.Add(importplan.Warn, "", key, "The top-level key %s is not part of Compose; ignored.", key)
			}
		}
	}
}

// ---- services -------------------------------------------------------------------

// handled are the service keys the import maps (or warns about in its own
// words); ignored are the ones that change nothing in Kubernetes.
var (
	handled = map[string]bool{
		"image": true, "build": true, "command": true, "entrypoint": true, "environment": true, "env_file": true,
		"ports": true, "expose": true, "volumes": true, "deploy": true, "scale": true, "mem_limit": true, "cpus": true,
		"healthcheck": true, "depends_on": true, "networks": true, "network_mode": true, "restart": true,
		"profiles": true, "x-kwerft": true, "privileged": true, "cap_add": true, "devices": true, "user": true,
		"working_dir": true, "tmpfs": true, "secrets": true, "configs": true, "links": true, "extends": true,
		"hostname": true, "container_name": true,
	}
	ignored = map[string]bool{
		"labels": true, "cap_drop": true, "logging": true, "init": true, "tty": true, "stdin_open": true, "platform": true,
		"pull_policy": true, "annotations": true, "attach": true, "develop": true, "stop_grace_period": true,
		"stop_signal": true, "mem_reservation": true, "cpu_shares": true, "cpu_quota": true, "cpu_period": true,
		"cpuset": true, "domainname": true, "label_file": true,
	}
	unsupported = map[string]string{
		"pid":                 "Containers do not share the host's process namespace.",
		"ipc":                 "Containers do not share an IPC namespace.",
		"userns_mode":         "User namespaces are the cluster's.",
		"security_opt":        "Security options are the cluster's (Pod Security baseline, seccomp RuntimeDefault).",
		"sysctls":             "Kernel parameters cannot be set per app.",
		"ulimits":             "Limits are the node's.",
		"shm_size":            "/dev/shm is the container runtime's default (64 MiB).",
		"extra_hosts":         "Extra /etc/hosts entries are not set; apps reach each other by name.",
		"dns":                 "DNS is the cluster's.",
		"dns_search":          "DNS is the cluster's.",
		"dns_opt":             "DNS is the cluster's.",
		"volumes_from":        "Mount the same named volume in both services instead.",
		"external_links":      "Apps reach each other by name within the project.",
		"read_only":           "The root file system stays writable.",
		"cgroup_parent":       "Cgroups are the cluster's.",
		"oom_kill_disable":    "The kernel's OOM killer stays on.",
		"oom_score_adj":       "The kernel's OOM score is the cluster's.",
		"runtime":             "The runtime is the cluster's.",
		"isolation":           "Isolation is the cluster's.",
		"group_add":           "Supplementary groups are the image's.",
		"storage_opt":         "Storage options are the cluster's.",
		"device_cgroup_rules": "Containers get no device access.",
		"gpus":                "GPUs are not offered.",
		"mac_address":         "Addresses are the cluster's.",
		"credential_spec":     "Windows credential specs are not supported.",
		"post_start":          "Lifecycle hooks are not offered.",
		"pre_stop":            "Kwerft drains a stopping replica for a few seconds instead.",
		"blkio_config":        "I/O limits are the node's.",
		"memswap_limit":       "There is no swap.",
		"mem_swappiness":      "There is no swap.",
		"pids_limit":          "Process limits are the node's.",
		"use_api_socket":      "Containers get no Docker socket.",
		"models":              "Models are not offered.",
		"provider":            "Providers are not offered.",
	}
)

func (c *converter) service(key string, raw map[string]any) {
	if p := strList(raw["profiles"]); len(p) > 0 {
		c.plan.Add(importplan.Warn, key, "profiles", "Not imported: the service has the profiles %s, which Compose starts only on request.", strings.Join(p, ", "))
		return
	}
	s := &service{key: key}
	src, ok := c.source(key, raw)
	if !ok {
		return
	}
	s.app.Service = key
	s.app.Spec.Source = src
	name := importplan.SafeName(key, importplan.MaxAppName, "app")
	name = importplan.Unique(name, importplan.MaxAppName, func(n string) bool { return c.apps[n] })
	c.apps[name] = true
	if name != key {
		c.plan.Renames = append(c.plan.Renames, importplan.Rename{Kind: "App", From: key, To: name})
	}
	s.app.Name = name

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c.serviceKey(s, k, raw[k])
	}
	c.command(s, raw)
	c.environment(s, raw)
	c.portsOf(s, raw)
	c.volumesOf(s, raw)
	c.resources(s, raw)
	c.healthcheck(s, raw["healthcheck"])
	c.kwerftExtension(s, raw)
	c.svcs = append(c.svcs, s)
}

// serviceKey warns about one key the import does not map as such.
func (c *converter) serviceKey(s *service, k string, v any) {
	key := s.key
	switch {
	case handled[k] || ignored[k] || strings.HasPrefix(k, "x-"):
	case unsupported[k] != "":
		c.plan.Add(importplan.Warn, key, k, "%s is not supported: %s", k, unsupported[k])
	default:
		c.plan.Add(importplan.Warn, key, k, "%s is not supported; ignored.", k)
	}
	switch k {
	case "privileged":
		if truthy(v) {
			c.plan.Add(importplan.Warn, key, k, "Privileged containers are not allowed (Pod Security baseline): the app runs unprivileged and may fail where it needs more.")
		}
	case "cap_add":
		if caps := strList(v); len(caps) > 0 {
			c.plan.Add(importplan.Warn, key, k, "Capabilities are not added (%s): apps run with the runtime's default set, without privilege escalation.", strings.Join(caps, ", "))
		}
	case "devices":
		c.plan.Add(importplan.Warn, key, k, "Containers get no host devices.")
	case "network_mode":
		c.plan.Add(importplan.Warn, key, k, "network_mode %v is not supported: every app has its own address and reaches the others by name.", v)
	case "user":
		c.plan.Add(importplan.Warn, key, k, "user %v is not applied: the container runs as its image's user.", v)
	case "working_dir":
		c.plan.Add(importplan.Warn, key, k, "working_dir %v is not applied: the container starts in its image's directory. Wrap the command (sh -c \"cd … && …\") if it matters.", v)
	case "tmpfs":
		c.plan.Add(importplan.Warn, key, k, "tmpfs mounts are not made; the paths are on the container's own file system.")
	case "secrets", "configs":
		c.plan.Add(importplan.Warn, key, k, "%s are not mounted: put the values in a secret set, or the files in a Secret mounted as files (app settings › Volumes).", k)
	case "env_file":
		c.plan.Add(importplan.Warn, key, k, "env_file %s is not read: paste its variables under environment (secret-looking ones go to the app's secret set).", strings.Join(strList(v), ", "))
	case "links":
		c.plan.Add(importplan.Warn, key, k, "links are not needed: apps reach each other by service name. Aliases do not exist.")
	case "extends":
		c.plan.Add(importplan.Warn, key, k, "extends is not followed: copy the base service's settings into this one.")
	case "depends_on":
		var deps []string
		if m, ok := v.(map[string]any); ok {
			for d := range m {
				deps = append(deps, d)
			}
			sort.Strings(deps)
		} else {
			deps = strList(v)
		}
		c.plan.Add(importplan.Warn, key, k, "Start order is not kept: all apps start together, so this one must retry (or restart) until %s %s up.", strings.Join(deps, ", "), map[bool]string{true: "is", false: "are"}[len(deps) == 1])
	case "restart":
		if fmt.Sprint(v) == "no" {
			c.plan.Add(importplan.Warn, key, k, "Apps always restart when they exit; use a job for something that runs once.")
		}
	case "hostname":
		c.plan.Add(importplan.Info, key, k, "The hostname is the app's name, %s.", s.app.Name)
	case "networks":
		switch n := v.(type) {
		case map[string]any:
			for name := range n {
				s.networks = append(s.networks, name)
			}
			sort.Strings(s.networks)
		default:
			s.networks = strList(v)
		}
	}
}

// source is the image to run, or the Git repository to build.
func (c *converter) source(key string, raw map[string]any) (kwerftv1.AppSource, bool) {
	image, _ := raw["image"].(string)
	image = strings.TrimSpace(image)
	if b, ok := raw["build"]; ok && b != nil {
		ctx, dockerfile := "", ""
		switch b := b.(type) {
		case string:
			ctx = b
		case map[string]any:
			ctx, _ = b["context"].(string)
			dockerfile, _ = b["dockerfile"].(string)
			for _, k := range []string{"args", "target", "secrets", "ssh", "additional_contexts"} {
				if b[k] != nil {
					c.plan.Add(importplan.Warn, key, "build."+k, "build.%s is not used: Kwerft builds the Dockerfile as it is.", k)
				}
			}
		}
		if g, ok := gitSource(ctx, dockerfile); ok {
			if image != "" {
				c.plan.Add(importplan.Info, key, "image", "Built from %s; the image name %s is not used.", g.Repository, image)
			}
			return kwerftv1.AppSource{Git: g}, true
		}
		if image == "" {
			c.plan.Add(importplan.Warn, key, "build", "Not imported: it is built from local files (%s). Push the image to a registry and set image:, or build from a Git URL.", ctx)
			return kwerftv1.AppSource{}, false
		}
		c.plan.Add(importplan.Info, key, "build", "Not built: Kwerft deploys the image %s, which must be in a registry the cluster can pull from.", image)
	}
	if image == "" {
		c.plan.Add(importplan.Warn, key, "image", "Not imported: the service has no image.")
		return kwerftv1.AppSource{}, false
	}
	if tag := imageTag(image); tag == "" || tag == "latest" {
		c.plan.Add(importplan.Info, key, "image", "%s has no version tag: pin one (like :1.4.2) so a rollback means something.", image)
	}
	return kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: image}}, true
}

// gitSource reads a build context that is a Git URL, with Compose's
// #ref:directory suffix.
func gitSource(ctx, dockerfile string) (*kwerftv1.GitSource, bool) {
	ctx = strings.TrimSpace(ctx)
	if !(strings.HasPrefix(ctx, "https://") || strings.HasPrefix(ctx, "http://") || strings.HasPrefix(ctx, "git@") ||
		strings.HasPrefix(ctx, "ssh://") || strings.HasPrefix(ctx, "git://")) {
		return nil, false
	}
	repo, frag, _ := strings.Cut(ctx, "#")
	ref, dir, _ := strings.Cut(frag, ":")
	g := &kwerftv1.GitSource{Repository: repo, Branch: strings.TrimPrefix(ref, "refs/heads/"), Path: "/" + strings.Trim(dir, "/")}
	if g.Branch == "" {
		g.Branch = "main"
	}
	if dockerfile != "" && !strings.HasPrefix(dockerfile, "/") {
		g.Dockerfile = dockerfile
	}
	return g, true
}

// imageTag is the tag of an image reference ("" without one; a digest
// counts as a tag).
func imageTag(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	if i := strings.LastIndex(last, ":"); i >= 0 {
		return last[i+1:]
	}
	return ""
}

// imageName is the last path element of an image's repository, without tag:
// "postgres" for docker.io/library/postgres:17.
func imageName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	last := ref[strings.LastIndex(ref, "/")+1:]
	last, _, _ = strings.Cut(last, ":")
	return strings.ToLower(last)
}

// escapeDollars keeps a value literal for Kubernetes, which would expand
// $(VAR) in env values, commands and args (and turn $$ into $).
func escapeDollars(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func (c *converter) command(s *service, raw map[string]any) {
	words := func(k string) ([]string, bool) {
		switch v := raw[k].(type) {
		case nil:
			return nil, false
		case string:
			w, err := splitShell(v)
			if err != nil {
				c.plan.Add(importplan.Warn, s.key, k, "%s cannot be split into words (%v); not used.", k, err)
				return nil, false
			}
			return w, true
		default:
			return strList(v), true
		}
	}
	esc := func(w []string) []string {
		out := make([]string, len(w))
		for i, x := range w {
			out[i] = escapeDollars(x)
		}
		return out
	}
	if ep, ok := words("entrypoint"); ok && len(ep) > 0 {
		s.app.Spec.Command = esc(ep)
		s.text = append(s.text, ep...)
	}
	if cmd, ok := words("command"); ok && len(cmd) > 0 {
		s.app.Spec.Args = esc(cmd)
		s.text = append(s.text, cmd...)
	}
	if len(s.app.Spec.Command) > 0 && strings.ContainsAny(s.app.Spec.Command[0], " \t") {
		c.plan.Add(importplan.Warn, s.key, "entrypoint", "The program %q contains a space; give each argument its own item.", s.app.Spec.Command[0])
	}
}

// ---- environment -------------------------------------------------------------------

// secretWords are names, or parts of names between _ . -, that make a
// variable look secret.
var secretWords = []string{"PASSWORD", "PASSWD", "PASS", "PWD", "SECRET", "SECRETS", "TOKEN", "KEY", "APIKEY", "PRIVATEKEY", "CREDENTIALS"}

// LooksSecret reports whether an environment variable's name suggests its
// value is a secret: PASSWORD, SECRET, TOKEN, KEY, _PASS and the like.
func LooksSecret(name string) bool {
	u := strings.ToUpper(name)
	for _, w := range []string{"PASSWORD", "SECRET", "TOKEN", "PASSWD", "PRIVATE_KEY", "API_KEY", "APIKEY", "ACCESS_KEY"} {
		if strings.Contains(u, w) {
			return true
		}
	}
	parts := strings.FieldsFunc(u, func(r rune) bool { return r == '_' || r == '.' || r == '-' })
	for _, p := range parts {
		if slices.Contains(secretWords, p) {
			return true
		}
	}
	return false
}

// credentialURL matches a URL with a password in it, like
// postgres://app:s3cret@db:5432/app.
var credentialURL = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@\s:]*:[^/@\s]+@`)

type envItem struct {
	name  string
	value string
	unset bool // KEY without a value, and none given with the file
}

func (c *converter) envItems(s *service, v any) []envItem {
	var out []envItem
	switch e := v.(type) {
	case map[string]any:
		names := make([]string, 0, len(e))
		for k := range e {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if e[k] == nil {
				val, ok := c.opt.Env[k]
				out = append(out, envItem{name: k, value: val, unset: !ok})
				continue
			}
			out = append(out, envItem{name: k, value: scalar(e[k])})
		}
	case []any:
		for _, item := range e {
			str := scalar(item)
			k, val, ok := strings.Cut(str, "=")
			if !ok {
				v, given := c.opt.Env[k]
				out = append(out, envItem{name: k, value: v, unset: !given})
				continue
			}
			out = append(out, envItem{name: k, value: val})
		}
	case nil:
	default:
		c.plan.Add(importplan.Warn, s.key, "environment", "environment is neither a list nor a mapping; ignored.")
	}
	return out
}

func (c *converter) environment(s *service, raw map[string]any) {
	for _, e := range c.envItems(s, raw["environment"]) {
		if !validEnvName(e.name) {
			c.plan.Add(importplan.Warn, s.key, "environment", "%q is not a valid variable name; skipped.", e.name)
			continue
		}
		s.text = append(s.text, e.value)
		secretURL := !LooksSecret(e.name) && credentialURL.MatchString(e.value)
		if secretURL {
			c.plan.Add(importplan.Info, s.key, "environment", "%s holds a password in its URL: kept in the app's secret set.", e.name)
		}
		if !LooksSecret(e.name) && !secretURL {
			if e.unset {
				c.plan.Add(importplan.Warn, s.key, "environment", "%s has no value here; it is left out. Give it with the file, or set it in the app's settings.", e.name)
				continue
			}
			s.app.Spec.Env = append(s.app.Spec.Env, corev1.EnvVar{Name: e.name, Value: escapeDollars(e.value)})
			continue
		}
		if s.set == nil {
			s.set = &importplan.SecretSet{Name: importplan.AppEnvSet(s.app.Name), App: s.app.Name,
				Description: "Secret variables of " + s.app.Name + ", from its Compose file", Values: map[string]string{}}
		}
		s.app.Spec.Env = append(s.app.Spec.Env, corev1.EnvVar{Name: e.name, ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: s.set.Name}, Key: e.name}}})
		if e.value == "" {
			s.set.Missing = append(s.set.Missing, e.name)
			c.plan.Add(importplan.Warn, s.key, "environment", "%s looks secret but has no value: set it on the Secrets page (%s). The app waits for it.", e.name, s.set.Name)
			continue
		}
		s.set.Values[e.name] = e.value
		s.set.Keys = append(s.set.Keys, e.name)
	}
}

func validEnvName(n string) bool {
	if n == "" || n[0] >= '0' && n[0] <= '9' {
		return false
	}
	for i := 0; i < len(n); i++ {
		ch := n[i]
		if !(isNameChar(ch) || ch == '-' || ch == '.') {
			return false
		}
	}
	return true
}

// ---- helpers ------------------------------------------------------------------------

// scalar is a YAML scalar as Compose sees it: a string.
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, scalar(item))
		}
		return out
	default:
		return []string{scalar(x)}
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	}
	return false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), x == float64(int(x))
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	}
	return 0, false
}
