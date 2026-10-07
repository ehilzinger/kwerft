// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package alerting

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Notifier sends a channel's test notification straight to its destination,
// the way Alertmanager would (same payloads, same TLS rules), and reports
// what the destination answered. Going through Alertmanager instead would
// only tell that the alert was accepted, not whether Slack took it.
type Notifier struct {
	HTTP *http.Client
	// Dial opens SMTP connections; nil means net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLS is the base TLS configuration for SMTP (tests add a root CA).
	TLS *tls.Config
	Now func() time.Time
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// TestAlert is the synthetic alert a test notification carries.
func TestAlert(channel, consoleHost string, now time.Time) AMAlert {
	a := AMAlert{
		Labels: map[string]string{"alertname": "KwerftTest", observability.LabelRule: "kwerft-test", "severity": "info", "channel": channel},
		Annotations: map[string]string{
			"summary":     "Test notification from Kwerft",
			"description": "The notification channel " + channel + " works. Alerts arrive here like this one.",
		},
		StartsAt: now,
	}
	if consoleHost != "" {
		a.Annotations["console_url"] = "https://" + consoleHost + "/monitoring/channels"
	}
	a.Fingerprint = Fingerprint(a.Labels)
	return a
}

// Test sends a test notification through ch with the Secret's data.
func (n *Notifier) Test(ctx context.Context, ch *kwerftv1.NotificationChannel, secret map[string][]byte, consoleHost string) error {
	if key, what := RequiredSecret(&ch.Spec); key != "" && len(secret[key]) == 0 {
		return fmt.Errorf("no %s is stored", what)
	}
	alert := TestAlert(ch.Name, consoleHost, n.now())
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	switch ch.Spec.Type {
	case kwerftv1.NotifySlack:
		return n.slack(ctx, ch, string(secret[observability.KeyURL]), alert)
	case kwerftv1.NotifyWebhook:
		return n.post(ctx, string(secret[observability.KeyURL]), webhookPayload(ch.Name, alert, consoleHost), "", "the webhook")
	case kwerftv1.NotifyNtfy:
		return n.post(ctx, NtfyURL(ch.Spec.Ntfy.Server, ch.Spec.Ntfy.Topic), webhookPayload(ch.Name, alert, consoleHost),
			string(secret[observability.KeyToken]), "ntfy")
	case kwerftv1.NotifyEmail:
		return n.email(ctx, ch.Spec.Email, string(secret[observability.KeyPassword]), alert)
	}
	return fmt.Errorf("unknown channel type %q", ch.Spec.Type)
}

// webhookPayload is Alertmanager's webhook body (version 4) for one alert.
func webhookPayload(receiver string, a AMAlert, consoleHost string) map[string]any {
	ext := ""
	if consoleHost != "" {
		ext = "https://" + consoleHost + "/monitoring"
	}
	return map[string]any{
		"version": "4", "groupKey": `{}:{kwerft_rule="kwerft-test"}`, "truncatedAlerts": 0,
		"status": "firing", "receiver": receiver,
		"groupLabels":       map[string]string{observability.LabelRule: a.Labels[observability.LabelRule]},
		"commonLabels":      a.Labels,
		"commonAnnotations": a.Annotations,
		"externalURL":       ext,
		"alerts": []map[string]any{{
			"status": "firing", "labels": a.Labels, "annotations": a.Annotations,
			"startsAt": a.StartsAt.UTC().Format(time.RFC3339), "endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": ext, "fingerprint": a.Fingerprint,
		}},
	}
}

func (n *Notifier) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// post sends JSON and turns the answer into an error a person can act on.
// The URL is never part of an error: it often carries a token.
func (n *Notifier) post(ctx context.Context, rawURL string, body any, bearer, what string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("the URL of %s is not valid", what)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := n.client().Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %s", what, describeNetErr(err))
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode >= 300 {
		msg := strings.TrimSpace(string(answer))
		if msg == "" {
			msg = http.StatusText(res.StatusCode)
		}
		return fmt.Errorf("%s answered %d: %s", what, res.StatusCode, truncate(oneLine(msg), 200))
	}
	return nil
}

func (n *Notifier) slack(ctx context.Context, ch *kwerftv1.NotificationChannel, hook string, a AMAlert) error {
	text := a.Annotations["description"]
	if u := a.Annotations["console_url"]; u != "" {
		text += " <" + u + "|Open in Kwerft>"
	}
	msg := map[string]any{"attachments": []map[string]any{{
		"title": "[TEST] " + a.Annotations["summary"], "title_link": a.Annotations["console_url"],
		"text": text, "color": "good", "fallback": "[TEST] " + a.Annotations["summary"],
	}}}
	if ch.Spec.Slack != nil && ch.Spec.Slack.Channel != "" {
		msg["channel"] = ch.Spec.Slack.Channel
	}
	return n.post(ctx, hook, msg, "", "Slack")
}

// email sends over SMTP with STARTTLS required (Alertmanager's require_tls),
// or implicit TLS on port 465, and AUTH when a username is set.
func (n *Notifier) email(ctx context.Context, e *kwerftv1.EmailSettings, password string, a AMAlert) error {
	host, port, err := net.SplitHostPort(e.SMTPHost)
	if err != nil {
		return fmt.Errorf("the SMTP server must be host:port")
	}
	dial := n.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	conn, err := dial(ctx, "tcp", e.SMTPHost)
	if err != nil {
		return fmt.Errorf("could not reach %s: %s", e.SMTPHost, describeNetErr(err))
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if n.TLS != nil {
		tlsCfg = n.TLS.Clone()
		tlsCfg.ServerName = host
	}
	if port == "465" {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s did not greet like an SMTP server: %v", e.SMTPHost, err)
	}
	defer c.Close()
	if err := c.Hello("kwerft"); err != nil {
		return fmt.Errorf("SMTP HELO: %v", err)
	}
	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS; Kwerft only sends mail encrypted", e.SMTPHost)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS with %s failed: %v", e.SMTPHost, err)
		}
	}
	if e.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.Username, password, host)); err != nil {
			return fmt.Errorf("%s rejected the username or password: %v", e.SMTPHost, err)
		}
	}
	if err := c.Mail(e.From); err != nil {
		return fmt.Errorf("%s refused the sender %s: %v", e.SMTPHost, e.From, err)
	}
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("%s refused the recipient %s: %v", e.SMTPHost, to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %v", err)
	}
	body := a.Annotations["description"]
	if u := a.Annotations["console_url"]; u != "" {
		body += "\r\n\r\n" + u
	}
	msg := "From: " + e.From + "\r\nTo: " + strings.Join(e.To, ", ") + "\r\nSubject: [TEST] " + a.Annotations["summary"] +
		"\r\nDate: " + n.now().UTC().Format(time.RFC1123Z) + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
	if _, err := io.WriteString(w, msg); err != nil {
		return fmt.Errorf("SMTP DATA: %v", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%s did not accept the message: %v", e.SMTPHost, err)
	}
	return c.Quit()
}

// describeNetErr keeps the cause of a network error but not the URL (which
// may carry a token).
func describeNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	if msg := err.Error(); !strings.Contains(msg, "://") {
		return msg
	}
	return "connection failed"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
