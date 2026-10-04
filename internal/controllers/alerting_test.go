package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/observability"
)

const testAlertmanager = "vm-victoria-metrics-k8s-stack"

// setupObservability creates kwerft-observability and the stack's
// VMAlertmanager as install.sh leaves them.
func setupObservability(ctx context.Context) error {
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: observability.Namespace}}); err != nil {
		return err
	}
	am := &unstructured.Unstructured{}
	am.SetGroupVersionKind(VMAlertmanagerGVK)
	am.SetNamespace(observability.Namespace)
	am.SetName(testAlertmanager)
	am.Object["spec"] = map[string]any{"disableNamespaceMatcher": true, "selectAllByDefault": true}
	return k8s.Create(ctx, am)
}

func vmRule(t *testing.T) map[string]map[string]any {
	t.Helper()
	var u unstructured.Unstructured
	u.SetGroupVersionKind(VMRuleGVK)
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: observability.Namespace, Name: AlertsVMRule}, &u); err != nil {
		t.Fatalf("get VMRule: %v", err)
	}
	groups, _, _ := unstructured.NestedSlice(u.Object, "spec", "groups")
	out := map[string]map[string]any{}
	for _, g := range groups {
		m := g.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

func alertRule(t *testing.T, name string, spec kwerftv1.AlertRuleSpec) *kwerftv1.AlertRule {
	t.Helper()
	r := &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(context.Background(), &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return r
}

// ruleReady waits for the rule's Ready condition at its current generation.
func ruleReady(t *testing.T, name string, check func(*kwerftv1.AlertRule, *metav1.Condition) error) *kwerftv1.AlertRule {
	t.Helper()
	var r kwerftv1.AlertRule
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &r); err != nil {
			return err
		}
		c := meta.FindStatusCondition(r.Status.Conditions, ConditionReady)
		if c == nil || c.ObservedGeneration != r.Generation {
			return fmt.Errorf("no current Ready condition")
		}
		return check(&r, c)
	})
	return &r
}

func withReason(reason string) func(*kwerftv1.AlertRule, *metav1.Condition) error {
	return func(_ *kwerftv1.AlertRule, c *metav1.Condition) error {
		if c.Reason != reason {
			return fmt.Errorf("reason %s (%s), want %s", c.Reason, c.Message, reason)
		}
		return nil
	}
}

func TestDefaultAlertRulesAppearStayEditedAndComeBack(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	for _, d := range alerting.DefaultRules() {
		r := ruleReady(t, d.Name, func(*kwerftv1.AlertRule, *metav1.Condition) error { return nil })
		if r.Labels[observability.LabelDefault] != "true" || r.Spec.Condition != d.Spec.Condition || len(r.Spec.Channels) != 0 {
			t.Errorf("default %s = %+v", d.Name, r)
		}
	}
	if g := vmRule(t)["crash-looping"]; g == nil || g["interval"] != "10s" {
		t.Fatalf("crash-looping group = %v", g)
	}

	// An edit stays.
	var r kwerftv1.AlertRule
	if err := k8s.Get(ctx, client.ObjectKey{Name: "restarts"}, &r); err != nil {
		t.Fatal(err)
	}
	three := int64(3)
	r.Spec.Threshold = &three
	if err := k8s.Update(ctx, &r); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		g := vmRule(t)["restarts"]
		if g == nil || !strings.HasSuffix(g["rules"].([]any)[0].(map[string]any)["expr"].(string), "> 3") {
			return fmt.Errorf("restarts group = %v", g)
		}
		return nil
	})
	time.Sleep(300 * time.Millisecond)
	if err := k8s.Get(ctx, client.ObjectKey{Name: "restarts"}, &r); err != nil || r.Spec.Threshold == nil || *r.Spec.Threshold != 3 {
		t.Fatalf("the edit was overwritten: %+v %v", r.Spec, err)
	}

	// Deleting one restores its defaults.
	if err := k8s.Delete(ctx, &r); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var again kwerftv1.AlertRule
		if err := k8s.Get(ctx, client.ObjectKey{Name: "restarts"}, &again); err != nil {
			return err
		}
		if again.UID == r.UID || again.Spec.Threshold != nil {
			return fmt.Errorf("not restored yet: %+v", again.Spec)
		}
		return nil
	})
}

func TestAlertRulesRenderIntoOneVMRule(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	alertRule(t, "shop-errors", kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertHTTPErrorRate, Severity: "critical",
		Scope: kwerftv1.AlertScope{Projects: []string{"shop"}}})
	alertRule(t, "shop-quiet", kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBuildFailing, Disabled: true})
	// Past the console (the CRD only knows that expr is set): reported, left
	// out, and the other rules keep working.
	alertRule(t, "broken", kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCustom, Expr: "sum(rate(x[5m]"})
	alertRule(t, "paged", kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCrashLooping, Channels: []string{"nobody"}})

	r := ruleReady(t, "shop-errors", withReason("Evaluated"))
	wantExpr := alerting.Expr(&r.Spec)
	if r.Status.Expr != wantExpr {
		t.Errorf("status.expr = %q", r.Status.Expr)
	}
	ruleReady(t, "shop-quiet", withReason("Disabled"))
	broken := ruleReady(t, "broken", withReason("InvalidExpression"))
	if c := meta.FindStatusCondition(broken.Status.Conditions, ConditionReady); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "MetricsQL") {
		t.Errorf("broken = %+v", c)
	}
	paged := ruleReady(t, "paged", withReason("ChannelMissing"))
	if c := meta.FindStatusCondition(paged.Status.Conditions, ConditionReady); !strings.Contains(c.Message, "nobody") {
		t.Errorf("paged = %+v", c)
	}

	groups := vmRule(t)
	g := groups["shop-errors"]
	if g == nil {
		t.Fatalf("groups = %v", groups)
	}
	rule := g["rules"].([]any)[0].(map[string]any)
	labels := rule["labels"].(map[string]any)
	ann := rule["annotations"].(map[string]any)
	if rule["alert"] != "shop-errors" || rule["expr"] != wantExpr || rule["for"] != "5m" ||
		labels["kwerft_rule"] != "shop-errors" || labels["severity"] != "critical" ||
		!strings.HasPrefix(ann["console_url"].(string), `{{ if $labels.app }}https://`+testConsoleDomain+`/apps/`) || ann["summary"] == "" {
		t.Errorf("rule = %v", rule)
	}
	if _, ok := groups["shop-quiet"]; ok {
		t.Error("a disabled rule is evaluated")
	}
	if _, ok := groups["broken"]; ok {
		t.Error("an invalid rule reached vmalert")
	}

	// The operator rejects the VMRule: every evaluated rule says so.
	var u unstructured.Unstructured
	u.SetGroupVersionKind(VMRuleGVK)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: AlertsVMRule}, &u); err != nil {
		t.Fatal(err)
	}
	u.Object["status"] = map[string]any{"updateStatus": "failed", "reason": "bad template", "observedGeneration": u.GetGeneration()}
	if err := k8s.Status().Update(ctx, &u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var u unstructured.Unstructured
		u.SetGroupVersionKind(VMRuleGVK)
		if k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: AlertsVMRule}, &u) == nil {
			u.Object["status"] = map[string]any{}
			_ = k8s.Status().Update(ctx, &u)
		}
	})
	// A rule change starts a pass (the reconciler does not watch VMRules).
	touch := &kwerftv1.AlertRule{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: "shop-quiet"}, touch); err != nil {
		t.Fatal(err)
	}
	touch.Labels = map[string]string{"touched": "yes"}
	if err := k8s.Update(ctx, touch); err != nil {
		t.Fatal(err)
	}
	r = ruleReady(t, "shop-errors", withReason("RejectedByVMAlert"))
	if c := meta.FindStatusCondition(r.Status.Conditions, ConditionReady); !strings.Contains(c.Message, "bad template") {
		t.Errorf("rejected = %+v", c)
	}
	ruleReady(t, "shop-quiet", withReason("Disabled"))
}

// ---- channels -------------------------------------------------------------------------

func channel(t *testing.T, name string, spec kwerftv1.NotificationChannelSpec) *kwerftv1.NotificationChannel {
	t.Helper()
	ch := &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(context.Background(), &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return ch
}

func channelReady(t *testing.T, name, reason string) *kwerftv1.NotificationChannel {
	t.Helper()
	var ch kwerftv1.NotificationChannel
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &ch); err != nil {
			return err
		}
		got, err := readyReason(ch.Status.Conditions, ch.Generation)
		if err != nil {
			return err
		}
		if got != reason {
			c := meta.FindStatusCondition(ch.Status.Conditions, ConditionReady)
			return fmt.Errorf("reason %s (%s), want %s", got, c.Message, reason)
		}
		return nil
	})
	return &ch
}

// storeSecret writes channel credentials as the console would, and marks
// the channel so the reconciler looks again.
func storeSecret(t *testing.T, name string, data map[string]string) {
	t.Helper()
	ctx := context.Background()
	var sec corev1.Secret
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: observability.ChannelSecret(name)}, &sec)
	})
	sec.StringData = data
	if err := k8s.Update(ctx, &sec); err != nil {
		t.Fatal(err)
	}
	touchChannel(t, name)
}

func touchChannel(t *testing.T, name string) {
	t.Helper()
	var ch kwerftv1.NotificationChannel
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &ch); err != nil {
		t.Fatal(err)
	}
	if ch.Annotations == nil {
		ch.Annotations = map[string]string{}
	}
	ch.Annotations[AnnotationCredentialsUpdated] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := k8s.Update(context.Background(), &ch); err != nil {
		t.Fatal(err)
	}
}

func channelConfig(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	var u unstructured.Unstructured
	u.SetGroupVersionKind(VMAlertmanagerConfigGVK)
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: observability.Namespace, Name: ChannelConfigPrefix + name}, &u); err != nil {
		t.Fatalf("get config: %v", err)
	}
	return &u
}

func setNamespaceMatcherDisabled(t *testing.T, disabled bool) {
	t.Helper()
	var am unstructured.Unstructured
	am.SetGroupVersionKind(VMAlertmanagerGVK)
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: observability.Namespace, Name: testAlertmanager}, &am); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(am.Object, disabled, "spec", "disableNamespaceMatcher")
	if err := k8s.Update(context.Background(), &am); err != nil {
		t.Fatal(err)
	}
}

func TestSlackChannelRoutesItsRules(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ch := channel(t, "ops", kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifySlack, Slack: &kwerftv1.SlackSettings{Channel: "#ops"}})

	// The Secret appears, empty, owned by the channel; owners and admins may
	// patch exactly it.
	var sec corev1.Secret
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: "notify-ops"}, &sec)
	})
	if !metav1.IsControlledBy(&sec, ch) || len(sec.Data) != 0 {
		t.Errorf("secret = %+v", sec.ObjectMeta)
	}
	eventually(t, func() error {
		var role rbacv1.Role
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: NotifySecretsRole}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 1 || !slices.Contains(role.Rules[0].ResourceNames, "notify-ops") || !slices.Equal(role.Rules[0].Verbs, []string{"patch"}) {
			return fmt.Errorf("role = %+v", role.Rules)
		}
		return nil
	})
	var binding rbacv1.RoleBinding
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: NotifySecretsRole}, &binding); err != nil || len(binding.Subjects) != 2 {
		t.Errorf("binding = %+v, %v", binding.Subjects, err)
	}
	channelReady(t, "ops", "WaitingForCredentials")

	// Without credentials for a while: missing.
	channelClock.Advance(2 * time.Minute)
	t.Cleanup(channelClock.Reset)
	touchChannel(t, "ops")
	got := channelReady(t, "ops", "SecretMissing")
	if got.Status.SecretSet {
		t.Error("secretSet without a secret")
	}
	receivers, _, _ := unstructured.NestedSlice(channelConfig(t, "ops").Object, "spec", "receivers")
	if len(receivers) != 1 || receivers[0].(map[string]any)["slack_configs"] != nil {
		t.Errorf("a receiver without its URL: %v", receivers)
	}

	storeSecret(t, "ops", map[string]string{observability.KeyURL: "https://hooks.slack.invalid/services/T0/B0/x"})
	got = channelReady(t, "ops", "Routing")
	if !got.Status.SecretSet || !strings.Contains(meta.FindStatusCondition(got.Status.Conditions, ConditionReady).Message, "No rule") {
		t.Errorf("status = %+v", got.Status)
	}
	cfg := channelConfig(t, "ops")
	if !metav1.IsControlledBy(cfg, ch) {
		t.Errorf("config owners = %v", cfg.GetOwnerReferences())
	}
	receivers, _, _ = unstructured.NestedSlice(cfg.Object, "spec", "receivers")
	slack := receivers[0].(map[string]any)["slack_configs"].([]any)[0].(map[string]any)
	if slack["channel"] != "#ops" || slack["send_resolved"] != true ||
		slack["api_url"].(map[string]any)["name"] != "notify-ops" || slack["api_url"].(map[string]any)["key"] != "url" ||
		!strings.Contains(slack["text"].(string), "console_url") {
		t.Errorf("slack config = %v", slack)
	}
	if _, found, _ := unstructured.NestedMap(cfg.Object, "spec", "route"); found {
		t.Error("a route without rules")
	}

	// A rule naming the channel is routed to it; disabling it removes the route.
	alertRule(t, "ops-crash", kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCrashLooping, Channels: []string{"ops"}})
	ruleReady(t, "ops-crash", withReason("Evaluated"))
	eventually(t, func() error {
		route, _, _ := unstructured.NestedMap(channelConfig(t, "ops").Object, "spec", "route")
		if route == nil {
			return fmt.Errorf("no route")
		}
		m := route["matchers"].([]any)
		if len(m) != 1 || m[0] != `kwerft_rule=~"ops-crash"` || route["group_wait"] != alerting.GroupWait ||
			route["receiver"] != receivers[0].(map[string]any)["name"] {
			return fmt.Errorf("route = %v", route)
		}
		return nil
	})
	got = channelReady(t, "ops", "Routing")
	if !strings.Contains(meta.FindStatusCondition(got.Status.Conditions, ConditionReady).Message, "ops-crash") {
		t.Errorf("message = %+v", got.Status.Conditions)
	}
	var rule kwerftv1.AlertRule
	if err := k8s.Get(ctx, client.ObjectKey{Name: "ops-crash"}, &rule); err != nil {
		t.Fatal(err)
	}
	rule.Spec.Disabled = true
	if err := k8s.Update(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if _, found, _ := unstructured.NestedMap(channelConfig(t, "ops").Object, "spec", "route"); found {
			return fmt.Errorf("route of a disabled rule")
		}
		return nil
	})

	// The operator rejecting the config, and Alertmanager's namespace matcher.
	cfg = channelConfig(t, "ops")
	cfg.Object["status"] = map[string]any{"updateStatus": "operational", "observedGeneration": cfg.GetGeneration(),
		"reason": "unknown receiver field", "conditions": []any{}}
	if err := k8s.Status().Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	touchChannel(t, "ops")
	channelReady(t, "ops", "RejectedByAlertmanager")
	cfg = channelConfig(t, "ops")
	cfg.Object["status"] = map[string]any{"updateStatus": "operational", "observedGeneration": cfg.GetGeneration()}
	if err := k8s.Status().Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	setNamespaceMatcherDisabled(t, false)
	t.Cleanup(func() { setNamespaceMatcherDisabled(t, true) })
	touchChannel(t, "ops")
	got = channelReady(t, "ops", "NamespaceMatcher")
	if !strings.Contains(meta.FindStatusCondition(got.Status.Conditions, ConditionReady).Message, "installer") {
		t.Errorf("message = %+v", got.Status.Conditions)
	}
	setNamespaceMatcherDisabled(t, true)
	touchChannel(t, "ops")
	channelReady(t, "ops", "Routing")

	// Deleting the channel removes its Secret, config and Role entry.
	if err := k8s.Delete(ctx, &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: "ops"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: "notify-ops"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("secret still there: %v", err)
		}
		var u unstructured.Unstructured
		u.SetGroupVersionKind(VMAlertmanagerConfigGVK)
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: "kwerft-ops"}, &u); !apierrors.IsNotFound(err) {
			return fmt.Errorf("config still there: %v", err)
		}
		var role rbacv1.Role
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: NotifySecretsRole}, &role); err != nil {
			return err
		}
		for _, r := range role.Rules {
			if slices.Contains(r.ResourceNames, "notify-ops") {
				return fmt.Errorf("role still lists notify-ops")
			}
		}
		return nil
	})
}

// Email, webhook and ntfy configs are accepted by the operator's CRD and
// reference the Secret's keys.
func TestEmailWebhookAndNtfyChannels(t *testing.T) {
	requireEnvtest(t)
	f := false
	channel(t, "mail", kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifyEmail, SendResolved: &f, Email: &kwerftv1.EmailSettings{
		To: []string{"ops@example.com", "dev@example.com"}, From: "kwerft@example.com", SMTPHost: "smtp.example.com:587", Username: "kwerft"}})
	channel(t, "hook", kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifyWebhook, Webhook: &kwerftv1.WebhookSettings{}})
	channel(t, "phone", kwerftv1.NotificationChannelSpec{Type: kwerftv1.NotifyNtfy, Ntfy: &kwerftv1.NtfySettings{Server: "https://ntfy.example.com", Topic: "kwerft-alerts"}})
	alertRule(t, "all-builds", kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBuildFailing, Channels: []string{"mail", "hook", "phone"}})

	// ntfy needs no secret; the others wait for theirs.
	if ch := channelReady(t, "phone", "Routing"); ch.Status.SecretSet {
		t.Error("ntfy without a token reports a secret")
	}
	channelReady(t, "mail", "WaitingForCredentials")
	storeSecret(t, "mail", map[string]string{observability.KeyPassword: "s3cret"})
	storeSecret(t, "hook", map[string]string{observability.KeyURL: "https://hooks.example.com/kwerft?token=x"})
	storeSecret(t, "phone", map[string]string{observability.KeyToken: "tk_test"})
	for _, n := range []string{"mail", "hook", "phone"} {
		channelReady(t, n, "Routing")
		eventually(t, func() error {
			var ch kwerftv1.NotificationChannel
			if err := k8s.Get(context.Background(), client.ObjectKey{Name: n}, &ch); err != nil {
				return err
			}
			if !ch.Status.SecretSet {
				return fmt.Errorf("%s: secretSet false", n)
			}
			return nil
		})
	}

	receiver := func(name string) map[string]any {
		r, _, _ := unstructured.NestedSlice(channelConfig(t, name).Object, "spec", "receivers")
		return r[0].(map[string]any)
	}
	email := receiver("mail")["email_configs"].([]any)[0].(map[string]any)
	if email["to"] != "ops@example.com, dev@example.com" || email["smarthost"] != "smtp.example.com:587" || email["require_tls"] != true ||
		email["auth_username"] != "kwerft" || email["auth_password"].(map[string]any)["key"] != "password" || email["send_resolved"] != false {
		t.Errorf("email = %v", email)
	}
	hook := receiver("hook")["webhook_configs"].([]any)[0].(map[string]any)
	if hook["url_secret"].(map[string]any)["name"] != "notify-hook" || hook["url"] != nil {
		t.Errorf("webhook = %v", hook)
	}
	ntfy := receiver("phone")["webhook_configs"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(ntfy["url"].(string), "https://ntfy.example.com/kwerft-alerts?") || !strings.Contains(ntfy["url"].(string), "tpl=yes") ||
		ntfy["http_config"].(map[string]any)["authorization"].(map[string]any)["credentials"].(map[string]any)["key"] != "token" {
		t.Errorf("ntfy = %v", ntfy)
	}
	// One rule, three channels: each route matches it.
	for _, n := range []string{"mail", "hook", "phone"} {
		route, _, _ := unstructured.NestedMap(channelConfig(t, n).Object, "spec", "route")
		if route == nil || route["matchers"].([]any)[0] != `kwerft_rule=~"all-builds"` {
			t.Errorf("%s route = %v", n, route)
		}
	}
}
