package controllers

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	kwerftv1ac "github.com/ehilzinger/kwerft/api/applyconfiguration/api/v1alpha1"
	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Storage classes behind AppVolume.Class and Volume.Class.
var storageClasses = map[string]string{
	"local-nvme":    "local-path",     // k3s local-path provisioner
	"hcloud-volume": "hcloud-volumes", // Hetzner Cloud CSI
}

func storageClass(class string) string {
	if sc := storageClasses[class]; sc != "" {
		return sc
	}
	return storageClasses["local-nvme"]
}

// Size presets offered by the deploy wizard: CPU request, memory request = limit.
var sizes = map[string][2]string{
	"small":  {"250m", "256Mi"},
	"medium": {"500m", "512Mi"},
	"large":  {"1", "2Gi"},
}

// appRender holds everything derived from one App for one reconcile.
type appRender struct {
	app      *kwerftv1.App
	image    string
	project  string
	owner    *metav1ac.OwnerReferenceApplyConfiguration
	selector map[string]string
	labels   map[string]string
}

func newAppRender(app *kwerftv1.App, image, project string) *appRender {
	sel := map[string]string{LabelApp: app.Name}
	return &appRender{
		app:      app,
		image:    image,
		project:  project,
		owner:    controllerRef(app, kwerftv1.GroupVersion.WithKind("App")),
		selector: sel,
		labels:   map[string]string{LabelApp: app.Name, LabelProject: project, LabelManagedBy: ManagedByKwerft},
	}
}

func (a *appRender) replicas() int32 {
	if a.app.Spec.Replicas != nil {
		return *a.app.Spec.Replicas
	}
	return 1
}

// stateful apps (any disk of their own) run as a StatefulSet so each replica
// keeps its disk. Apps that only mount shared Volumes run as a Deployment.
func (a *appRender) stateful() bool {
	for _, v := range a.app.Spec.Volumes {
		if v.Volume == "" {
			return true
		}
	}
	return false
}

// pod fills the pod template builder that Apps and Tasks share.
func (a *appRender) pod() *podShape {
	s := a.app.Spec
	p := &podShape{
		container:   "app",
		image:       a.image,
		command:     s.Command,
		env:         s.Env,
		size:        s.Size,
		resources:   s.Resources,
		ports:       s.Ports,
		healthCheck: s.HealthCheck,
		volumes:     s.Volumes,
		labels:      a.labels,
	}
	if src := s.Source.Image; src != nil {
		p.pullSecret = src.PullSecret
	}
	// A new value rolls the workload out (a Task's onSuccess.restart).
	if at := a.app.Annotations[kwerftv1.AnnotationRestartedAt]; at != "" {
		p.annotations = map[string]string{kwerftv1.AnnotationRestartedAt: at}
	}
	return p
}

func (a *appRender) podTemplate() *corev1ac.PodTemplateSpecApplyConfiguration {
	return a.pod().template()
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
		if v.Volume != "" {
			continue // shared: a claim reference in the pod template
		}
		spec.WithVolumeClaimTemplates(corev1ac.PersistentVolumeClaim(volumeName(i), "").
			WithSpec(corev1ac.PersistentVolumeClaimSpec().
				WithAccessModes(corev1.ReadWriteOnce).
				WithStorageClassName(storageClass(v.Class)).
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

// domains returns one Domain per public hostname, keyed by Domain name. The
// Domain reconciler turns each into an HTTPS listener with a certificate.
func (a *appRender) domains() map[string]*kwerftv1ac.DomainApplyConfiguration {
	out := map[string]*kwerftv1ac.DomainApplyConfiguration{}
	for _, p := range a.app.Spec.Ports {
		if p.Public == "" {
			continue
		}
		out[p.Public] = kwerftv1ac.Domain(p.Public, a.app.Namespace).
			WithLabels(a.labels).
			WithOwnerReferences(a.owner).
			WithSpec(kwerftv1ac.DomainSpec().WithHostname(p.Public))
	}
	return out
}

// routes returns, per public port, an HTTPS route on the hostname's own
// listener and a plain-HTTP route that redirects to HTTPS. Keyed by name.
func (a *appRender) routes() map[string]*gwv1ac.HTTPRouteApplyConfiguration {
	out := map[string]*gwv1ac.HTTPRouteApplyConfiguration{}
	for _, p := range a.app.Spec.Ports {
		if p.Public == "" {
			continue
		}
		name := fmt.Sprintf("%s-%d", a.app.Name, p.Container)
		out[name] = a.route(name, ListenerName(p.Public), p.Public, gwv1ac.HTTPRouteRule().
			WithBackendRefs(gwv1ac.HTTPBackendRef().
				WithName(gwv1.ObjectName(a.app.Name)).
				WithPort(gwv1.PortNumber(p.Container))))
		// ACME HTTP-01 challenges still get through: cert-manager's routes
		// match their exact path, which takes precedence over this one.
		out[name+"-redirect"] = a.route(name+"-redirect", httpListener, p.Public, gwv1ac.HTTPRouteRule().
			WithFilters(gwv1ac.HTTPRouteFilter().
				WithType(gwv1.HTTPRouteFilterRequestRedirect).
				WithRequestRedirect(gwv1ac.HTTPRequestRedirectFilter().WithScheme("https").WithStatusCode(301))))
	}
	return out
}

// route is an HTTPRoute for host attached to one listener of the shared Gateway.
func (a *appRender) route(name, listener, host string, rule *gwv1ac.HTTPRouteRuleApplyConfiguration) *gwv1ac.HTTPRouteApplyConfiguration {
	return gwv1ac.HTTPRoute(name, a.app.Namespace).
		WithLabels(a.labels).
		WithOwnerReferences(a.owner).
		WithSpec(gwv1ac.HTTPRouteSpec().
			WithParentRefs(gwv1ac.ParentReference().
				WithName(gwv1.ObjectName(GatewayName)).
				WithNamespace(gwv1.Namespace(GatewayNamespace)).
				WithSectionName(gwv1.SectionName(listener))).
			WithHostnames(gwv1.Hostname(host)).
			WithRules(rule))
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
	// Tasks started from an allowed App (fromApp) may connect too, e.g. the
	// migration of "api" reaching the database that "api" may reach.
	for _, ref := range a.app.Spec.AllowFrom {
		project, app := a.project, ref
		if before, after, ok := strings.Cut(ref, "/"); ok {
			project, app = before, after
		}
		from = append(from, networkingv1ac.NetworkPolicyPeer().
			WithNamespaceSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{LabelProject: project})).
			WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{LabelAsApp: app})))
	}

	spec := networkingv1ac.NetworkPolicySpec().
		WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(a.selector)).
		WithPolicyTypes(networkingv1.PolicyTypeIngress).
		WithIngress(networkingv1ac.NetworkPolicyIngressRule().WithFrom(from...).WithPorts(ports...))
	withEgress(spec, a.app.Spec.Egress)

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

func probe(hc *kwerftv1.HealthCheck) *corev1ac.ProbeApplyConfiguration {
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

func protocol(p kwerftv1.AppPort) corev1.Protocol {
	if p.Protocol == "" {
		return corev1.ProtocolTCP
	}
	return p.Protocol
}

func portName(p kwerftv1.AppPort) string {
	return fmt.Sprintf("%s-%d", strings.ToLower(string(protocol(p))), p.Container)
}

func volumeName(i int) string { return fmt.Sprintf("data-%d", i) }
