package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConsoleSettingsName is the only ConsoleSettings object Kwerft reads.
const ConsoleSettingsName = "kwerft"

// TLSMode says how certificates for app hostnames are issued.
// +kubebuilder:validation:Enum=http01;dns01
type TLSMode string

const (
	// TLSHTTP01 issues one certificate per hostname through ACME HTTP-01.
	// No DNS credentials are needed; every hostname needs its own DNS record
	// (or a wildcard record) pointing at the server.
	TLSHTTP01 TLSMode = "http01"
	// TLSDNS01 issues one wildcard certificate for *.<appsDomain> through
	// ACME DNS-01, solved via the DNS provider's API. Hostnames directly
	// under appsDomain share one Gateway listener.
	TLSDNS01 TLSMode = "dns01"
)

// DNSSettings is the DNS provider that hosts the zones of the console
// hostname and the apps domain.
type DNSSettings struct {
	// Provider of the zones. Only Hetzner (DNS in the Hetzner Console, Cloud
	// API) for now. It solves DNS-01 challenges for tls dns01.
	// +kubebuilder:validation:Enum=hetzner
	Provider string `json:"provider"`

	// ManageRecords makes Kwerft keep A and AAAA records for the console
	// hostname and *.<appsDomain> pointing at the nodes' public addresses
	// (status.publicAddresses). It labels the records it creates and never
	// changes records it did not create. Turning it off leaves the records
	// in place.
	// +optional
	ManageRecords bool `json:"manageRecords,omitempty"`
}

// ConsoleSettingsSpec is what owners and admins choose on the Settings page
// (or the installer from --domain and --config).
type ConsoleSettingsSpec struct {
	// ConsoleDomain is the console's hostname. Empty falls back to the
	// --console-domain flag the installer passed. Changing it adds a listener
	// for the new name; the console moves there once its certificate is
	// issued (status.consoleDomain).
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ConsoleDomain string `json:"consoleDomain,omitempty"`

	// AppsDomain is the base domain for apps, e.g. apps.example.com. The
	// deploy wizard suggests <app>.<appsDomain>. With tls dns01, hostnames
	// one label below it share a wildcard listener and certificate.
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=200
	// +optional
	AppsDomain string `json:"appsDomain,omitempty"`

	// TLS is how app certificates are issued.
	// +kubebuilder:default=http01
	// +optional
	TLS TLSMode `json:"tls,omitempty"`

	// DNS is the provider for tls dns01 and for managed records. Its API
	// token lives in the Secret kwerft-dns-token (key "token") in
	// kwerft-system, which owners and admins may write through the console
	// but nobody reads back.
	// +optional
	DNS *DNSSettings `json:"dns,omitempty"`

	// SSO is single sign-on through an OpenID Connect provider. The client
	// secret lives in the Secret kwerft-oidc-client (key "clientSecret") in
	// kwerft-system, which owners and admins may write through the console
	// but nobody reads back.
	// +optional
	SSO *SSOSettings `json:"sso,omitempty"`

	// HetznerCloud is what Kwerft does with the Hetzner Cloud API: the
	// Cloud Firewall and a Load Balancer in front of the ingress. Its API
	// token lives in the Secret kwerft-hcloud-token (key "token") in
	// kwerft-system, which owners and admins may write through the console
	// but nobody reads back.
	// +optional
	HetznerCloud *HetznerCloudSettings `json:"hetznerCloud,omitempty"`

	// Updates is how this console learns about and installs new releases
	// (docs/phase6-upgrades.md); management cluster only, for every cluster.
	// +optional
	Updates *UpdateSettings `json:"updates,omitempty"`

	// Backups is where backups go (docs/phase6.md). The bucket's access
	// keys live in the Secret kwerft-backup-credentials and the recovery
	// key in kwerft-backup-key (kwerft-system), which owners and admins may
	// write but nobody reads back.
	// +optional
	Backups *BackupSettings `json:"backups,omitempty"`
}

// UpdatePolicy is what the console does about new releases.
// +kubebuilder:validation:Enum=Off;Notify;AutoPatch
type UpdatePolicy string

const (
	UpdatesOff       UpdatePolicy = "Off"
	UpdatesNotify    UpdatePolicy = "Notify"
	UpdatesAutoPatch UpdatePolicy = "AutoPatch"
)

// UpdateSettings: channel, policy and the maintenance window for AutoPatch.
type UpdateSettings struct {
	// +kubebuilder:validation:Enum=stable;edge
	// +kubebuilder:default=stable
	// +optional
	Channel string `json:"channel,omitempty"`
	// +kubebuilder:default=Notify
	// +optional
	Policy UpdatePolicy `json:"policy,omitempty"`
	// KubernetesPatches lets AutoPatch install k3s patch versions too.
	// +optional
	KubernetesPatches bool `json:"kubernetesPatches,omitempty"`
	// +optional
	Window *MaintenanceWindow `json:"window,omitempty"`
}

// MaintenanceWindow is when automatic upgrades may start.
type MaintenanceWindow struct {
	// Days of the week (Mon … Sun); empty: every day.
	// +listType=set
	// +optional
	Days []string `json:"days,omitempty"`
	// Start as HH:MM in TimeZone.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start"`
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`
	// TimeZone, an IANA name (default UTC).
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
}

// BackupSettings: the S3-compatible bucket backups go to (Hetzner Object
// Storage) and the etcd snapshot schedule.
type BackupSettings struct {
	// Endpoint, e.g. https://fsn1.your-objectstorage.com.
	// +kubebuilder:validation:Pattern=`^https://[^/\s]+/?$`
	Endpoint string `json:"endpoint"`
	// Region, e.g. fsn1.
	// +optional
	Region string `json:"region,omitempty"`
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	Bucket string `json:"bucket"`
	// Prefix inside the bucket, so several consoles can share one
	// (default: the console's hostname).
	// +kubebuilder:validation:MaxLength=200
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// EtcdSnapshots: k3s's own snapshots of the cluster state, which k3s
	// keeps locally (every 6 hours, 28 kept, unless set here). Set, Kwerft's
	// node agent also uploads them to the same bucket (folder
	// <prefix>/etcd/<node>), encrypted with the SSE-C key derived from the
	// recovery key, and keeps the same number there; nil keeps them local
	// only. The installer applies a changed schedule on its next run (k3s
	// reads it only when it starts).
	// +optional
	EtcdSnapshots *EtcdSnapshotSettings `json:"etcdSnapshots,omitempty"`
}

// EtcdSnapshotSettings schedules k3s etcd snapshots and sends them to the bucket.
type EtcdSnapshotSettings struct {
	// Schedule in cron syntax (default every 6 hours).
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// Retention: how many snapshots to keep (default 28).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	// +optional
	Retention int32 `json:"retention,omitempty"`
}

// CloudFirewallMode says whether Kwerft keeps a Hetzner Cloud Firewall.
// +kubebuilder:validation:Enum=sync;off
type CloudFirewallMode string

const (
	// CloudFirewallSync (the default) keeps one Cloud Firewall per cluster
	// with the public part of the FirewallRules, applied to the cluster's
	// Cloud servers.
	CloudFirewallSync CloudFirewallMode = "sync"
	// CloudFirewallOff removes Kwerft's Cloud Firewall from the servers and
	// deletes it. The host firewall stays.
	CloudFirewallOff CloudFirewallMode = "off"
)

// HetznerCloudSettings are the Cloud API features owners and admins turn on.
type HetznerCloudSettings struct {
	// Firewall: sync (default) or off.
	// +optional
	Firewall CloudFirewallMode `json:"firewall,omitempty"`

	// LoadBalancer puts a Hetzner Load Balancer in front of the ingress.
	// +optional
	LoadBalancer *LoadBalancerSettings `json:"loadBalancer,omitempty"`
}

// LoadBalancerSettings: a Hetzner Load Balancer that forwards TCP 80 and 443
// (with the PROXY protocol, so the ingress sees client addresses) to the
// cluster's Cloud servers over the private network. While it serves, the
// console's and the apps' DNS records point at it instead of the nodes.
type LoadBalancerSettings struct {
	Enabled bool `json:"enabled"`
	// Type of the Load Balancer, e.g. lb11 (the default).
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,31}$`
	// +optional
	Type string `json:"type,omitempty"`
	// Location, e.g. fsn1; empty: the location of the cluster's first Cloud
	// server.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,31}$`
	// +optional
	Location string `json:"location,omitempty"`
}

// SSOSettings configures "Sign in with …" on the console's sign-in page.
// Users are matched by the email address the provider verified; a first
// sign-in links the provider account to the user.
type SSOSettings struct {
	// Enabled shows the button on the sign-in page.
	Enabled bool `json:"enabled"`

	// Provider is the preset: google, microsoft (Entra ID, one tenant),
	// keycloak or oidc (any OpenID Connect provider).
	// +kubebuilder:validation:Enum=google;microsoft;keycloak;oidc
	Provider string `json:"provider"`

	// Issuer is the provider's issuer URL; discovery reads
	// <issuer>/.well-known/openid-configuration.
	// +kubebuilder:validation:Pattern=`^https://[^\s?#]+$`
	// +kubebuilder:validation:MaxLength=500
	Issuer string `json:"issuer"`

	// ClientID of the console's OAuth client at the provider.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=500
	ClientID string `json:"clientID"`

	// DisplayName names the provider on the button ("Sign in with …").
	// +kubebuilder:validation:MaxLength=60
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// AllowedDomains, when set, admits only email addresses in these
	// domains. Auto-join requires it.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	AllowedDomains []string `json:"allowedDomains,omitempty"`

	// AutoJoin creates an account with DefaultRole for a verified address
	// in AllowedDomains on its first sign-in. Off: only existing members and
	// people with an open invite can sign in.
	// +optional
	AutoJoin bool `json:"autoJoin,omitempty"`

	// DefaultRole for auto-joined accounts.
	// +kubebuilder:validation:Enum=developer;viewer
	// +optional
	DefaultRole string `json:"defaultRole,omitempty"`
}

// CertificateState is one certificate the console itself depends on.
type CertificateState struct {
	// Name of the cert-manager Certificate in kwerft-system.
	Name string `json:"name"`
	// Hostnames the certificate covers.
	Hostnames []string `json:"hostnames"`
	// Purpose: console, console-next, console-previous or apps-wildcard.
	Purpose string `json:"purpose"`
	// Ready mirrors the Certificate's Ready condition.
	Ready bool `json:"ready"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	NotAfter *metav1.Time `json:"notAfter,omitempty"`
}

// ConsoleSettingsStatus is written by the Domain reconciler, which owns the
// shared Gateway.
type ConsoleSettingsStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ConsoleDomain is the hostname the console is served on now. While a
	// change waits for its certificate it still names the old hostname.
	// +optional
	ConsoleDomain string `json:"consoleDomain,omitempty"`

	// PreviousConsoleDomain redirects to ConsoleDomain for a day after a
	// switch, so open tabs and bookmarks find the new name.
	// +optional
	PreviousConsoleDomain string `json:"previousConsoleDomain,omitempty"`
	// +optional
	SwitchedAt *metav1.Time `json:"switchedAt,omitempty"`

	// WildcardDomain is "*.<appsDomain>" once the wildcard listener serves
	// apps (its certificate is issued); empty otherwise.
	// +optional
	WildcardDomain string `json:"wildcardDomain,omitempty"`

	// PublicAddresses are the nodes' external IPs: where DNS records for the
	// console and apps must point.
	// +optional
	PublicAddresses []string `json:"publicAddresses,omitempty"`

	// Certificates the console and the wildcard listener use.
	// +optional
	Certificates []CertificateState `json:"certificates,omitempty"`

	// DNS is what the DNS reconciler did with spec.dns.manageRecords. It has
	// a field of its own because another reconciler writes the rest of the
	// status.
	// +optional
	DNS *DNSStatus `json:"dns,omitempty"`

	// HetznerCloud is what the Hetzner Cloud reconciler found and did (the
	// Cloud Firewall, the Load Balancer). A field of its own, like DNS.
	// +optional
	HetznerCloud *HetznerCloudStatus `json:"hetznerCloud,omitempty"`

	// Updates is what release discovery found (docs/phase6-upgrades.md).
	// +optional
	Updates *UpdatesStatus `json:"updates,omitempty"`

	// Backups is the state of the backup target.
	// +optional
	Backups *BackupsStatus `json:"backups,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HetznerCloudStatus is the outcome of the last sync with the Cloud API.
type HetznerCloudStatus struct {
	// Servers are this cluster's nodes found in the token's project.
	// +optional
	Servers []CloudServerStatus `json:"servers,omitempty"`
	// Firewall is the Cloud Firewall sync.
	// +optional
	Firewall *CloudFirewallStatus `json:"firewall,omitempty"`
	// LoadBalancer is the Load Balancer in front of the ingress.
	// +optional
	LoadBalancer *LoadBalancerStatus `json:"loadBalancer,omitempty"`
	// Message explains a problem that kept the whole sync from running (no
	// token, token rejected); empty when it ran.
	// +optional
	Message string `json:"message,omitempty"`
	// SyncedAt is when the Cloud API was last read.
	// +optional
	SyncedAt *metav1.Time `json:"syncedAt,omitempty"`
}

// CloudServerStatus is one node that is a Hetzner Cloud server.
type CloudServerStatus struct {
	Node string `json:"node"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// +optional
	Location string `json:"location,omitempty"`
	// Labelled: the server carries kwerft.dev/cluster=<this cluster> (Kwerft
	// created it); others (the installer's first server) are named one by
	// one in the firewall and the Load Balancer.
	// +optional
	Labelled bool `json:"labelled,omitempty"`
}

// CloudFirewallStatus reports the cluster's Cloud Firewall.
type CloudFirewallStatus struct {
	// State: InSync, Applying, Off, Error.
	State string `json:"state"`
	// +optional
	ID int64 `json:"id,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// Rules in the Cloud Firewall.
	// +optional
	Rules int32 `json:"rules,omitempty"`
	// Servers it is applied to.
	// +optional
	Servers int32 `json:"servers,omitempty"`
	// Revision of the FirewallRules it carries (the confirmed one).
	// +optional
	Revision string `json:"revision,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// LoadBalancerStatus reports the Load Balancer in front of the ingress.
type LoadBalancerStatus struct {
	// State: Creating, Waiting (no healthy target yet), Active, Draining
	// (turned off; deleted once DNS moved back), Error.
	State string `json:"state"`
	// Active: DNS points at the Load Balancer (status.publicAddresses).
	// +optional
	Active bool `json:"active,omitempty"`
	// +optional
	ID int64 `json:"id,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	Location string `json:"location,omitempty"`
	// +optional
	IPv4 string `json:"ipv4,omitempty"`
	// +optional
	IPv6 string `json:"ipv6,omitempty"`
	// Targets the Load Balancer forwards to, and how many of them pass the
	// health checks on 443.
	// +optional
	Targets int32 `json:"targets,omitempty"`
	// +optional
	HealthyTargets int32 `json:"healthyTargets,omitempty"`
	// DrainingSince is when it was turned off; it is deleted DNS TTLs later.
	// +optional
	DrainingSince *metav1.Time `json:"drainingSince,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// DNSRecordState says where a managed hostname's records stand.
// +kubebuilder:validation:Enum=Managed;External;Conflict;TakenOver;NoZone;Error;Pending;Unsupported
type DNSRecordState string

const (
	// DNSManaged: Kwerft's records point at the nodes.
	DNSManaged DNSRecordState = "Managed"
	// DNSExternal: records Kwerft did not create already point at the nodes;
	// Kwerft leaves them alone.
	DNSExternal DNSRecordState = "External"
	// DNSConflict: records Kwerft did not create point elsewhere (or a CNAME
	// exists); Kwerft does not overwrite them.
	DNSConflict DNSRecordState = "Conflict"
	// DNSTakenOver: another Kwerft installation took over records this one
	// created; this one stops updating them.
	DNSTakenOver DNSRecordState = "TakenOver"
	// DNSNoZone: no primary zone in the token's project contains the host.
	DNSNoZone DNSRecordState = "NoZone"
	// DNSError: the provider's API failed for this host.
	DNSError DNSRecordState = "Error"
	// DNSPending: a remote cluster's hostname waits for the cluster's public
	// addresses; records it already has stay as they are.
	DNSPending DNSRecordState = "Pending"
	// DNSUnsupported: a remote cluster's hostname Kwerft keeps no record for
	// (more than one label below the apps domain).
	DNSUnsupported DNSRecordState = "Unsupported"
)

// DNSRecordStatus is one hostname Kwerft keeps records for.
type DNSRecordStatus struct {
	// Hostname, e.g. ops.example.com or *.apps.example.com.
	Hostname string `json:"hostname"`
	// Purpose: console, console-next, console-previous, apps, or app (a
	// remote cluster's hostname under the apps domain).
	Purpose string `json:"purpose"`
	// Project of the Domain, for purpose app.
	// +optional
	Project string `json:"project,omitempty"`
	// Zone that contains the hostname; empty for NoZone.
	// +optional
	Zone string `json:"zone,omitempty"`
	// State of the hostname's records.
	State DNSRecordState `json:"state"`
	// Values the hostname's A and AAAA records hold now.
	// +optional
	Values []string `json:"values,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// DNSStatus is the outcome of the last sync with the DNS provider.
type DNSStatus struct {
	// Records Kwerft keeps, one entry per hostname.
	// +optional
	Records []DNSRecordStatus `json:"records,omitempty"`
	// Zones are the primary zones in the token's project: hostnames in them
	// get their records from Kwerft, so the console need not wait for DNS
	// before using them.
	// +optional
	Zones []string `json:"zones,omitempty"`
	// Message explains a problem that kept the whole sync from running (no
	// token, token rejected, no public address); empty when it ran.
	// +optional
	Message string `json:"message,omitempty"`
	// SyncedAt is when the provider was last read.
	// +optional
	SyncedAt *metav1.Time `json:"syncedAt,omitempty"`
}

// ConsoleSettings holds the console's own configuration that reconcilers
// need: its hostname, the apps base domain and how certificates are issued.
// It is a singleton named "kwerft".
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=kwset
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'kwerft'",message="the console settings object must be named kwerft"
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.tls) || self.spec.tls != 'dns01' || (has(self.spec.appsDomain) && size(self.spec.appsDomain) > 0 && has(self.spec.dns))",message="tls dns01 needs appsDomain and dns"
// +kubebuilder:printcolumn:name="Console",type=string,JSONPath=`.status.consoleDomain`
// +kubebuilder:printcolumn:name="Apps",type=string,JSONPath=`.spec.appsDomain`
// +kubebuilder:printcolumn:name="TLS",type=string,JSONPath=`.spec.tls`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type ConsoleSettings struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConsoleSettingsSpec   `json:"spec,omitempty"`
	Status ConsoleSettingsStatus `json:"status,omitempty"`
}

// ConsoleSettingsList contains a list of ConsoleSettings.
//
// +kubebuilder:object:root=true
type ConsoleSettingsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConsoleSettings `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConsoleSettings{}, &ConsoleSettingsList{})
}

// UpdatesStatus is the last release discovery.
type UpdatesStatus struct {
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`
	// +optional
	Current *UpgradeVersions `json:"current,omitempty"`
	// +optional
	Available []AvailableUpdate `json:"available,omitempty"`
	// +optional
	Error string `json:"error,omitempty"`
	// AutoPatchPausedBy names the auto-update that failed or rolled back;
	// AutoPatch waits until an owner resumes it.
	// +optional
	AutoPatchPausedBy string `json:"autoPatchPausedBy,omitempty"`
}

// AvailableUpdate is one release the console could upgrade to.
type AvailableUpdate struct {
	Component UpgradeComponent `json:"component"`
	Version   string           `json:"version"`
	// Kind: Patch or Minor.
	Kind string `json:"kind"`
	// +optional
	Notes string `json:"notes,omitempty"`
	// Allowed: preflight-free rules (order, upgradeFrom, supported k3s).
	Allowed bool `json:"allowed"`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// BackupsStatus is the state of the backup target.
type BackupsStatus struct {
	// State: NotConfigured, Ready, Error.
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// RecoveryKeyCreatedAt is when the recovery key (the repository
	// password) was made; it was shown once then.
	// +optional
	RecoveryKeyCreatedAt *metav1.Time `json:"recoveryKeyCreatedAt,omitempty"`
	// +optional
	LastSuccessfulAt *metav1.Time `json:"lastSuccessfulAt,omitempty"`
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`
	// EtcdSnapshots: per control-plane node, the last etcd snapshot the
	// node agent uploaded to the bucket (encrypted with the key derived
	// from the recovery key), as the agents report it. Empty while etcd
	// snapshots do not go to the bucket.
	// +optional
	EtcdSnapshots []EtcdSnapshotUpload `json:"etcdSnapshots,omitempty"`
}

// EtcdSnapshotUpload is one node's etcd snapshot uploads.
type EtcdSnapshotUpload struct {
	// Node is the control-plane node whose local snapshots these are
	// (folder <prefix>/etcd/<node> in the bucket).
	// +optional
	Node string `json:"node,omitempty"`
	// Name of the newest snapshot in the bucket, e.g.
	// etcd-snapshot-server-1-1759665600.zip.
	// +optional
	Name string `json:"name,omitempty"`
	// UploadedAt is when the agent last finished an upload.
	// +optional
	UploadedAt *metav1.Time `json:"uploadedAt,omitempty"`
	// CheckedAt is when the agent last compared its snapshots with the
	// bucket.
	// +optional
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`
	// Stored is how many of this node's snapshots the bucket holds.
	// +optional
	Stored int32 `json:"stored,omitempty"`
	// Message says what went wrong; empty when the last pass succeeded.
	// +optional
	Message string `json:"message,omitempty"`
}
