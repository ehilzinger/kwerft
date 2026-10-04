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

// DNS01Settings configures the DNS-01 solver.
type DNS01Settings struct {
	// Provider of the DNS zone that contains appsDomain. Only Hetzner (DNS in
	// the Hetzner Console, Cloud API) for now.
	// +kubebuilder:validation:Enum=hetzner
	Provider string `json:"provider"`
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

	// DNS01 configures the solver for tls dns01. Its API token lives in the
	// Secret kwerft-dns-token (key "token") in kwerft-system, which owners and
	// admins may write through the console but nobody reads back.
	// +optional
	DNS01 *DNS01Settings `json:"dns01,omitempty"`
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

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConsoleSettings holds the console's own configuration that reconcilers
// need: its hostname, the apps base domain and how certificates are issued.
// It is a singleton named "kwerft".
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=kwset
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'kwerft'",message="the console settings object must be named kwerft"
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.tls) || self.spec.tls != 'dns01' || (has(self.spec.appsDomain) && size(self.spec.appsDomain) > 0 && has(self.spec.dns01))",message="tls dns01 needs appsDomain and dns01"
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
