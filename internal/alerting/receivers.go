// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package alerting

import (
	"net/url"
	"strings"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Routing: every NotificationChannel becomes one VMAlertmanagerConfig
// (kwerft-<channel> in kwerft-observability) with one receiver and one
// route matching the rules that name the channel (kwerft_rule=~"a|b").
// The operator merges it into Alertmanager's configuration as a sub-route
// of the root and, by default, adds the matcher namespace="kwerft-observability"
// to that route, which would drop every app alert (namespace=<project>) and
// every node alert (no namespace). install.sh therefore sets
// alertmanager.spec.disableNamespaceMatcher=true on the VMAlertmanager; the
// channel reconciler reports a channel as not ready while it is missing.
// The operator also forces continue: true on the route, so a rule with
// several channels reaches all of them.

// Timing of Kwerft's routes, for the 2-minute crash-loop criterion: the
// first notification of a group waits group_wait (Alertmanager's default is
// 30s), changes to a firing group wait group_interval (default 5m).
const (
	GroupWait      = "10s"
	GroupInterval  = "1m"
	RepeatInterval = "4h"
)

// GroupBy: one notification per rule and app (or project, or node set).
var GroupBy = []string{observability.LabelRule, "namespace", "app"}

// Alertmanager templates (Go text/template over Alertmanager's notification
// data) shared by the Slack receiver and the email subject.
const (
	slackTitle = `{{ if eq .Status "firing" }}[{{ .CommonLabels.severity | toUpper }}]{{ else }}[RESOLVED]{{ end }} ` +
		`{{ with .CommonAnnotations.summary }}{{ . }}{{ else }}{{ .GroupLabels.kwerft_rule }}: {{ len .Alerts }} alerts{{ end }}`
	slackTitleLink = `{{ (index .Alerts 0).Annotations.console_url }}`
	slackText      = `{{ range .Alerts }}{{ if gt (len $.Alerts) 1 }}- {{ .Annotations.summary }}: {{ end }}{{ .Annotations.description }}` +
		`{{ with .Annotations.console_url }} <{{ . }}|Open in Kwerft>{{ end }}` + "\n" + `{{ end }}`
	slackColor   = `{{ if eq .Status "firing" }}{{ if eq .CommonLabels.severity "critical" }}danger{{ else }}warning{{ end }}{{ else }}good{{ end }}`
	emailSubject = `[{{ .Status | toUpper }}] {{ with .CommonAnnotations.summary }}{{ . }}{{ else }}{{ .GroupLabels.kwerft_rule }}: {{ len .Alerts }} alerts{{ end }}`
)

// ntfy templates. Alertmanager's webhook payload is not ntfy's format, but
// ntfy (2.9 and later; ntfy.sh is current) renders a JSON body through Go
// templates given in the URL (tpl=yes, t= title, m= message), so the
// webhook receiver posts straight to the topic; no adapter in between.
// The data are the webhook JSON's own keys (lower camel case).
const (
	ntfyTitle = `{{if eq .status "firing"}}[{{.commonLabels.severity}}]{{else}}[resolved]{{end}} ` +
		`{{with .commonAnnotations.summary}}{{.}}{{else}}{{.groupLabels.kwerft_rule}}{{end}}`
	ntfyMessage = `{{range .alerts}}{{.annotations.summary}}: {{.annotations.description}}` +
		`{{with .annotations.console_url}} {{.}}{{end}}` + "\n" + `{{end}}`
	// NtfyMaxAlerts keeps the body under ntfy's message size limit.
	NtfyMaxAlerts = 10
)

// NtfyURL is the publish URL of a topic with Kwerft's templates.
func NtfyURL(server, topic string) string {
	if server == "" {
		server = "https://ntfy.sh"
	}
	q := url.Values{"tpl": {"yes"}, "t": {ntfyTitle}, "m": {ntfyMessage}}
	return strings.TrimSuffix(server, "/") + "/" + url.PathEscape(topic) + "?" + q.Encode()
}

// RequiredSecret names the Secret key a channel cannot work without, and
// how to ask for it ("" when it needs none).
func RequiredSecret(spec *kwerftv1.NotificationChannelSpec) (key, what string) {
	switch spec.Type {
	case kwerftv1.NotifySlack:
		return observability.KeyURL, "Slack webhook URL"
	case kwerftv1.NotifyWebhook:
		return observability.KeyURL, "webhook URL"
	case kwerftv1.NotifyEmail:
		if spec.Email != nil && spec.Email.Username != "" {
			return observability.KeyPassword, "SMTP password"
		}
	}
	return "", ""
}

// ChannelConfig is what a channel's VMAlertmanagerConfig is made of.
type ChannelConfig struct {
	Channel *kwerftv1.NotificationChannel
	// SecretKeys are the keys present in the channel's Secret.
	SecretKeys map[string]bool
	// SecretVersion (the Secret's resourceVersion) is part of the receiver
	// name, so new credentials change the object and the operator rebuilds
	// Alertmanager's configuration at once (it does not watch Secrets).
	SecretVersion string
	// Rules routed to the channel (enabled rules naming it), sorted.
	Rules []string
}

// AlertmanagerConfigSpec renders the VMAlertmanagerConfig spec. Without the
// required secret the receiver gets no integration, so nothing invalid
// reaches Alertmanager.
func AlertmanagerConfigSpec(c ChannelConfig) map[string]any {
	ch := c.Channel
	secret := func(key string) map[string]any {
		return map[string]any{"name": observability.ChannelSecret(ch.Name), "key": key}
	}
	sendResolved := ch.Spec.SendResolved == nil || *ch.Spec.SendResolved
	name := string(ch.Spec.Type)
	if c.SecretVersion != "" {
		name += "-v" + c.SecretVersion
	}
	receiver := map[string]any{"name": name}
	key, _ := RequiredSecret(&ch.Spec)
	if key == "" || c.SecretKeys[key] {
		switch ch.Spec.Type {
		case kwerftv1.NotifySlack:
			cfg := map[string]any{
				"api_url": secret(observability.KeyURL), "send_resolved": sendResolved,
				"title": slackTitle, "title_link": slackTitleLink, "text": slackText, "color": slackColor,
			}
			if ch.Spec.Slack != nil && ch.Spec.Slack.Channel != "" {
				cfg["channel"] = ch.Spec.Slack.Channel
			}
			receiver["slack_configs"] = []any{cfg}
		case kwerftv1.NotifyEmail:
			e := ch.Spec.Email
			cfg := map[string]any{
				"to": strings.Join(e.To, ", "), "from": e.From, "smarthost": e.SMTPHost,
				"require_tls": true, "send_resolved": sendResolved,
				"headers": map[string]any{"Subject": emailSubject},
			}
			if e.Username != "" {
				cfg["auth_username"] = e.Username
				cfg["auth_password"] = secret(observability.KeyPassword)
			}
			receiver["email_configs"] = []any{cfg}
		case kwerftv1.NotifyWebhook:
			receiver["webhook_configs"] = []any{map[string]any{"url_secret": secret(observability.KeyURL), "send_resolved": sendResolved}}
		case kwerftv1.NotifyNtfy:
			cfg := map[string]any{
				"url": NtfyURL(ch.Spec.Ntfy.Server, ch.Spec.Ntfy.Topic), "send_resolved": sendResolved, "max_alerts": NtfyMaxAlerts,
			}
			if c.SecretKeys[observability.KeyToken] {
				cfg["http_config"] = map[string]any{"authorization": map[string]any{"type": "Bearer", "credentials": secret(observability.KeyToken)}}
			}
			receiver["webhook_configs"] = []any{cfg}
		}
	}
	spec := map[string]any{"receivers": []any{receiver}}
	if len(c.Rules) > 0 {
		groupBy := make([]any, len(GroupBy))
		for i, g := range GroupBy {
			groupBy[i] = g
		}
		spec["route"] = map[string]any{
			"receiver":        name,
			"matchers":        []any{observability.LabelRule + `=~"` + strings.Join(c.Rules, "|") + `"`},
			"group_by":        groupBy,
			"group_wait":      GroupWait,
			"group_interval":  GroupInterval,
			"repeat_interval": RepeatInterval,
			"continue":        true,
		}
	}
	return spec
}
