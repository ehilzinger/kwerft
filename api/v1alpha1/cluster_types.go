package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterProvider says where a cluster came from.
// +kubebuilder:validation:Enum=local;hetzner-cloud;adopted
type ClusterProvider string

const (
	// ClusterLocal is the cluster the console runs in (one, named "local").
	ClusterLocal ClusterProvider = "local"
	// ClusterHetznerCloud: Kwerft created its servers through the Cloud API.
	ClusterHetznerCloud ClusterProvider = "hetzner-cloud"
	// ClusterAdopted: installed elsewhere (dedicated servers, another
	// provider) and connected with the install command the console shows.
	ClusterAdopted ClusterProvider = "adopted"
)

// HetznerClusterSpec is how Kwerft creates a cluster on Hetzner Cloud.
type HetznerClusterSpec struct {
	// Location, e.g. fsn1, nbg1, hel1.
	// +kubebuilder:validation:MinLength=1
	Location string `json:"location"`
	// ServerType of the control-plane servers, e.g. cx22.
	// +kubebuilder:validation:MinLength=1
	ServerType string `json:"serverType"`
	// ControlPlanes: 1, or 3 for a highly available control plane.
	// +kubebuilder:validation:Enum=1;3
	// +kubebuilder:default=1
	ControlPlanes int32 `json:"controlPlanes,omitempty"`

	// Firewall: sync (default) or off — the cluster's Hetzner Cloud
	// Firewall, kept by Kwerft in that cluster as Settings › Hetzner Cloud
	// does for the console's own.
	// +optional
	Firewall CloudFirewallMode `json:"firewall,omitempty"`
	// LoadBalancer puts a Hetzner Load Balancer in front of the cluster's
	// ingress.
	// +optional
	LoadBalancer *LoadBalancerSettings `json:"loadBalancer,omitempty"`
}

// CloudSettings are the Cloud Firewall and Load Balancer settings the
// cluster's own Hetzner Cloud reconciler follows.
func (h *HetznerClusterSpec) CloudSettings() *HetznerCloudSettings {
	return &HetznerCloudSettings{Firewall: h.Firewall, LoadBalancer: h.LoadBalancer.DeepCopy()}
}

// ClusterSpec is one Kubernetes cluster the console manages.
//
// +kubebuilder:validation:XValidation:rule="self.provider != 'hetzner-cloud' || has(self.hetznerCloud)",message="a hetzner-cloud cluster needs hetznerCloud"
type ClusterSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider cannot be changed"
	Provider ClusterProvider `json:"provider"`
	// +optional
	HetznerCloud *HetznerClusterSpec `json:"hetznerCloud,omitempty"`
	// ConsoleDomain the cluster's own ingress serves apps under is set in
	// that cluster (ConsoleSettings there); this cluster's apps domain, if
	// different, is shown for reference.
	// +optional
	AppsDomain string `json:"appsDomain,omitempty"`
}

// ClusterPhase is where a cluster stands.
// +kubebuilder:validation:Enum=Pending;Provisioning;Connected;Disconnected;Failed
type ClusterPhase string

// ClusterStatus is written by the cluster reconciler and the agent tunnel.
type ClusterStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase ClusterPhase `json:"phase,omitempty"`
	// LastSeen is when the agent last answered.
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// +optional
	Nodes int32 `json:"nodes,omitempty"`
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty"`
	// HetznerCloud is what the cluster's own Hetzner Cloud reconciler last
	// reported (its Cloud Firewall and Load Balancer); hetzner-cloud
	// clusters only, copied through the agent's tunnel.
	// +optional
	HetznerCloud *HetznerCloudStatus `json:"hetznerCloud,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Cluster is a Kubernetes cluster managed from this console. It lives in
// the management cluster (where the console runs), which is itself the
// Cluster "local". Cluster-scoped; owners and admins manage clusters.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.nodes`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.kubernetesVersion`
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

// ClusterList contains a list of Clusters.
//
// +kubebuilder:object:root=true
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

// NodeRole is what a pool's nodes do.
// +kubebuilder:validation:Enum=worker;control-plane;builds
type NodeRole string

const (
	NodeWorker       NodeRole = "worker"
	NodeControlPlane NodeRole = "control-plane"
	// NodeBuilds: tainted for builds only; may scale to zero between builds.
	NodeBuilds NodeRole = "builds"
)

// NodePoolSpec is a group of Hetzner Cloud servers Kwerft keeps as nodes of
// one cluster.
type NodePoolSpec struct {
	// Cluster the nodes join ("local" for the management cluster).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cluster cannot be changed"
	Cluster string `json:"cluster"`
	// +kubebuilder:default=worker
	Role NodeRole `json:"role,omitempty"`
	// ServerType, e.g. cx32. Changing it replaces nodes one at a time.
	// +kubebuilder:validation:MinLength=1
	ServerType string `json:"serverType"`
	// Location, e.g. fsn1; must be in the cluster's network zone.
	// +kubebuilder:validation:MinLength=1
	Location string `json:"location"`
	// Count of servers. For role builds it is the maximum; the pool scales
	// to zero when no build has run for ScaleDownAfter.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	Count int32 `json:"count"`
	// +optional
	ScaleDownAfter *metav1.Duration `json:"scaleDownAfter,omitempty"`
	// Labels put on the pool's Kubernetes nodes.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// PoolNode is one server of a NodePool.
type PoolNode struct {
	Name string `json:"name"`
	// +optional
	ServerID int64 `json:"serverID,omitempty"`
	// +optional
	PublicIP string `json:"publicIP,omitempty"`
	// +optional
	PrivateIP string `json:"privateIP,omitempty"`
	// ServerType the server runs as (differs from the pool's while it is
	// being replaced).
	// +optional
	ServerType string `json:"serverType,omitempty"`
	// Phase: Creating, Joining, Ready, Draining, Deleting, Failed.
	Phase string `json:"phase"`
	// +optional
	Message string `json:"message,omitempty"`
}

// NodePoolStatus is written by the node pool reconciler.
type NodePoolStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Nodes []PoolNode `json:"nodes,omitempty"`
	// +optional
	ReadyNodes int32 `json:"readyNodes,omitempty"`
	// Desired is how many servers the pool aims for now: spec.count, or
	// for a builds pool between zero and spec.count with the build load.
	// +optional
	Desired int32 `json:"desired,omitempty"`
	// LastBuildAt is when a builds pool last saw a build running or
	// queued; it scales to zero ScaleDownAfter later.
	// +optional
	LastBuildAt *metav1.Time `json:"lastBuildAt,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NodePool keeps Count Hetzner Cloud servers joined to a cluster. Lives in
// the management cluster; cluster-scoped; owners and admins.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pool
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.serverType`
// +kubebuilder:printcolumn:name="Count",type=integer,JSONPath=`.spec.count`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyNodes`
type NodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodePoolSpec   `json:"spec,omitempty"`
	Status NodePoolStatus `json:"status,omitempty"`
}

// NodePoolList contains a list of NodePools.
//
// +kubebuilder:object:root=true
type NodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{}, &NodePool{}, &NodePoolList{})
}
