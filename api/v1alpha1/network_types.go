package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TrafficPeer is one side of a TrafficRule. Exactly one field is set.
//
// +kubebuilder:validation:XValidation:rule="(has(self.app) ? 1 : 0) + (has(self.project) ? 1 : 0) + (has(self.internet) && self.internet ? 1 : 0) + (has(self.cidr) ? 1 : 0) == 1",message="set exactly one of app, project, internet or cidr"
type TrafficPeer struct {
	// App in this rule's project, or "<project>/<app>" in another.
	// +kubebuilder:validation:MaxLength=127
	// +optional
	App string `json:"app,omitempty"`
	// Project: every app of that project.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Project string `json:"project,omitempty"`
	// Internet: traffic from outside the cluster through the ingress
	// (sources), or to public addresses (destinations).
	// +optional
	Internet bool `json:"internet,omitempty"`
	// CIDR outside the cluster, e.g. 10.0.0.0/16 (a Hetzner private network).
	// +kubebuilder:validation:MaxLength=43
	// +optional
	CIDR string `json:"cidr,omitempty"`
}

// TrafficPort is a port and protocol a rule allows.
type TrafficPort struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +optional
	EndPort int32 `json:"endPort,omitempty"`
	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default=TCP
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// TrafficRuleSpec allows traffic between apps, projects and the outside, on
// top of the project's default (isolated projects deny all ingress from
// other projects; Apps open their own ports to the ingress and their
// project).
type TrafficRuleSpec struct {
	// From are the sources. For an ingress rule (To empty or apps of this
	// project) they may be anything; Internet means through the ingress.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	From []TrafficPeer `json:"from"`
	// To are the destinations. Apps of this rule's project receive (ingress);
	// Internet, CIDR or other projects' apps are reached by From, which
	// must then be apps of this project (egress).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	To []TrafficPeer `json:"to"`
	// Ports; empty means every port.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Ports []TrafficPort `json:"ports,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Description string `json:"description,omitempty"`
	// Disabled rules are kept but not applied.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
}

// TrafficCounts are Hubble's flow counts for a rule over the last hour.
type TrafficCounts struct {
	Allowed int64 `json:"allowed"`
	Dropped int64 `json:"dropped"`
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
}

// TrafficRuleStatus is written by the TrafficRule reconciler.
type TrafficRuleStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Policies are the CiliumNetworkPolicies the rule became.
	// +optional
	Policies []string `json:"policies,omitempty"`
	// +optional
	Counts *TrafficCounts `json:"counts,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TrafficRule allows network traffic in the project it lives in (namespaced).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=traffic
// +kubebuilder:printcolumn:name="Disabled",type=boolean,JSONPath=`.spec.disabled`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type TrafficRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrafficRuleSpec   `json:"spec,omitempty"`
	Status TrafficRuleStatus `json:"status,omitempty"`
}

// TrafficRuleList contains a list of TrafficRules.
//
// +kubebuilder:object:root=true
type TrafficRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrafficRule `json:"items"`
}

// FirewallRuleSpec opens a port on the servers themselves (not the apps,
// which the ingress serves). The installer's rules (SSH, HTTP(S), the
// cluster's private ports) are FirewallRules labelled kwerft.dev/required
// that cannot be deleted, only narrowed where safe (SSH sources).
type FirewallRuleSpec struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +optional
	EndPort int32 `json:"endPort,omitempty"`
	// +kubebuilder:validation:Enum=TCP;UDP;ICMP
	Protocol string `json:"protocol"`
	// Sources allowed in, as CIDRs; empty means anywhere.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Sources []string `json:"sources,omitempty"`
	// Nodes it applies to: all, or only control-plane nodes.
	// +kubebuilder:validation:Enum=all;control-plane
	// +kubebuilder:default=all
	// +optional
	Nodes string `json:"nodes,omitempty"`
	// +optional
	Description string `json:"description,omitempty"`
	// +optional
	Disabled bool `json:"disabled,omitempty"`
}

// FirewallRuleStatus is written by the firewall reconciler.
type FirewallRuleStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// CloudFirewall reports the Hetzner Cloud Firewall sync for cloud nodes.
	// +optional
	CloudFirewall string `json:"cloudFirewall,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FirewallRule is a server firewall rule (cluster-scoped, owners and admins).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fw
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.port`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type FirewallRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FirewallRuleSpec   `json:"spec,omitempty"`
	Status FirewallRuleStatus `json:"status,omitempty"`
}

// FirewallRuleList contains a list of FirewallRules.
//
// +kubebuilder:object:root=true
type FirewallRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FirewallRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrafficRule{}, &TrafficRuleList{}, &FirewallRule{}, &FirewallRuleList{})
}
