package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitProvider is the kind of Git host.
// +kubebuilder:validation:Enum=github;gitlab;gitea;generic
type GitProvider string

const (
	GitHub  GitProvider = "github"
	GitLab  GitProvider = "gitlab"
	Gitea   GitProvider = "gitea" // also Forgejo and Codeberg
	Generic GitProvider = "generic"
)

// GitAuth is how Kwerft authenticates to the host.
// +kubebuilder:validation:Enum=none;token;sshKey;githubApp
type GitAuth string

const (
	// GitAuthNone: public repositories over HTTPS; no commit checks.
	GitAuthNone GitAuth = "none"
	// GitAuthToken: an access token (GitHub fine-grained token, GitLab or
	// Gitea access token) for cloning over HTTPS, webhooks and commit checks.
	GitAuthToken GitAuth = "token"
	// GitAuthSSHKey: a deploy key for cloning over SSH; no commit checks.
	GitAuthSSHKey GitAuth = "sshKey"
	// GitAuthGitHubApp: a GitHub App installation (provider github).
	GitAuthGitHubApp GitAuth = "githubApp"
)

// GitHubAppSettings identifies a GitHub App installation. Its private key
// lives in the connection's credentials Secret.
type GitHubAppSettings struct {
	AppID          int64 `json:"appID"`
	InstallationID int64 `json:"installationID"`
	// Slug is the app's URL name, for links to its settings.
	// +optional
	Slug string `json:"slug,omitempty"`
}

// GitConnectionSpec is access to repositories on one Git host. Credentials
// live in the Secret "git-<name>" in the kwerft-builds namespace (keys:
// token, ssh-privatekey, known_hosts, github-app-private-key,
// webhook-secret), which owners and admins may write through the console
// but nobody reads back.
//
// +kubebuilder:validation:XValidation:rule="self.auth != 'githubApp' || (self.provider == 'github' && has(self.githubApp))",message="auth githubApp needs provider github and githubApp"
type GitConnectionSpec struct {
	Provider GitProvider `json:"provider"`

	// URL of the host, e.g. https://github.com or https://gitlab.example.com.
	// +kubebuilder:validation:Pattern=`^https://[^/\s]+(/[^\s]*)?$`
	URL string `json:"url"`

	Auth GitAuth `json:"auth"`

	// Owner limits the connection to one account, organisation or group,
	// e.g. "acme". Empty: whatever the credentials reach.
	// +optional
	Owner string `json:"owner,omitempty"`

	// +optional
	GitHubApp *GitHubAppSettings `json:"githubApp,omitempty"`

	// Projects that may build with this connection; empty means all.
	// +optional
	Projects []string `json:"projects,omitempty"`
}

// GitConnectionStatus is written by the GitConnection reconciler.
type GitConnectionStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Account the credentials act as (user, bot or app installation).
	// +optional
	Account string `json:"account,omitempty"`

	// WebhookURL receives the host's push and pull-request events.
	// +optional
	WebhookURL string `json:"webhookURL,omitempty"`

	// LastDelivery is when the last valid webhook arrived.
	// +optional
	LastDelivery *metav1.Time `json:"lastDelivery,omitempty"`

	// WebhookAutomatic: Kwerft sets up the webhooks itself (the GitHub
	// App's own, or one per repository) and none failed. False: add the
	// webhook by hand, with WebhookURL and the webhook secret.
	// +optional
	WebhookAutomatic bool `json:"webhookAutomatic,omitempty"`

	// Webhooks lists the repositories Apps build from with this connection
	// and whether each has its webhook.
	// +optional
	Webhooks []GitWebhookStatus `json:"webhooks,omitempty"`

	// WebhookFingerprint identifies the URL and secret the automatic
	// webhooks were last set to (a truncated hash, not the secret), so a
	// rotated secret or a moved console reaches them.
	// +optional
	WebhookFingerprint string `json:"webhookFingerprint,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GitWebhookStatus is the webhook of one repository.
type GitWebhookStatus struct {
	Repository string `json:"repository"`
	// Automatic: Kwerft created the webhook and keeps it current.
	Automatic bool `json:"automatic"`
	// Message says what to do when it is not automatic.
	// +optional
	Message string `json:"message,omitempty"`
}

// GitConnection grants access to repositories on a Git host. Apps with a
// Git source name it in spec.source.git.connection. Cluster-scoped: owners
// and admins manage connections for every project.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gitconn
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Auth",type=string,JSONPath=`.spec.auth`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type GitConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GitConnectionSpec   `json:"spec,omitempty"`
	Status GitConnectionStatus `json:"status,omitempty"`
}

// GitConnectionList contains a list of GitConnections.
//
// +kubebuilder:object:root=true
type GitConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitConnection{}, &GitConnectionList{})
}
