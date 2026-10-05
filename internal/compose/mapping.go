package compose

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/importplan"
)

// ---- ports -----------------------------------------------------------------------

// nonHTTP are well-known ports of protocols other than HTTP: published,
// they stay inside the cluster rather than getting a public hostname.
var nonHTTP = map[int32]bool{
	21: true, 22: true, 23: true, 25: true, 53: true, 110: true, 143: true, 389: true, 465: true, 587: true,
	636: true, 993: true, 995: true, 1433: true, 1521: true, 1883: true, 2049: true, 2181: true, 3306: true,
	3389: true, 4222: true, 5432: true, 5433: true, 5671: true, 5672: true, 6379: true, 6380: true, 6432: true,
	7000: true, 7001: true, 7687: true, 8883: true, 9042: true, 9092: true, 9093: true, 9160: true,
	11211: true, 26257: true, 27017: true, 27018: true, 27019: true, 28015: true, 33060: true,
}

func (c *converter) portsOf(s *service, raw map[string]any) {
	add := func(p port) {
		for i, q := range s.ports {
			if q.container == p.container && q.protocol == p.protocol {
				s.ports[i].published = q.published || p.published
				s.ports[i].local = q.local && p.local
				s.ports[i].http = q.http || p.http
				return
			}
		}
		s.ports = append(s.ports, p)
	}
	for i, item := range anyList(raw["ports"]) {
		key := fmt.Sprintf("ports[%d]", i)
		switch v := item.(type) {
		case map[string]any:
			target, ok := toInt(v["target"])
			if !ok || target < 1 || target > 65535 {
				c.plan.Add(importplan.Warn, s.key, key, "The port has no valid target; skipped.")
				continue
			}
			p := port{container: int32(target), protocol: protocolOf(scalar(v["protocol"])), published: true}
			host := scalar(v["host_ip"])
			p.local = host == "127.0.0.1" || host == "::1" || host == "localhost"
			ap := strings.ToLower(scalar(v["app_protocol"]))
			p.http = ap == "http" || ap == "http2" || ap == "h2c"
			add(p)
		default:
			p, err := parsePort(scalar(v))
			if err != nil {
				c.plan.Add(importplan.Warn, s.key, key, "%s; skipped.", err)
				continue
			}
			add(p)
		}
	}
	for i, item := range anyList(raw["expose"]) {
		str := scalar(item)
		num, proto, _ := strings.Cut(str, "/")
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || n > 65535 {
			c.plan.Add(importplan.Warn, s.key, fmt.Sprintf("expose[%d]", i), "%q is not a port (ranges are not supported); skipped.", str)
			continue
		}
		add(port{container: int32(n), protocol: protocolOf(proto)})
	}
}

func protocolOf(s string) corev1.Protocol {
	if strings.EqualFold(s, "udp") {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

// parsePort reads the short syntax [[ip:]published:]container[/protocol].
func parsePort(s string) (port, error) {
	s = strings.TrimSpace(s)
	spec, proto, _ := strings.Cut(s, "/")
	p := port{protocol: protocolOf(proto), published: true}
	host := ""
	if strings.HasPrefix(spec, "[") { // [::1]:8080:80
		end := strings.Index(spec, "]")
		if end < 0 {
			return p, fmt.Errorf("%q is not a port mapping", s)
		}
		host, spec = spec[1:end], strings.TrimPrefix(spec[end+1:], ":")
	}
	parts := strings.Split(spec, ":")
	if len(parts) == 3 {
		host, parts = parts[0], parts[1:]
	}
	target := parts[len(parts)-1]
	if strings.Contains(target, "-") {
		return p, fmt.Errorf("the port range %s is not supported: list the ports one by one", target)
	}
	n, err := strconv.Atoi(target)
	if err != nil || n < 1 || n > 65535 {
		return p, fmt.Errorf("%q is not a port mapping", s)
	}
	p.container = int32(n)
	p.local = host == "127.0.0.1" || host == "::1" || host == "localhost"
	return p, nil
}

// ---- volumes ---------------------------------------------------------------------

// dataDirs are where well-known images keep their data: without a volume
// there, it is lost when the app restarts.
var dataDirs = map[string]string{
	"postgres": "/var/lib/postgresql/data", "postgis": "/var/lib/postgresql/data", "pgvector": "/var/lib/postgresql/data",
	"timescaledb": "/var/lib/postgresql/data", "mysql": "/var/lib/mysql", "mariadb": "/var/lib/mysql",
	"mongo": "/data/db", "redis": "/data", "valkey": "/data", "minio": "/data", "clickhouse-server": "/var/lib/clickhouse",
	"elasticsearch": "/usr/share/elasticsearch/data", "rabbitmq": "/var/lib/rabbitmq", "influxdb": "/var/lib/influxdb2",
}

var modeWords = map[string]bool{"ro": true, "rw": true, "z": true, "Z": true, "nocopy": true, "cached": true, "delegated": true, "consistent": true}

func (c *converter) volumesOf(s *service, raw map[string]any) {
	for i, item := range anyList(raw["volumes"]) {
		key := fmt.Sprintf("volumes[%d]", i)
		var kind, source, target string
		readOnly := false
		switch v := item.(type) {
		case map[string]any:
			kind, source, target = scalar(v["type"]), scalar(v["source"]), scalar(v["target"])
			readOnly = truthy(v["read_only"])
			if kind == "" {
				kind = "volume"
			}
		default:
			parts := strings.Split(scalar(v), ":")
			switch {
			case len(parts) == 1:
				target = parts[0]
			case len(parts) == 2 && modeWords[parts[1]] && strings.HasPrefix(parts[0], "/"):
				target = parts[0]
				readOnly = parts[1] == "ro"
			case len(parts) >= 2:
				source, target = parts[0], parts[1]
				if len(parts) > 2 {
					readOnly = slices.Contains(strings.Split(parts[2], ","), "ro")
				}
			}
			switch {
			case source == "":
				kind = "volume"
			case strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~"):
				kind = "bind"
			default:
				kind = "volume"
			}
		}
		if !strings.HasPrefix(target, "/") {
			c.plan.Add(importplan.Warn, s.key, key, "%q is not an absolute path in the container; skipped.", target)
			continue
		}
		switch {
		case kind == "bind":
			c.plan.Add(importplan.Warn, s.key, key, "The bind mount %s → %s is not made: the server's files are not the app's. Use a named volume and copy the files in, or a Secret mounted as files for configuration.", source, target)
		case kind == "tmpfs":
			c.plan.Add(importplan.Warn, s.key, key, "The tmpfs at %s is not made; the path is on the container's own file system.", target)
		case kind != "volume":
			c.plan.Add(importplan.Warn, s.key, key, "Mounts of type %s are not supported; %s skipped.", kind, target)
		case source == "":
			c.plan.Add(importplan.Warn, s.key, key, "The anonymous volume at %s is not kept: name it (like data:%s) to get a Volume.", target, target)
		default:
			name := c.volume(s.key, source)
			if slices.ContainsFunc(s.app.Spec.Volumes, func(v kwerftv1.AppVolume) bool { return v.Path == target }) {
				c.plan.Add(importplan.Warn, s.key, key, "%s is mounted twice; the second mount is skipped.", target)
				continue
			}
			s.app.Spec.Volumes = append(s.app.Spec.Volumes, kwerftv1.AppVolume{Path: target, Volume: name, ReadOnly: readOnly})
		}
	}
	if s.app.Spec.Source.Image != nil {
		if dir := dataDirs[imageName(s.app.Spec.Source.Image.Ref)]; dir != "" {
			covered := slices.ContainsFunc(s.app.Spec.Volumes, func(v kwerftv1.AppVolume) bool {
				return dir == v.Path || strings.HasPrefix(dir, strings.TrimSuffix(v.Path, "/")+"/")
			})
			if !covered {
				c.plan.Add(importplan.Warn, s.key, "volumes", "Its data (%s) is on the container's file system and lost when the app restarts: mount a named volume there.", dir)
			}
		}
	}
}

// volume is the Volume for a named Compose volume, created once however many
// services mount it.
func (c *converter) volume(service, source string) string {
	if name, ok := c.volumes[source]; ok {
		return name
	}
	def := c.volumeDefs[source]
	external := def != nil && (truthy(def["external"]) || isMap(def["external"]))
	base := source
	if n := scalar(def["name"]); n != "" {
		base = n
	}
	if ext, ok := def["external"].(map[string]any); ok && scalar(ext["name"]) != "" {
		base = scalar(ext["name"])
	}
	name := importplan.SafeName(base, 63, "vol")
	if !external {
		name = importplan.Unique(name, 63, func(n string) bool { return slices.Contains(mapValues(c.volumes), n) })
	}
	c.volumes[source] = name
	if name != base {
		c.plan.Renames = append(c.plan.Renames, importplan.Rename{Kind: "Volume", From: base, To: name})
	}
	if external {
		c.plan.Add(importplan.Info, service, "volumes", "%s is external: the app mounts the project's Volume %s, which must exist.", source, name)
		return name
	}
	spec := kwerftv1.VolumeSpec{Size: resource.MustParse("5Gi"), Class: "local-nvme"}
	if d := scalar(def["driver"]); d != "" && d != "local" {
		c.plan.Add(importplan.Warn, "", "volumes."+source, "The volume driver %s is not used: Kwerft makes a disk.", d)
	}
	if def["driver_opts"] != nil {
		c.plan.Add(importplan.Warn, "", "volumes."+source, "driver_opts are not used: Kwerft makes a disk.")
	}
	if x, ok := def["x-kwerft"].(map[string]any); ok {
		if sz := scalar(x["size"]); sz != "" {
			q, err := resource.ParseQuantity(sz)
			if err != nil || q.Sign() <= 0 {
				c.plan.Add(importplan.Warn, "", "volumes."+source, "x-kwerft size %q is not a size like 20Gi; using 5Gi.", sz)
			} else {
				spec.Size = q
			}
		}
		if cl := scalar(x["class"]); cl != "" {
			if cl != "local-nvme" && cl != "hcloud-volume" {
				c.plan.Add(importplan.Warn, "", "volumes."+source, "x-kwerft class %q is not local-nvme or hcloud-volume; using local-nvme.", cl)
			} else {
				spec.Class = cl
			}
		}
	}
	c.plan.Volumes = append(c.plan.Volumes, importplan.Volume{Name: name, Spec: spec})
	return name
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// ---- resources --------------------------------------------------------------------

// heavy images get more than the small size by default.
var heavy = map[string]string{
	"mysql": "medium", "mariadb": "medium", "mongo": "medium", "wordpress": "medium", "rabbitmq": "medium",
	"elasticsearch": "large", "opensearch": "large", "clickhouse-server": "large", "keycloak": "large", "sonarqube": "large",
}

var presets = []struct {
	name string
	mem  string
	note string
}{{"small", "256Mi", "0.25 CPU, 256 MiB"}, {"medium", "512Mi", "0.5 CPU, 512 MiB"}, {"large", "2Gi", "1 CPU, 2 GiB"}}

func (c *converter) resources(s *service, raw map[string]any) {
	deploy, _ := raw["deploy"].(map[string]any)
	replicas := -1
	if n, ok := toInt(raw["scale"]); ok {
		replicas = n
	}
	var mem, cpus string
	if v := scalar(raw["mem_limit"]); v != "" {
		mem = v
	}
	if v := scalar(raw["cpus"]); v != "" {
		cpus = v
	}
	if deploy != nil {
		keys := make([]string, 0, len(deploy))
		for k := range deploy {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var unused []string
		for _, k := range keys {
			switch k {
			case "replicas":
				if n, ok := toInt(deploy[k]); ok {
					replicas = n
				}
			case "resources":
				r, _ := deploy[k].(map[string]any)
				limits, _ := r["limits"].(map[string]any)
				reservations, _ := r["reservations"].(map[string]any)
				if v := scalar(limits["memory"]); v != "" {
					mem = v
				} else if v := scalar(reservations["memory"]); v != "" && mem == "" {
					mem = v
				}
				if v := scalar(limits["cpus"]); v != "" {
					cpus = v
				}
			case "mode":
				if scalar(deploy[k]) == "global" {
					c.plan.Add(importplan.Warn, s.key, "deploy.mode", "global mode (one replica per node) is not supported: the app runs the replicas it is given.")
				}
			case "placement":
				c.plan.Add(importplan.Warn, s.key, "deploy.placement", "Placement constraints are not used: the cluster schedules the app.")
			default:
				unused = append(unused, k)
			}
		}
		if len(unused) > 0 {
			c.plan.Add(importplan.Info, s.key, "deploy", "deploy.%s not used: Kwerft rolls out, restarts and updates apps itself.", strings.Join(unused, ", deploy."))
		}
	}
	if replicas >= 0 {
		r := int32(replicas)
		s.app.Spec.Replicas = &r
	}
	size := "small"
	if s.app.Spec.Source.Image != nil {
		if h := heavy[imageName(s.app.Spec.Source.Image.Ref)]; h != "" {
			size = h
		}
	}
	if mem == "" {
		s.app.Spec.Size = size
		if size != "small" {
			c.plan.Add(importplan.Info, s.key, "", "Size %s (%s), as this image usually needs; change it in the app's settings.", size, presetNote(size))
		}
		return
	}
	bytes, err := parseBytes(mem)
	if err != nil {
		c.plan.Add(importplan.Warn, s.key, "deploy.resources", "The memory limit %q cannot be read; size %s.", mem, size)
		s.app.Spec.Size = size
		return
	}
	for _, p := range presets {
		if q := resource.MustParse(p.mem); bytes <= q.Value() {
			s.app.Spec.Size = p.name
			c.plan.Add(importplan.Info, s.key, "deploy.resources", "Size %s (%s) for its memory limit %s.", p.name, p.note, mem)
			return
		}
	}
	// More than the large preset: the limit as it is.
	memQ := *resource.NewQuantity(bytes, resource.BinarySI)
	cpuQ := resource.MustParse("1")
	if cpus != "" {
		if q, err := resource.ParseQuantity(cpus); err == nil && q.Sign() > 0 {
			cpuQ = q
		}
	}
	s.app.Spec.Size = "custom"
	s.app.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpuQ, corev1.ResourceMemory: memQ},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: memQ},
	}
	c.plan.Add(importplan.Info, s.key, "deploy.resources", "Custom size: %s CPU, %s memory, from its limits.", cpuQ.String(), memQ.String())
}

func presetNote(name string) string {
	for _, p := range presets {
		if p.name == name {
			return p.note
		}
	}
	return ""
}

var bytesRE = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([bkmgt]?)(?:i?b)?$`)

// parseBytes reads Docker's memory sizes: 512m, 1g, 1.5GB, 268435456.
func parseBytes(s string) (int64, error) {
	m := bytesRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("not a size")
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "b": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}[m[2]]
	return int64(f * mult), nil
}

// ---- health checks ----------------------------------------------------------------

var (
	httpCheckRE = regexp.MustCompile(`\b(curl|wget)\b.*?\b(https?)://(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])(?::(\d+))?(/[^\s'"|;&)]*)?`)
	ncCheckRE   = regexp.MustCompile(`\bnc\b[^|;&]*-z[^|;&]*?\s(?:localhost|127\.0\.0\.1|0\.0\.0\.0)\s+(\d+)`)
	devTCPRE    = regexp.MustCompile(`/dev/tcp/(?:localhost|127\.0\.0\.1)/(\d+)`)
	portFlagRE  = regexp.MustCompile(`\s(?:-p|--port)[ =]?(\d+)`)
)

// dbChecks are database clients that check their own server: mapped to a
// TCP check on its port.
var dbChecks = []struct {
	tool string
	port int32
}{{"pg_isready", 5432}, {"redis-cli", 6379}, {"valkey-cli", 6379}, {"mysqladmin", 3306}, {"mariadb-admin", 3306},
	{"healthcheck.sh", 3306}, {"mongosh", 27017}, {"mongo ", 27017}, {"clickhouse-client", 9000}}

func (c *converter) healthcheck(s *service, v any) {
	hc, ok := v.(map[string]any)
	if !ok || truthy(hc["disable"]) {
		return
	}
	var cmd string
	switch t := hc["test"].(type) {
	case string:
		cmd = t
	case []any:
		words := strList(t)
		if len(words) == 0 || words[0] == "NONE" {
			return
		}
		if words[0] == "CMD-SHELL" {
			cmd = strings.Join(words[1:], " ")
		} else {
			cmd = strings.Join(words[1:], " ")
			if words[0] != "CMD" {
				cmd = strings.Join(words, " ")
			}
		}
	default:
		return
	}
	if sp := scalar(hc["start_period"]); sp != "" {
		if d, err := time.ParseDuration(sp); err == nil && d >= 45*time.Second {
			c.plan.Add(importplan.Warn, s.key, "healthcheck", "Not mapped: the service takes up to %s to start, and Kwerft's check would restart it after about a minute. Add a check in the app's settings once it runs.", sp)
			return
		}
	}
	if m := httpCheckRE.FindStringSubmatch(cmd); m != nil {
		if m[2] == "https" {
			c.plan.Add(importplan.Warn, s.key, "healthcheck", "Not mapped: the check uses HTTPS inside the container; Kwerft checks plain HTTP or TCP.")
			return
		}
		port := int32(80)
		if m[3] != "" {
			n, _ := strconv.Atoi(m[3])
			port = int32(n)
		}
		path := m[4]
		if path == "" {
			path = "/"
		}
		s.app.Spec.HealthCheck = &kwerftv1.HealthCheck{HTTP: path, Port: port}
		return
	}
	for _, re := range []*regexp.Regexp{ncCheckRE, devTCPRE} {
		if m := re.FindStringSubmatch(cmd); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n >= 1 && n <= 65535 {
				s.app.Spec.HealthCheck = &kwerftv1.HealthCheck{Port: int32(n)}
				return
			}
		}
	}
	for _, d := range dbChecks {
		if !strings.Contains(cmd+" ", d.tool) {
			continue
		}
		port := d.port
		if m := portFlagRE.FindStringSubmatch(cmd); m != nil {
			n, _ := strconv.Atoi(m[1])
			port = int32(n)
		}
		s.app.Spec.HealthCheck = &kwerftv1.HealthCheck{Port: port}
		c.plan.Add(importplan.Info, s.key, "healthcheck", "%s became a TCP check on port %d.", strings.TrimSpace(d.tool), port)
		return
	}
	c.plan.Add(importplan.Warn, s.key, "healthcheck", "Not mapped: Kwerft checks an HTTP path or a TCP port, not a command (%s). Add one in the app's settings.", truncate(cmd, 80))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- x-kwerft ----------------------------------------------------------------------

// kwerftExtension reads the service's x-kwerft block: size, egress, public.
func (c *converter) kwerftExtension(s *service, raw map[string]any) {
	x, ok := raw["x-kwerft"].(map[string]any)
	if !ok {
		return
	}
	if v := scalar(x["size"]); v != "" {
		if presetNote(v) == "" {
			c.plan.Add(importplan.Warn, s.key, "x-kwerft.size", "size %q is not small, medium or large; kept %s.", v, s.app.Spec.Size)
		} else {
			s.app.Spec.Size, s.app.Spec.Resources = v, nil
		}
	}
	if v := scalar(x["egress"]); v != "" {
		if v != "none" && v != "https" && v != "all" {
			c.plan.Add(importplan.Warn, s.key, "x-kwerft.egress", "egress %q is not none, https or all; ignored.", v)
		} else {
			s.app.Spec.Egress = v
		}
	}
	switch v := x["public"].(type) {
	case nil:
	case bool:
		if !v {
			s.public = false
		}
	default:
		h := strings.ToLower(strings.TrimSpace(scalar(v)))
		if !hostnameRE.MatchString(h) || len(h) > 253 {
			c.plan.Add(importplan.Warn, s.key, "x-kwerft.public", "%q is not a hostname; ignored.", h)
		} else {
			s.public = h
		}
	}
}

var hostnameRE = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`)

// ---- across services --------------------------------------------------------------

// defaultPorts are what well-known images listen on, for services that
// declare no port (other services reach them all the same in Compose).
var defaultPorts = map[string][]int32{
	"postgres": {5432}, "postgis": {5432}, "pgvector": {5432}, "timescaledb": {5432}, "mysql": {3306}, "mariadb": {3306},
	"redis": {6379}, "valkey": {6379}, "keydb": {6379}, "mongo": {27017}, "memcached": {11211}, "rabbitmq": {5672},
	"elasticsearch": {9200}, "opensearch": {9200}, "clickhouse-server": {8123, 9000}, "nats": {4222}, "minio": {9000},
	"kafka": {9092}, "influxdb": {8086}, "meilisearch": {7700}, "typesense": {8108}, "qdrant": {6333},
	"nginx": {80}, "httpd": {80}, "caddy": {80}, "wordpress": {80}, "ghost": {2368}, "mailpit": {1025, 8025},
}

func (c *converter) crossReferences() {
	// Ports nobody declared, found where other services name them.
	for _, s := range c.svcs {
		if len(s.ports) > 0 {
			continue
		}
		re := regexp.MustCompile(`(?:^|[^A-Za-z0-9._-])` + regexp.QuoteMeta(s.key) + `:(\d{2,5})(?:[^0-9]|$)`)
		named := false
		found := map[int32]bool{}
		mention := regexp.MustCompile(`(?:^|[^A-Za-z0-9._-])` + regexp.QuoteMeta(s.key) + `(?:[^A-Za-z0-9_-]|$)`)
		for _, o := range c.svcs {
			if o == s {
				continue
			}
			for _, t := range o.text {
				for _, m := range re.FindAllStringSubmatch(t, -1) {
					if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 && n <= 65535 {
						found[int32(n)] = true
					}
				}
				named = named || mention.MatchString(t)
			}
		}
		var ports []int32
		for p := range found {
			ports = append(ports, p)
		}
		slices.Sort(ports)
		how := "as other services name it"
		if len(ports) == 0 && s.app.Spec.Source.Image != nil {
			ports = defaultPorts[imageName(s.app.Spec.Source.Image.Ref)]
			how = "the image's usual port"
		}
		for _, p := range ports {
			s.ports = append(s.ports, port{container: p, protocol: corev1.ProtocolTCP})
		}
		switch {
		case len(ports) > 0:
			c.plan.Add(importplan.Info, s.key, "ports", "Reachable inside the project on port %s (%s).", joinPorts(ports), how)
		case named:
			c.plan.Add(importplan.Warn, s.key, "ports", "Other services name it, but it declares no port: add expose: [PORT], or the port in the app's settings, or they cannot reach it.")
		}
	}
	// Ports, public hostnames, and who may connect.
	for _, s := range c.svcs {
		c.appPorts(s)
	}
	for _, s := range c.svcs {
		if len(s.app.Spec.Ports) == 0 {
			continue
		}
		var from []string
		for _, o := range c.svcs {
			if o != s && sharesNetwork(s, o) {
				from = append(from, o.app.Name)
			}
		}
		sort.Strings(from)
		s.app.Spec.AllowFrom = from
		if s.app.Spec.HealthCheck != nil && !slices.ContainsFunc(s.app.Spec.Ports, func(p kwerftv1.AppPort) bool { return p.Container == s.app.Spec.HealthCheck.Port }) {
			c.plan.Add(importplan.Info, s.key, "healthcheck", "The check connects to port %d, which the app does not declare.", s.app.Spec.HealthCheck.Port)
		}
	}
	for _, s := range c.svcs {
		nets := networksOf(s)
		if len(nets) > 0 && !slices.ContainsFunc(nets, func(n string) bool { return !c.internal[n] }) && s.app.Spec.Egress == "" {
			s.app.Spec.Egress = "none"
			c.plan.Add(importplan.Info, s.key, "networks", "No outbound internet access: its networks are internal.")
		}
	}
	// Renamed services: other services may name them by the old name.
	for _, r := range c.plan.Renames {
		if r.Kind != "App" {
			continue
		}
		mention := regexp.MustCompile(`(?:^|[^A-Za-z0-9._-])` + regexp.QuoteMeta(r.From) + `(?:[^A-Za-z0-9_-]|$)`)
		var users []string
		for _, o := range c.svcs {
			if o.key != r.From && slices.ContainsFunc(o.text, mention.MatchString) {
				users = append(users, o.key)
			}
		}
		if len(users) > 0 {
			c.plan.Add(importplan.Warn, r.From, "", "Renamed to %s, but %s still name it %s: change those values to %s after the import.", r.To, strings.Join(users, ", "), r.From, r.To)
		}
	}
}

func joinPorts(ps []int32) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = strconv.Itoa(int(p))
	}
	return strings.Join(out, ", ")
}

func networksOf(s *service) []string {
	if len(s.networks) == 0 {
		return []string{"default"}
	}
	return s.networks
}

func sharesNetwork(a, b *service) bool {
	return slices.ContainsFunc(networksOf(a), func(n string) bool { return slices.Contains(networksOf(b), n) })
}

// appPorts sets the App's ports: every port inside the cluster, published
// HTTP ports also on a public hostname under the apps domain.
func (c *converter) appPorts(s *service) {
	publicCount := 0
	noDomain := false
	for _, p := range s.ports {
		ap := kwerftv1.AppPort{Container: p.container, Protocol: p.protocol}
		candidate := p.published && !p.local && p.protocol == corev1.ProtocolTCP && (p.http || !nonHTTP[p.container]) && s.public != false
		if p.published && p.container == 443 && !p.http {
			c.plan.Add(importplan.Warn, s.key, "ports", "Port 443 serves HTTPS itself: Kwerft ends TLS at its gateway and forwards plain HTTP, so it stays inside the cluster. Publish the app's HTTP port instead.")
			candidate = false
		}
		if p.published && p.local {
			c.plan.Add(importplan.Info, s.key, "ports", "Port %d is published on localhost only: it stays inside the cluster.", p.container)
		}
		if candidate {
			host := ""
			switch h := s.public.(type) {
			case string:
				if publicCount == 0 {
					host = h
				}
			}
			if host == "" && c.opt.AppsDomain != "" {
				label := s.app.Name
				if publicCount > 0 {
					suffix := fmt.Sprintf("-%d", p.container)
					if len(label)+len(suffix) > 63 {
						label = strings.TrimRight(label[:63-len(suffix)], "-")
					}
					label += suffix
				}
				host = label + "." + c.opt.AppsDomain
			}
			if host == "" {
				noDomain = true
			} else {
				ap.Public = host
				publicCount++
			}
		} else if p.published && !p.local && p.container != 443 && s.public != false && p.protocol == corev1.ProtocolTCP {
			c.plan.Add(importplan.Info, s.key, "ports", "Port %d is not HTTP: it stays inside the cluster, for the project's apps.", p.container)
		}
		s.app.Spec.Ports = append(s.app.Spec.Ports, ap)
	}
	if noDomain {
		c.plan.Add(importplan.Warn, s.key, "ports", "No apps domain is set (Settings), so its published ports stay inside the cluster: add a public hostname in the app's settings.")
	}
}

func anyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	default:
		return []any{x}
	}
}
