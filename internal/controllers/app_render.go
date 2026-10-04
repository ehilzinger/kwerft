package controllers

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	werftv1 "github.com/ehilzinger/werft/api/v1alpha1"
)

// Storage classes behind AppVolume.Class.
var storageClasses = map[string]string{
	"local-nvme":    "local-path",     // k3s local-path provisioner
	"hcloud-volume": "hcloud-volumes", // Hetzner Cloud CSI
}

// Size presets offered by the deploy wizard: CPU request, memory request = limit.
var sizes = map[string][2]string{
	"small":  {"250m", "256Mi"},
	"medium": {"500m", "512Mi"},
	"large":  {"1", "2Gi"},
}

// appRender holds everything derived from one App for one reconcile.
type appRender struct {
	app      *werftv1.App
	image    string
	project  string
	owner    *metav1ac.OwnerReferenceApplyConfiguration
	selector map[string]string
	labels   map[string]string
}

func newAppRender(app *werftv1.App, image, project string) *appRender {
	sel := map[string]string{LabelApp: app.Name}
	return &appRender{
		app:      app,
		image:    image,
		project:  project,
		owner:    controllerRef(app, werftv1.GroupVersion.WithKind("App")),
		selector: sel,
		labels:   map[string]string{LabelApp: app.Name, LabelProject: project, LabelManagedBy: ManagedByWerft},
	}
}

func (a *appRender) replicas() int32 {
	if a.app.Spec.Replicas != nil {
		return *a.app.Spec.Replicas
	}
	return 1
}

// stateful apps (any volume) run as a StatefulSet so each replica keeps its disk.
func (a *appRender) stateful() bool { return len(a.app.Spec.Volumes) > 0 }

func (a *appRender) resources() *corev1ac.ResourceRequirementsApplyConfiguration {
	if a.app.Spec.Size == "custom" && a.app.Spec.Resources != nil {
		return corev1ac.ResourceRequirements().
			WithRequests(a.app.Spec.Resources.Requests).
			WithLimits(a.app.Spec.Resources.Limits)
	}
	preset, ok := sizes[a.app.Spec.Size]
	if !ok {
		preset = sizes["small"]
	}
	mem := resource.MustParse(preset[1])
	// No CPU limit: throttling hurts latency more than sharing spare CPU does.
	return corev1ac.ResourceRequirements().
		WithRequests(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(preset[0]), corev1.ResourceMemory: mem}).
		WithLimits(corev1.ResourceList{corev1.ResourceMemory: mem})
}

func (a *appRender) container() *corev1ac.ContainerApplyConfiguration {
	c := corev1ac.Container().
		WithName("app").
		WithImage(a.image).
		WithResources(a.resources()).
		WithSecurityContext(corev1ac.SecurityContext().WithAllowPrivilegeEscalation(false))
	if len(a.app.Spec.Command) > 0 {
		c.WithCommand(a.app.Spec.Command...)
	}
	for _, e := range a.app.Spec.Env {
		c.WithEnv(envVar(e))
	}
	for _, p := range a.app.Spec.Ports {
		c.WithPorts(corev1ac.ContainerPort().WithContainerPort(p.Container).WithProtocol(protocol(p)))
	}
	if hc := a.app.Spec.HealthCheck; hc != nil {
		c.WithReadinessProbe(probe(hc).WithPeriodSeconds(5).WithFailureThreshold(3))
		c.WithLivenessProbe(probe(hc).WithPeriodSeconds(10).WithFailureThreshold(6))
	}
	for i, v := range a.app.Spec.Volumes {
		c.WithVolumeMounts(corev1ac.VolumeMount().WithName(volumeName(i)).WithMountPath(v.Path))
	}
	return c
}

func (a *appRender) podTemplate() *corev1ac.PodTemplateSpecApplyConfiguration {
	spec := corev1ac.PodSpec().
		WithContainers(a.container()).
		WithEnableServiceLinks(false).
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault)))
	if src := a.app.Spec.Source.Image; src != nil && src.PullSecret != "" {
		spec.WithImagePullSecrets(corev1ac.LocalObjectReference().WithName(src.PullSecret))
	}
	return corev1ac.PodTemplateSpec().WithLabels(a.labels).WithSpec(spec)
}

func (a *appRender) deployment() *appsv1ac.DeploymentApplyConfiguration {
	return appsv1ac.Deployment(a.app.Name, a.app.Namespace).
		WithLabels(a.labels).
		WithOwnerReferences(a.owner).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(a.replicas()).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(a.selector)).
			// Start the new replica before stopping an old one: no downtime
			// as long as the health check is honest.
			WithStrategy(appsv1ac.DeploymentStrategy().
				WithType(appsv1.RollingUpdateDeploymentStrategyType).
				WithRollingUpdate(appsv1ac.RollingUpdateDeployment().
					WithMaxUnavailable(intstr.FromInt32(0)).
					WithMaxSurge(intstr.FromInt32(1)))).
			WithTemplate(a.podTemplate()))
}

func (a *appRender) statefulSet() *appsv1ac.StatefulSetApplyConfiguration {
	spec := appsv1ac.StatefulSetSpec().
		WithReplicas(a.replicas()).
		WithServiceName(a.app.Name).
		WithSelector(metav1ac.LabelSelector().WithMatchLabels(a.selector)).
		WithTemplate(a.podTemplate())
	for i, v := range a.app.Spec.Volumes {
		class := storageClasses[v.Class]
		if class == "" {
			class = storageClasses["local-nvme"]
		}
		spec.WithVolumeClaimTemplates(corev1ac.PersistentVolumeClaim(volumeName(i), "").
			WithSpec(corev1ac.PersistentVolumeClaimSpec().
				WithAccessModes(corev1.ReadWriteOnce).
				WithStorageClassName(class).
				WithResources(corev1ac.VolumeResourceRequirements().
					WithRequests(corev1.ResourceList{corev1.ResourceStorage: v.Size}))))
	}
	return appsv1ac.StatefulSet(a.app.Name, a.app.Namespace).
		WithLabels(a.labels).
		WithOwnerReferences(a.owner).
		WithSpec(spec)
}

func (a *appRender) service() *corev1ac.ServiceApplyConfiguration {
	spec := corev1ac.ServiceSpec().WithSelector(a.selector)
	for _, p := range a.app.Spec.Ports {
		spec.WithPorts(corev1ac.ServicePort().
			WithName(portName(p)).
			WithPort(p.Container).
			WithTargetPort(intstr.FromInt32(p.Container)).
			WithProtocol(protocol(p)))
	}
	return corev1ac.Service(a.app.Name, a.app.Namespace).
		WithLabels(a.labels).
		WithOwnerReferences(a.owner).
		WithSpec(spec)
}

// routes returns one HTTPRoute per public port, keyed by route name.
// TLS listeners for these hostnames are the Domain reconciler's job.
func (a *appRender) routes() map[string]*gwv1ac.HTTPRouteApplyConfiguration {
	out := map[string]*gwv1ac.HTTPRouteApplyConfiguration{}
	for _, p := range a.app.Spec.Ports {
		if p.Public == "" {
			continue
		}
		name := fmt.Sprintf("%s-%d", a.app.Name, p.Container)
		out[name] = gwv1ac.HTTPRoute(name, a.app.Namespace).
			WithLabels(a.labels).
			WithOwnerReferences(a.owner).
			WithSpec(gwv1ac.HTTPRouteSpec().
				WithParentRefs(gwv1ac.ParentReference().
					WithName(gwv1.ObjectName(GatewayName)).
					WithNamespace(gwv1.Namespace(GatewayNamespace))).
				WithHostnames(gwv1.Hostname(p.Public)).
				WithRules(gwv1ac.HTTPRouteRule().
					WithBackendRefs(gwv1ac.HTTPBackendRef().
						WithName(gwv1.ObjectName(a.app.Name)).
						WithPort(gwv1.PortNumber(p.Container)))))
	}
	return out
}

// networkPolicy opens the project's default-deny for this app: listed apps and
// platform namespaces may connect in; outbound follows Spec.Egress.
//
// TODO(phase-4): switch to CiliumNetworkPolicy so traffic from Traefik on
// other nodes (host network, entity remote-node) is matched precisely.
func (a *appRender) networkPolicy() *networkingv1ac.NetworkPolicyApplyConfiguration {
	var ports []*networkingv1ac.NetworkPolicyPortApplyConfiguration
	for _, p := range a.app.Spec.Ports {
		ports = append(ports, networkingv1ac.NetworkPolicyPort().
			WithProtocol(protocol(p)).
			WithPort(intstr.FromInt32(p.Container)))
	}

	from := []*networkingv1ac.NetworkPolicyPeerApplyConfiguration{
		networkingv1ac.NetworkPolicyPeer().WithNamespaceSelector(
			metav1ac.LabelSelector().WithMatchLabels(map[string]string{LabelSystem: "true"})),
	}
	for _, ref := range a.app.Spec.AllowFrom {
		project, app := a.project, ref
		if before, after, ok := strings.Cut(ref, "/"); ok {
			project, app = before, after
		}
		from = append(from, networkingv1ac.NetworkPolicyPeer().
			WithNamespaceSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{LabelProject: project})).
			WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{LabelApp: app})))
	}

	spec := networkingv1ac.NetworkPolicySpec().
		WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(a.selector)).
		WithPolicyTypes(networkingv1.PolicyTypeIngress).
		WithIngress(networkingv1ac.NetworkPolicyIngressRule().WithFrom(from...).WithPorts(ports...))

	egress := a.app.Spec.Egress
	if egress == "" {
		egress = "https"
	}
	if egress != "all" {
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

	return networkingv1ac.NetworkPolicy(a.app.Name, a.app.Namespace).
		WithLabels(a.labels).
		WithOwnerReferences(a.owner).
		WithSpec(spec)
}

func (a *appRender) urls() []string {
	var out []string
	for _, p := range a.app.Spec.Ports {
		if p.Public != "" {
			out = append(out, "https://"+p.Public)
		}
	}
	return out
}

func probe(hc *werftv1.HealthCheck) *corev1ac.ProbeApplyConfiguration {
	if hc.HTTP != "" {
		return corev1ac.Probe().WithHTTPGet(corev1ac.HTTPGetAction().WithPath(hc.HTTP).WithPort(intstr.FromInt32(hc.Port)))
	}
	return corev1ac.Probe().WithTCPSocket(corev1ac.TCPSocketAction().WithPort(intstr.FromInt32(hc.Port)))
}

func envVar(e corev1.EnvVar) *corev1ac.EnvVarApplyConfiguration {
	out := corev1ac.EnvVar().WithName(e.Name)
	if e.ValueFrom == nil {
		return out.WithValue(e.Value)
	}
	src := corev1ac.EnvVarSource()
	switch vf := e.ValueFrom; {
	case vf.SecretKeyRef != nil:
		ref := corev1ac.SecretKeySelector().WithName(vf.SecretKeyRef.Name).WithKey(vf.SecretKeyRef.Key)
		if vf.SecretKeyRef.Optional != nil {
			ref.WithOptional(*vf.SecretKeyRef.Optional)
		}
		src.WithSecretKeyRef(ref)
	case vf.ConfigMapKeyRef != nil:
		ref := corev1ac.ConfigMapKeySelector().WithName(vf.ConfigMapKeyRef.Name).WithKey(vf.ConfigMapKeyRef.Key)
		if vf.ConfigMapKeyRef.Optional != nil {
			ref.WithOptional(*vf.ConfigMapKeyRef.Optional)
		}
		src.WithConfigMapKeyRef(ref)
	case vf.FieldRef != nil:
		src.WithFieldRef(corev1ac.ObjectFieldSelector().WithFieldPath(vf.FieldRef.FieldPath))
	}
	return out.WithValueFrom(src)
}

func protocol(p werftv1.AppPort) corev1.Protocol {
	if p.Protocol == "" {
		return corev1.ProtocolTCP
	}
	return p.Protocol
}

func portName(p werftv1.AppPort) string {
	return fmt.Sprintf("%s-%d", strings.ToLower(string(protocol(p))), p.Container)
}

func volumeName(i int) string { return fmt.Sprintf("data-%d", i) }
