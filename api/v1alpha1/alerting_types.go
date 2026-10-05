package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AlertCondition is a condition Kwerft knows how to measure and explain.
// +kubebuilder:validation:Enum=CrashLooping;Restarts;MemoryHigh;CPUHigh;VolumeFillingUp;NodeMemoryPressure;NodeDiskPressure;CertificateExpiring;ScheduleFailing;BuildFailing;HTTPErrorRate;HTTPLatency;BackupFailing;BackupMissing;UpgradeFailed;Custom
type AlertCondition string

const (
	// CrashLooping: a container is in CrashLoopBackOff.
	AlertCrashLooping AlertCondition = "CrashLooping"
	// Restarts: more than threshold restarts within window.
	AlertRestarts AlertCondition = "Restarts"
	// MemoryHigh / CPUHigh: usage above threshold percent of the limit.
	AlertMemoryHigh AlertCondition = "MemoryHigh"
	AlertCPUHigh    AlertCondition = "CPUHigh"
	// VolumeFillingUp: used above threshold percent, or full within window
	// at the current rate.
	AlertVolumeFillingUp AlertCondition = "VolumeFillingUp"
	// NodeMemoryPressure / NodeDiskPressure: available below threshold percent.
	AlertNodeMemoryPressure AlertCondition = "NodeMemoryPressure"
	AlertNodeDiskPressure   AlertCondition = "NodeDiskPressure"
	// CertificateExpiring: a certificate expires within window.
	AlertCertificateExpiring AlertCondition = "CertificateExpiring"
	// ScheduleFailing: a Schedule's last run failed, or it has not succeeded
	// within window.
	AlertScheduleFailing AlertCondition = "ScheduleFailing"
	// BuildFailing: an App's latest build failed.
	AlertBuildFailing AlertCondition = "BuildFailing"
	// HTTPErrorRate: 5xx responses above threshold percent over window.
	AlertHTTPErrorRate AlertCondition = "HTTPErrorRate"
	// HTTPLatency: p95 latency above threshold milliseconds over window.
	AlertHTTPLatency AlertCondition = "HTTPLatency"
	// BackupFailing: a BackupPlan's latest backup failed (Failed or
	// PartiallyFailed) and no backup of it succeeded since.
	AlertBackupFailing AlertCondition = "BackupFailing"
	// BackupMissing: a BackupPlan that is not paused has had no successful
	// backup within twice its interval (or window, when set).
	AlertBackupMissing AlertCondition = "BackupMissing"
	// UpgradeFailed: the newest finished Upgrade of a component (Kwerft or
	// Kubernetes) failed or was rolled back, within window (default a day).
	AlertUpgradeFailed AlertCondition = "UpgradeFailed"
	// Custom: spec.expr, a MetricsQL expression; owners and admins only.
	AlertCustom AlertCondition = "Custom"
)

// AlertSeverity orders alerts and picks their color.
// +kubebuilder:validation:Enum=critical;warning;info
type AlertSeverity string

// AlertScope narrows what a rule watches. Empty: everything the condition
// applies to (all projects, all nodes, all certificates).
type AlertScope struct {
	// Projects (namespaces) to watch.
	// +optional
	Projects []string `json:"projects,omitempty"`
	// Apps to watch, as "<project>/<app>".
	// +optional
	Apps []string `json:"apps,omitempty"`
}

// AlertRuleSpec is one alert: a condition, where it applies, how bad it is
// and who hears about it.
//
// +kubebuilder:validation:XValidation:rule="self.condition != 'Custom' || (has(self.expr) && size(self.expr) > 0)",message="a Custom rule needs expr"
// +kubebuilder:validation:XValidation:rule="self.condition == 'Custom' || !has(self.expr)",message="expr is only for Custom rules"
type AlertRuleSpec struct {
	Condition AlertCondition `json:"condition"`

	// Threshold of the condition: a count (Restarts), percent (MemoryHigh,
	// CPUHigh, VolumeFillingUp, Node*, HTTPErrorRate) or milliseconds
	// (HTTPLatency). Empty: the condition's default.
	// +optional
	Threshold *int64 `json:"threshold,omitempty"`

	// Window the condition looks at (Restarts, VolumeFillingUp prediction,
	// CertificateExpiring, ScheduleFailing, HTTP*). Empty: the default.
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`

	// For is how long the condition must hold before the alert fires.
	// +optional
	For *metav1.Duration `json:"for,omitempty"`

	// Expr is a MetricsQL expression for Custom rules; each result series is
	// one alert.
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Expr string `json:"expr,omitempty"`

	// +optional
	Scope AlertScope `json:"scope,omitempty"`

	// +kubebuilder:default=warning
	Severity AlertSeverity `json:"severity,omitempty"`

	// Channels are NotificationChannel names. Empty: the alert only shows in
	// the console.
	// +optional
	Channels []string `json:"channels,omitempty"`

	// Disabled rules are kept but not evaluated.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
}

// AlertRuleStatus is written by the alerting reconciler.
type AlertRuleStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Expr is the expression Kwerft evaluates for this rule, for reference.
	// +optional
	Expr string `json:"expr,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AlertRule is an alert Kwerft evaluates (vmalert) and routes to
// notification channels (Alertmanager). Cluster-scoped: owners and admins
// manage rules for every project. Kwerft creates a default set labelled
// kwerft.dev/default=true, which can be changed or disabled but comes back
// if deleted.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=alert
// +kubebuilder:printcolumn:name="Condition",type=string,JSONPath=`.spec.condition`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.spec.severity`
// +kubebuilder:printcolumn:name="Disabled",type=boolean,JSONPath=`.spec.disabled`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type AlertRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AlertRuleSpec   `json:"spec,omitempty"`
	Status AlertRuleStatus `json:"status,omitempty"`
}

// AlertRuleList contains a list of AlertRules.
//
// +kubebuilder:object:root=true
type AlertRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AlertRule `json:"items"`
}

// NotificationType is how a channel delivers.
// +kubebuilder:validation:Enum=slack;email;webhook;ntfy
type NotificationType string

const (
	NotifySlack   NotificationType = "slack"
	NotifyEmail   NotificationType = "email"
	NotifyWebhook NotificationType = "webhook"
	NotifyNtfy    NotificationType = "ntfy"
)

// SlackSettings: the incoming-webhook URL is secret (key "url").
type SlackSettings struct {
	// Channel to post to, e.g. #ops-alerts; empty uses the webhook's default.
	// +optional
	Channel string `json:"channel,omitempty"`
}

// EmailSettings: SMTP. The password is secret (key "password").
type EmailSettings struct {
	// +kubebuilder:validation:MinItems=1
	To []string `json:"to"`
	// +kubebuilder:validation:MinLength=1
	From string `json:"from"`
	// SMTPHost is host:port, e.g. smtp.example.com:587 (STARTTLS required).
	// +kubebuilder:validation:MinLength=1
	SMTPHost string `json:"smtpHost"`
	// +optional
	Username string `json:"username,omitempty"`
}

// WebhookSettings: the URL is secret (key "url"), as it often carries a token.
type WebhookSettings struct{}

// NtfySettings: an optional access token is secret (key "token").
type NtfySettings struct {
	// +kubebuilder:default="https://ntfy.sh"
	// +kubebuilder:validation:Pattern=`^https://`
	Server string `json:"server,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Topic string `json:"topic"`
}

// NotificationChannelSpec is one place alerts go. Secrets (webhook URLs,
// passwords, tokens) live in the Secret "notify-<name>" in
// kwerft-observability, written by owners and admins through the console and
// read back by nobody.
//
// +kubebuilder:validation:XValidation:rule="(self.type == 'slack') == has(self.slack) && (self.type == 'email') == has(self.email) && (self.type == 'webhook') == has(self.webhook) && (self.type == 'ntfy') == has(self.ntfy)",message="set exactly the settings of the channel's type"
type NotificationChannelSpec struct {
	Type NotificationType `json:"type"`
	// +optional
	Slack *SlackSettings `json:"slack,omitempty"`
	// +optional
	Email *EmailSettings `json:"email,omitempty"`
	// +optional
	Webhook *WebhookSettings `json:"webhook,omitempty"`
	// +optional
	Ntfy *NtfySettings `json:"ntfy,omitempty"`
	// SendResolved also notifies when an alert stops firing.
	// +kubebuilder:default=true
	// +optional
	SendResolved *bool `json:"sendResolved,omitempty"`
}

// NotificationChannelStatus is written by the alerting reconciler.
type NotificationChannelStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// SecretSet says the channel's Secret holds what its type needs (the
	// webhook URL, the SMTP password, the ntfy token); never the values.
	// +optional
	SecretSet bool `json:"secretSet,omitempty"`
	// LastTest is when the console last sent a test notification, and how it went.
	// +optional
	LastTest *metav1.Time `json:"lastTest,omitempty"`
	// +optional
	LastTestError string `json:"lastTestError,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NotificationChannel is a destination for alerts. Cluster-scoped, managed
// by owners and admins.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=notify
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type NotificationChannel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NotificationChannelSpec   `json:"spec,omitempty"`
	Status NotificationChannelStatus `json:"status,omitempty"`
}

// NotificationChannelList contains a list of NotificationChannels.
//
// +kubebuilder:object:root=true
type NotificationChannelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NotificationChannel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AlertRule{}, &AlertRuleList{}, &NotificationChannel{}, &NotificationChannelList{})
}
