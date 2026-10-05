package controllers

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// LabelVolumePrefix marks pods that mount a shared Volume:
// volume.kwerft.dev/<volume>: "true". Pods sharing a Volume prefer each
// other's node, since the disk is ReadWriteOnce.
const LabelVolumePrefix = "volume.kwerft.dev/"

// defaultSecretMode is AppVolume.mode's default (0444). Secret files belong
// to root (Kwerft sets no fsGroup), so a container running as another user
// can only read them world-readable; ssh accepts such a key, since it only
// rejects loose permissions on keys its own user owns.
const defaultSecretMode int32 = 0o444

// podShape is what Apps and Tasks have in common: one container from an
// image, with command, env, resources and volumes. App and Task renders fill
// it from their spec and add what is theirs (ports and probes; restart policy
// and priority).
type podShape struct {
	container   string // container name
	image       string
	pullSecret  string
	command     []string
	env         []corev1.EnvVar
	size        string
	resources   *corev1.ResourceRequirements
	ports       []kwerftv1.AppPort
	healthCheck *kwerftv1.HealthCheck
	volumes     []kwerftv1.AppVolume
	labels      map[string]string
	annotations map[string]string
	// drainSeconds delays SIGTERM by a preStop sleep; 0 for Tasks.
	drainSeconds int32
	// stopSeconds replaces the default time between SIGTERM and SIGKILL
	// (TaskSpec.StopSeconds); 0 keeps it.
	stopSeconds int32
}

// stopSeconds is what a container gets between SIGTERM and SIGKILL after it
// has drained: Kubernetes' default grace period.
const stopSeconds = 30

func (p *podShape) resourceRequirements() *corev1ac.ResourceRequirementsApplyConfiguration {
	if p.size == "custom" && p.resources != nil {
		return corev1ac.ResourceRequirements().
			WithRequests(p.resources.Requests).
			WithLimits(p.resources.Limits)
	}
	preset, ok := sizes[p.size]
	if !ok {
		preset = sizes["small"]
	}
	mem := resource.MustParse(preset[1])
	// No CPU limit: throttling hurts latency more than sharing spare CPU does.
	return corev1ac.ResourceRequirements().
		WithRequests(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(preset[0]), corev1.ResourceMemory: mem}).
		WithLimits(corev1.ResourceList{corev1.ResourceMemory: mem})
}

func (p *podShape) containerConfig() *corev1ac.ContainerApplyConfiguration {
	c := corev1ac.Container().
		WithName(p.container).
		WithImage(p.image).
		WithResources(p.resourceRequirements()).
		WithSecurityContext(corev1ac.SecurityContext().WithAllowPrivilegeEscalation(false))
	if len(p.command) > 0 {
		c.WithCommand(p.command...)
	}
	for _, e := range p.env {
		c.WithEnv(envVar(e))
	}
	for _, port := range uniquePorts(p.ports) {
		c.WithPorts(corev1ac.ContainerPort().WithContainerPort(port.Container).WithProtocol(protocol(port)))
	}
	if hc := p.healthCheck; hc != nil {
		c.WithReadinessProbe(probe(hc).WithPeriodSeconds(5).WithFailureThreshold(3))
		c.WithLivenessProbe(probe(hc).WithPeriodSeconds(10).WithFailureThreshold(6))
	}
	// A stopping pod leaves the Service's endpoints at once, but Cilium and
	// callers holding keep-alive connections learn it a moment later; it
	// keeps serving until then. The kubelet's own sleep, since distroless
	// images have no sleep binary.
	if p.drainSeconds > 0 {
		c.WithLifecycle(corev1ac.Lifecycle().WithPreStop(corev1ac.LifecycleHandler().
			WithSleep(corev1ac.SleepAction().WithSeconds(int64(p.drainSeconds)))))
	}
	for i, v := range p.volumes {
		m := corev1ac.VolumeMount().WithName(volumeName(i)).WithMountPath(v.Path)
		if v.ReadOnly || v.Secret != "" {
			m.WithReadOnly(true)
		}
		c.WithVolumeMounts(m)
	}
	return c
}

// template is the pod template. Disks of its own (AppVolume.size) are only
// mounted here; the StatefulSet's claim templates provide them. Shared
// Volumes become claim references, Secrets secret volumes.
func (p *podShape) template() *corev1ac.PodTemplateSpecApplyConfiguration {
	labels := maps.Clone(p.labels)
	spec := corev1ac.PodSpec().
		WithContainers(p.containerConfig()).
		WithEnableServiceLinks(false).
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault)))
	if p.drainSeconds > 0 || p.stopSeconds > 0 {
		stop := int64(stopSeconds)
		if p.stopSeconds > 0 {
			stop = int64(p.stopSeconds)
		}
		// The grace period counts from the start of preStop.
		spec.WithTerminationGracePeriodSeconds(int64(p.drainSeconds) + stop)
	}
	if p.pullSecret != "" {
		spec.WithImagePullSecrets(corev1ac.LocalObjectReference().WithName(p.pullSecret))
	}
	var colocate []*corev1ac.WeightedPodAffinityTermApplyConfiguration
	for i, v := range p.volumes {
		if v.Secret != "" {
			mode := defaultSecretMode
			if v.Mode != nil {
				mode = *v.Mode
			}
			// Not optional: without the Secret the pod waits in
			// ContainerCreating rather than starting without its files.
			spec.WithVolumes(corev1ac.Volume().
				WithName(volumeName(i)).
				WithSecret(corev1ac.SecretVolumeSource().WithSecretName(v.Secret).WithDefaultMode(mode)))
			continue
		}
		if v.Volume == "" {
			continue
		}
		spec.WithVolumes(corev1ac.Volume().
			WithName(volumeName(i)).
			WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().
				WithClaimName(volumeClaimName(v.Volume)).
				WithReadOnly(v.ReadOnly)))
		key := LabelVolumePrefix + v.Volume
		labels[key] = "true"
		// Preferred, not required: a required term would leave a pod that
		// mounts two Volumes, only one of them in use elsewhere, unschedulable.
		colocate = append(colocate, corev1ac.WeightedPodAffinityTerm().
			WithWeight(100).
			WithPodAffinityTerm(corev1ac.PodAffinityTerm().
				WithLabelSelector(metav1ac.LabelSelector().WithMatchExpressions(
					metav1ac.LabelSelectorRequirement().WithKey(key).WithOperator(metav1.LabelSelectorOpExists))).
				WithTopologyKey(corev1.LabelHostname)))
	}
	if len(colocate) > 0 {
		spec.WithAffinity(corev1ac.Affinity().WithPodAffinity(
			corev1ac.PodAffinity().WithPreferredDuringSchedulingIgnoredDuringExecution(colocate...)))
	}
	tmpl := corev1ac.PodTemplateSpec().WithLabels(labels).WithSpec(spec)
	if len(p.annotations) > 0 {
		tmpl.WithAnnotations(p.annotations)
	}
	return tmpl
}

// withEgress limits outbound traffic: none (DNS and the cluster only), https
// (plus port 443 on the internet) or all (no egress policy).
func withEgress(spec *networkingv1ac.NetworkPolicySpecApplyConfiguration, egress string) {
	if egress == "" {
		egress = "https"
	}
	if egress == "all" {
		return
	}
	spec.WithPolicyTypes(networkingv1.PolicyTypeEgress)
	// DNS, and any pod in the cluster: the target's ingress policy decides.
	spec.WithEgress(
		networkingv1ac.NetworkPolicyEgressRule().
			WithTo(networkingv1ac.NetworkPolicyPeer().
				WithNamespaceSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{"kubernetes.io/metadata.name": "kube-system"})).
				WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{"k8s-app": "kube-dns"}))).
			WithPorts(
				networkingv1ac.NetworkPolicyPort().WithProtocol(corev1.ProtocolUDP).WithPort(intstr.FromInt32(53)),
				networkingv1ac.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt32(53))),
		networkingv1ac.NetworkPolicyEgressRule().
			WithTo(networkingv1ac.NetworkPolicyPeer().WithNamespaceSelector(metav1ac.LabelSelector())),
	)
	if egress == "https" {
		spec.WithEgress(networkingv1ac.NetworkPolicyEgressRule().
			WithTo(networkingv1ac.NetworkPolicyPeer().WithIPBlock(networkingv1ac.IPBlock().
				WithCIDR("0.0.0.0/0").
				WithExcept("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"))).
			WithPorts(networkingv1ac.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(intstr.FromInt32(443))))
	}
}
