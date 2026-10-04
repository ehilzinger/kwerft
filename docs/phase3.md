# Phase 3 — Monitoring & logs: work split and contracts

Exit criterion: a crash-looping app raises an alert in Slack within
2 minutes, with a link to its logs. Milestone: usable by the team.

The stack is already installed by `install.sh` (stage "Observability") in
namespace `kwerft-observability`: VictoriaMetrics k8s stack (vmsingle,
vmagent, vmalert, Alertmanager, the operator with its `VM*` CRDs,
kube-state-metrics, node-exporter) and VictoriaLogs with Vector. Phase 3
brings it into Kwerft.

Four workers build this in parallel, each in its own branch. This file is
the contract between them. Shared, already on main: `api/v1alpha1`
(`AlertRule`, `NotificationChannel`), `internal/observability` (service
URLs, label names, secret names) and the Monitoring page skeleton
(`web/src/pages/Monitoring.tsx` with one placeholder file per tab).

## Ownership

| Worker | Owns | Touches lightly |
|---|---|---|
| W1 metrics | `internal/metrics/` (query client, query catalog), `internal/server/api_metrics.go`, Kwerft's own `/metrics` (collectors in `internal/controllers/metrics*.go`), recording rules and scrape config (chart templates `metrics-*.yaml`), Traefik metrics in `install.sh`; UI: `MonitoringMetrics.tsx`, the App detail **Metrics** tab, `web/src/metrics.ts`, a small chart component | `main.go` (metrics server), `AppDetail.tsx` (adding the tab) |
| W2 logs | `internal/logs/` (VictoriaLogs client), `internal/server/api_logsearch.go`, Vector configuration (installer values / chart), history fallback for Task and Build logs whose pod is gone; UI: `MonitoringLogs.tsx`, history search in the App **Logs** tab, `web/src/logsearch.ts`, the `?tab=logs` deep link on `/apps/$project/$name` (alerts link there) | `api_logs.go`, `api_builds.go`, `api_jobs.go` (fallback only), `LogViewer`, `router.tsx` (search param) |
| W3 alerting backend | `internal/controllers/alerting_*.go` (AlertRule → VMRule, channels → Alertmanager config, default rules), `internal/alerting/` (expressions per condition, Alertmanager client, notifiers' test send), `internal/server/api_alerts.go`; RBAC for channel secrets | `roles.yaml` + `internal/access`, `main.go` registration, `install.sh` only if Alertmanager/vmalert settings must change (coordinate in the report) |
| W4 alerting UI & attention | `MonitoringAlerts.tsx`, `MonitoringRules.tsx`, `MonitoringChannels.tsx`, `web/src/alerts.ts`, the Overview page's "needs attention" feed and the sidebar badge | `Overview.tsx`, `Shell.tsx` |

## Security

- Metrics and logs are read with the console's own identity from in-cluster
  endpoints (`internal/observability`), never exposed through the Gateway.
- Every query is confined to project namespaces: the console adds the
  filter itself (VictoriaMetrics `extra_filters[]` /
  `extra_label=namespace=…`; VictoriaLogs `extra_filters`), from the list of
  namespaces labelled `kwerft.dev/project` that the user may read (today: all
  projects for every role; Phase 4 narrows per project). User input never
  becomes a label matcher on `namespace` directly.
- Platform namespaces (`kwerft-system`, `kwerft-observability`,
  `kwerft-builds`, `kube-system`, …) and node metrics are for owners and
  admins only; build logs are readable through the Build (as in Phase 2).
- Custom alert expressions (`AlertRule.spec.expr`) are owners' and admins';
  developers create rules from the built-in conditions only.
- Notification secrets: Secret `notify-<channel>` in `kwerft-observability`,
  write-only for owners and admins (patch, never get), with a Role the
  reconciler keeps (as for Git connections in Phase 2).

## Metric names (W1 produces, W3 consumes)

Kwerft's own metrics, served by the controller manager on `:8081/metrics`
(not on the console port) and scraped through a `VMServiceScrape`:

```
kwerft_schedule_last_success_timestamp_seconds{namespace, schedule}
kwerft_schedule_last_failure_timestamp_seconds{namespace, schedule}
kwerft_app_latest_build_failed{namespace, app}                 0 | 1
kwerft_build_duration_seconds_bucket{namespace, app, result}   histogram
kwerft_domain_certificate_expiry_timestamp_seconds{namespace, domain, hostname}
kwerft_console_certificate_expiry_timestamp_seconds{hostname, purpose}
```

Recording rules (a `VMRule` in the chart), so alerts and charts use app
labels instead of Traefik's or the kubelet's:

```
kwerft:http_requests:rate5m{namespace, app, code_class}        # 2xx..5xx, from Traefik
kwerft:http_latency_p95_seconds:5m{namespace, app}
kwerft:container_memory_working_set_bytes{namespace, app, pod}
kwerft:container_cpu_usage_cores:rate5m{namespace, app, pod}
```

`app` comes from the pod label `kwerft.dev/app` (kube-state-metrics must
export it: `metricLabelsAllowlist` for pods) or, for Traefik, from the
HTTPRoute/Service name Kwerft renders.

## Alerting (W3)

- One `VMRule` named `kwerft-alerts` in `kwerft-observability`, one group
  per AlertRule; every alert carries `kwerft_rule=<rule>`,
  `severity=<critical|warning|info>`, and `namespace`/`app` where they apply.
  Annotations: `summary`, `description`, `console_url` (deep link: the app's
  Logs tab for app conditions, `https://<console>/apps/<p>/<a>?tab=logs`).
- Routes: Alertmanager receives one route per AlertRule with channels,
  matching `kwerft_rule`; receivers per NotificationChannel (Slack, email,
  webhook, ntfy — ntfy through its webhook-compatible JSON or a small
  adapter in the console; decide and document). Check whether the operator
  adds a namespace matcher to `VMAlertmanagerConfig` routes and avoid it
  breaking cluster-wide alerts.
- Timing for the exit criterion: CrashLooping fires from
  `kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff"}` with
  a short `for`; vmalert evaluation interval and Alertmanager
  `group_wait` must leave the end-to-end time under 2 minutes.
- Default rules (label `kwerft.dev/default=true`, recreated if deleted,
  editable): CrashLooping (critical), Restarts (> 5 in 15m, warning),
  MemoryHigh (> 90 % of limit for 10m), VolumeFillingUp (> 85 % or full
  within 7d), NodeMemoryPressure (< 10 % for 10m), NodeDiskPressure
  (< 10 %), CertificateExpiring (< 14d), ScheduleFailing, BuildFailing.
  None has channels until an owner adds one.

## Console API

W1 (metrics):

```
GET /api/v1/metrics/overview?range=1h      → {nodes: [...], platform: {...}, topApps: [{project, app, cpu, memory}], series?}
GET /api/v1/projects/{p}/apps/{a}/metrics?range=1h&step=…
    → {cpu, memory, restarts, requests, errors, latencyP95}: each [{t, v}] (null when unavailable)
GET /api/v1/metrics/query?query=…&range=…  (owners/admins: explore; others: confined to projects)
```

W2 (logs):

```
GET /api/v1/logs?query=<LogsQL>&project=…&app=…&since=…&until=…&limit=…   → {entries: [{time, project?, app?, pod, container, line, stream}], truncated}
GET /api/v1/logs/tail?…   SSE, like the existing pod log stream
```

W3 (alerting):

```
GET    /api/v1/alerts?state=firing|silenced|resolved          → [Alert]
POST   /api/v1/alerts/silences   ← {matchers|alert fingerprint, duration, comment} → Silence
DELETE /api/v1/alerts/silences/{id}
GET    /api/v1/alerts/rules                                   → [Rule]
POST   /api/v1/alerts/rules  PUT …/{name}  DELETE …/{name}    ← RuleInput
GET    /api/v1/alerts/channels                                → [Channel]
POST   /api/v1/alerts/channels  PUT …/{name}  DELETE …/{name} ← ChannelInput (secrets write-only)
POST   /api/v1/alerts/channels/{name}/test                    → {ok, message}

Alert   = {fingerprint, rule, severity, state: firing|silenced|resolved, summary, description,
           project?, app?, labels, startsAt, endsAt?, silencedUntil?, consoleURL?}
Rule    = {name, condition, threshold?, window?, for?, expr?, scope: {projects[], apps[]},
           severity, channels[], disabled, default: bool, firing: int, ready, message?, effectiveExpr}
Channel = {name, type, slack?: {channel}, email?: {to[], from, smtpHost, username?},
           ntfy?: {server, topic}, sendResolved, secretSet: bool, ready, message?, lastTest?, lastTestError?}
RuleInput    = Rule minus status fields
ChannelInput = Channel minus status fields, plus url? | password? | token? (write-only)
```

Resolved alerts come from the `ALERTS` series in VictoriaMetrics (last 24 h),
since Alertmanager forgets them.

W4 shows: Alerts (Firing / Silenced / Resolved, silence 1 h / 24 h, links to
logs and the app), Alert rules (table + editor per condition with sensible
defaults and a plain-language preview, e.g. "more than 5 restarts in 15
minutes"), Channels (add/edit, test send, secrets write-only), and on
Overview a "needs attention" feed: firing alerts plus what Kwerft already
knows (failed builds, failing schedules, Domains without certificates,
crash-looping apps from App status).

## Verification

Each worker: `make check` green (and `GOTOOLCHAIN=go1.26.0 go vet ./...`),
envtest tests for reconcilers, fakes for VictoriaMetrics, VictoriaLogs and
Alertmanager HTTP APIs, vitest for UI helpers. The coordinator merges,
deploys to the test server, and runs the exit criterion with a real Slack
incoming webhook and a deliberately crash-looping app.
