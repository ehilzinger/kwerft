// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package observability holds what the console and the reconcilers share
// about the metrics, logs and alerting stack the installer runs in
// kwerft-observability: service addresses, the labels Kwerft relies on, and
// the label every Kwerft alert carries.
package observability

const (
	// Namespace of VictoriaMetrics, VictoriaLogs, Vector, vmalert and
	// Alertmanager (install.sh stage "Observability").
	Namespace = "kwerft-observability"

	// In-cluster HTTP endpoints (Services of the installer's Helm releases).
	MetricsURL      = "http://vmsingle-vm-victoria-metrics-k8s-stack.kwerft-observability.svc:8428"
	LogsURL         = "http://vlogs-victoria-logs-single-server.kwerft-observability.svc:9428"
	AlertmanagerURL = "http://vmalertmanager-vm-victoria-metrics-k8s-stack.kwerft-observability.svc:9093"

	// LabelRule is set on every alert from an AlertRule (its name);
	// Alertmanager routes on it and the console groups by it.
	LabelRule = "kwerft_rule"
	// LabelKind is set on every alert from an AlertRule: what its condition
	// watches (app, volume, node, disk, …; internal/alerting.Kind), so the
	// console links an alert to the right page.
	LabelKind = "kwerft_kind"
	// LabelDefault marks the AlertRules Kwerft creates (value "true").
	LabelDefault = "kwerft.dev/default"

	// SMARTJob is the job label of the SMART readings: the chart's
	// smartctl_exporter DaemonSet on dedicated servers
	// (charts/kwerft/templates/disk-health.yaml), whose scrape adds the
	// label node (the pod's node) to every series.
	SMARTJob = "kwerft-disk-health"
	// NodeExporterJob is the job label of the stack's node-exporter.
	NodeExporterJob = "node-exporter"

	// SecretPrefix + channel name is a NotificationChannel's Secret in
	// Namespace. Keys: url (slack, webhook), password (email), token (ntfy).
	SecretPrefix = "notify-"
	KeyURL       = "url"
	KeyPassword  = "password"
	KeyToken     = "token"
)

// ChannelSecret is the Secret name for a NotificationChannel.
func ChannelSecret(channel string) string { return SecretPrefix + channel }
