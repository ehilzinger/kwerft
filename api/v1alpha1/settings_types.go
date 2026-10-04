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

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DNSRecordState says where a managed hostname's records stand.
// +kubebuilder:validation:Enum=Managed;External;Conflict;TakenOver;NoZone;Error
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
)

// DNSRecordStatus is one hostname Kwerft keeps records for.
type DNSRecordStatus struct {
	// Hostname, e.g. ops.example.com or *.apps.example.com.
	Hostname string `json:"hostname"`
	// Purpose: console, console-next, console-previous or apps.
	Purpose string `json:"purpose"`
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
