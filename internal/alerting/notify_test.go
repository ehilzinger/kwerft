package alerting

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

type received struct {
	mu     sync.Mutex
	path   string
	query  url.Values
	auth   string
	body   map[string]any
	status int
	answer string
}

func fakeTarget(t *testing.T) (*httptest.Server, *received) {
	t.Helper()
	got := &received{status: http.StatusOK, answer: "ok"}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		defer got.mu.Unlock()
		got.path, got.query, got.auth = r.URL.Path, r.URL.Query(), r.Header.Get("Authorization")
		got.body = nil
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(got.status)
		_, _ = io.WriteString(w, got.answer)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func channelOf(spec kwerftv1.NotificationChannelSpec) *kwerftv1.NotificationChannel {
	return &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: "ops"}, Spec: spec}
}

func TestSlackTestNotification(t *testing.T) {
	srv, got := fakeTarget(t)
	n := &Notifier{HTTP: srv.Client(), Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	ch := channelOf(kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifySlack, Slack: &kwerftv1.SlackSettings{Channel: "#ops"}})
	hook := srv.URL + "/services/T0/B0/secret-token"
	if err := n.Test(context.Background(), ch, map[string][]byte{"url": []byte(hook)}, "console.example.com"); err != nil {
		t.Fatal(err)
	}
	att := got.body["attachments"].([]any)[0].(map[string]any)
	if got.path != "/services/T0/B0/secret-token" || got.body["channel"] != "#ops" ||
		!strings.HasPrefix(att["title"].(string), "[TEST]") || att["title_link"] != "https://console.example.com/monitoring/channels" {
		t.Errorf("slack got %s %v", got.path, got.body)
	}

	// Slack's answer is the error; the URL (with its token) never is.
	got.status, got.answer = http.StatusNotFound, "no_service"
	err := n.Test(context.Background(), ch, map[string][]byte{"url": []byte(hook)}, "")
	if err == nil || !strings.Contains(err.Error(), "Slack answered 404: no_service") || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("err = %v", err)
	}
	err = n.Test(context.Background(), ch, map[string][]byte{"url": []byte("https://127.0.0.1:1/services/secret-token")}, "")
	if err == nil || strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "could not reach Slack") {
		t.Errorf("unreachable: %v", err)
	}
	if err := n.Test(context.Background(), ch, nil, ""); err == nil || !strings.Contains(err.Error(), "no Slack webhook URL") {
		t.Errorf("without URL: %v", err)
	}
}

// The webhook gets Alertmanager's payload; ntfy gets it too, at the URL
// carrying Kwerft's templates, and the templates render it.
func TestWebhookAndNtfyTestNotifications(t *testing.T) {
	srv, got := fakeTarget(t)
	n := &Notifier{HTTP: srv.Client()}
	hook := channelOf(kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifyWebhook, Webhook: &kwerftv1.WebhookSettings{}})
	if err := n.Test(context.Background(), hook, map[string][]byte{"url": []byte(srv.URL + "/hook")}, "c.example.com"); err != nil {
		t.Fatal(err)
	}
	if got.body["version"] != "4" || got.body["status"] != "firing" || len(got.body["alerts"].([]any)) != 1 {
		t.Errorf("webhook got %v", got.body)
	}

	ntfy := channelOf(kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifyNtfy, Ntfy: &kwerftv1.NtfySettings{Server: srv.URL, Topic: "kwerft-alerts"}})
	if err := n.Test(context.Background(), ntfy, map[string][]byte{"token": []byte("tk_abc")}, "c.example.com"); err != nil {
		t.Fatal(err)
	}
	if got.path != "/kwerft-alerts" || got.query.Get("tpl") != "yes" || got.auth != "Bearer tk_abc" {
		t.Errorf("ntfy got %s %v auth %q", got.path, got.query, got.auth)
	}
	// ntfy renders title and message with Go templates over the JSON body.
	render := func(tmpl string) string {
		var b strings.Builder
		if err := template.Must(template.New("n").Parse(tmpl)).Execute(&b, got.body); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if title := render(got.query.Get("t")); title != "[info] Test notification from Kwerft" {
		t.Errorf("ntfy title = %q", title)
	}
	if msg := render(got.query.Get("m")); !strings.Contains(msg, "The notification channel ops works") || !strings.Contains(msg, "https://c.example.com/monitoring/channels") {
		t.Errorf("ntfy message = %q", msg)
	}
	// Without a token: no Authorization header.
	if err := n.Test(context.Background(), ntfy, nil, ""); err != nil || got.auth != "" {
		t.Errorf("ntfy without token: %v, auth %q", err, got.auth)
	}
}

// fakeSMTP is a tiny SMTP server offering STARTTLS (or not) and AUTH PLAIN.
type fakeSMTP struct {
	addr     string
	roots    *x509.CertPool
	starttls bool
	mu       sync.Mutex
	authed   string
	rcpts    []string
	data     string
}

func newFakeSMTP(t *testing.T, starttls bool) *fakeSMTP {
	t.Helper()
	certSrv := httptest.NewTLSServer(http.NotFoundHandler())
	cert := certSrv.TLS.Certificates[0]
	roots := certSrv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	certSrv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{addr: ln.Addr().String(), roots: roots, starttls: starttls}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		}
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn, cfg *tls.Config) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), conn
	say := func(s string) { _, _ = io.WriteString(w, s+"\r\n") }
	say("220 fake ESMTP")
	secure := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			if f.starttls && !secure {
				say("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 AUTH PLAIN")
			}
		case upper == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, cfg)
			if tc.Handshake() != nil {
				return
			}
			conn, secure = tc, true
			r, w = bufio.NewReader(tc), tc
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(cmd[len("AUTH PLAIN"):]))
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[2] == "right" {
				f.mu.Lock()
				f.authed = parts[1]
				f.mu.Unlock()
				say("235 ok")
			} else {
				say("535 5.7.8 bad credentials")
			}
		case strings.HasPrefix(upper, "MAIL FROM"):
			say("250 ok")
		case strings.HasPrefix(upper, "RCPT TO"):
			f.mu.Lock()
			f.rcpts = append(f.rcpts, cmd)
			f.mu.Unlock()
			say("250 ok")
		case upper == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case upper == "QUIT":
			say("221 bye")
			return
		default:
			say("502 what")
		}
	}
}

func TestEmailTestNotificationNeedsSTARTTLS(t *testing.T) {
	f := newFakeSMTP(t, true)
	n := &Notifier{TLS: &tls.Config{RootCAs: f.roots}}
	ch := channelOf(kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifyEmail, Email: &kwerftv1.EmailSettings{
		To: []string{"ops@example.com"}, From: "kwerft@example.com", SMTPHost: f.addr, Username: "kwerft"}})
	if err := n.Test(context.Background(), ch, map[string][]byte{"password": []byte("right")}, "c.example.com"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.authed != "kwerft" || len(f.rcpts) != 1 || !strings.Contains(f.data, "Subject: [TEST] Test notification from Kwerft") ||
		!strings.Contains(f.data, "https://c.example.com/monitoring/channels") {
		t.Errorf("smtp got auth %q rcpts %v data %q", f.authed, f.rcpts, f.data)
	}
	f.mu.Unlock()

	err := n.Test(context.Background(), ch, map[string][]byte{"password": []byte("wrong")}, "")
	if err == nil || !strings.Contains(err.Error(), "rejected the username or password") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("wrong password: %v", err)
	}
	if err := n.Test(context.Background(), ch, nil, ""); err == nil || !strings.Contains(err.Error(), "no SMTP password") {
		t.Errorf("no password: %v", err)
	}

	plain := newFakeSMTP(t, false)
	ch.Spec.Email.SMTPHost = plain.addr
	if err := n.Test(context.Background(), ch, map[string][]byte{"password": []byte("right")}, ""); err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Errorf("without STARTTLS: %v", err)
	}
}

func TestAlertmanagerConfigSpecPerType(t *testing.T) {
	ch := channelOf(kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifySlack, Slack: &kwerftv1.SlackSettings{}})
	spec := AlertmanagerConfigSpec(ChannelConfig{Channel: ch, SecretKeys: map[string]bool{"url": true}, SecretVersion: "42", Rules: []string{"a", "b"}})
	recv := spec["receivers"].([]any)[0].(map[string]any)
	route := spec["route"].(map[string]any)
	if recv["name"] != "slack-v42" || route["receiver"] != "slack-v42" || route["matchers"].([]any)[0] != `kwerft_rule=~"a|b"` || route["continue"] != true {
		t.Errorf("spec = %v", spec)
	}
	if _, ok := recv["slack_configs"].([]any)[0].(map[string]any)["channel"]; ok {
		t.Error("an empty Slack channel is set")
	}
	// Without the URL: no integration, nothing invalid for Alertmanager.
	spec = AlertmanagerConfigSpec(ChannelConfig{Channel: ch, Rules: []string{"a"}})
	if recv := spec["receivers"].([]any)[0].(map[string]any); len(recv) != 1 {
		t.Errorf("receiver without URL = %v", recv)
	}
	// Alertmanager's Slack templates parse with its template functions.
	for _, tmpl := range []string{slackTitle, slackTitleLink, slackText, slackColor, emailSubject} {
		if _, err := template.New("t").Funcs(template.FuncMap{"toUpper": strings.ToUpper}).Parse(tmpl); err != nil {
			t.Errorf("%q: %v", tmpl, err)
		}
	}
}
