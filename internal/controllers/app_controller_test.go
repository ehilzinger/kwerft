package controllers

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// projectNamespace creates a Project and waits for its namespace.
func projectNamespace(t *testing.T, name string) {
	t.Helper()
	createProject(t, name, kwerftv1.ProjectSpec{})
	eventually(t, func() error {
		return k8s.Get(context.Background(), client.ObjectKey{Name: name}, &corev1.Namespace{})
	})
}

func createApp(t *testing.T, ns, name string, spec kwerftv1.AppSpec) *kwerftv1.App {
	t.Helper()
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	return app
}

// waitForApp waits until the App's Ready condition reflects its current
// generation and returns the fresh object.
func waitForApp(t *testing.T, app *kwerftv1.App, wantReason string) *kwerftv1.App {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(app), app); err != nil {
			return err
		}
		reason, err := readyReason(app.Status.Conditions, app.Generation)
		if err != nil {
			return err
		}
		if reason != wantReason {
			return fmt.Errorf("reason %q, want %q", reason, wantReason)
		}
		return nil
	})
	return app
}

func imageApp(ref string) kwerftv1.AppSpec {
	return kwerftv1.AppSpec{
		Source:      kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: ref, PullSecret: "ghcr-acme"}},
		Replicas:    ptr.To[int32](2),
		Size:        "medium",
		Ports:       []kwerftv1.AppPort{{Container: 8080, Public: "api.example.com"}},
		AllowFrom:   []string{"web-frontend", "internal/cron"},
		HealthCheck: &kwerftv1.HealthCheck{HTTP: "/healthz", Port: 8080},
		Env: []corev1.EnvVar{
			{Name: "LOG_LEVEL", Value: "info"},
			{Name: "DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "api-db"}, Key: "url"}}},
		},
	}
}

func TestAppRendersDeploymentServiceRouteAndPolicy(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "shop")
	app := createApp(t, "shop", "api", imageApp("ghcr.io/acme/api:1.42.0"))
	app = waitForApp(t, app, "Progressing") // envtest runs no pods

	// Deployment
	var d appsv1.Deployment
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &d); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&d, app) {
		t.Error("deployment is not controlled by the App")
	}
	if *d.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *d.Spec.Replicas)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/acme/api:1.42.0" {
		t.Errorf("image = %q", c.Image)
	}
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("cpu request = %s, want 500m (medium)", got.String())
	}
	if got := c.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("memory limit = %s, want 512Mi (medium)", got.String())
	}
	if _, hasCPULimit := c.Resources.Limits[corev1.ResourceCPU]; hasCPULimit {
		t.Error("presets must not set a CPU limit")
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("readiness probe = %+v", c.ReadinessProbe)
	}
	if len(c.Env) != 2 || c.Env[1].ValueFrom == nil || c.Env[1].ValueFrom.SecretKeyRef.Name != "api-db" {
		t.Errorf("env = %+v", c.Env)
	}
	if ps := d.Spec.Template.Spec.ImagePullSecrets; len(ps) != 1 || ps[0].Name != "ghcr-acme" {
		t.Errorf("imagePullSecrets = %+v", ps)
	}
	if d.Spec.Template.Labels[LabelProject] != "shop" || d.Spec.Template.Labels[LabelApp] != "api" {
		t.Errorf("pod labels = %v", d.Spec.Template.Labels)
	}

	// Service
	var svc corev1.Service
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &svc); err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
	}

	// HTTPRoute
	// (once the Domain reconciler has granted the hostname to this project)
	route := getRoute(t, "shop", "api-8080")
	if len(route.Spec.Hostnames) != 1 || route.Spec.Hostnames[0] != "api.example.com" {
		t.Errorf("hostnames = %v", route.Spec.Hostnames)
	}
	if pr := route.Spec.ParentRefs[0]; string(pr.Name) != GatewayName || pr.Namespace == nil || string(*pr.Namespace) != GatewayNamespace ||
		pr.SectionName == nil || string(*pr.SectionName) != ListenerName("api.example.com") {
		t.Errorf("parentRef = %+v, want the hostname's HTTPS listener", pr)
	}

	// CiliumNetworkPolicy: the ingress (nodes), platform namespaces, a
	// same-project app and a cross-project app with their Tasks; egress
	// https (DNS, the cluster, port 443 outside).
	assertSpec(t, ciliumSpec(t, "shop", "api"), apiPolicy)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Errorf("Kubernetes NetworkPolicy: err = %v, want none", err)
	}

	// Status
	if app.Status.Image != "ghcr.io/acme/api:1.42.0" || app.Status.Revision != 1 || len(app.Status.History) != 1 {
		t.Errorf("status = image %q revision %d history %d", app.Status.Image, app.Status.Revision, len(app.Status.History))
	}
	if len(app.Status.URLs) != 1 || app.Status.URLs[0] != "https://api.example.com" {
		t.Errorf("urls = %v", app.Status.URLs)
	}
}

// One port under two hostnames (a site moving to a new name): one Service
// and container port, a Domain and a route per hostname, the first route
// keeping the name it had before the second hostname was added.
func TestAppPortWithTwoHostnames(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "moving")
	spec := imageApp("ghcr.io/acme/edge:1")
	spec.Ports = []kwerftv1.AppPort{
		{Container: 8080, Public: "next.example.com"},
		{Container: 8080, Public: "www.example.com"},
	}
	app := createApp(t, "moving", "edge", spec)
	app = waitForApp(t, app, "Progressing")

	var svc corev1.Service
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "moving", Name: "edge"}, &svc); err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("service ports = %+v, want 8080 once", svc.Spec.Ports)
	}
	var d appsv1.Deployment
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "moving", Name: "edge"}, &d); err != nil {
		t.Fatal(err)
	}
	if ports := d.Spec.Template.Spec.Containers[0].Ports; len(ports) != 1 {
		t.Errorf("container ports = %+v, want 8080 once", ports)
	}
	for name, host := range map[string]string{"edge-8080": "next.example.com", "edge-8080-2": "www.example.com"} {
		route := getRoute(t, "moving", name)
		if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != host {
			t.Errorf("route %s hostnames = %v, want %s", name, route.Spec.Hostnames, host)
		}
		if pr := route.Spec.ParentRefs[0]; pr.SectionName == nil || string(*pr.SectionName) != ListenerName(host) {
			t.Errorf("route %s parentRef = %+v, want %s's listener", name, pr, host)
		}
	}
	if len(app.Status.URLs) != 2 {
		t.Errorf("urls = %v, want both hostnames", app.Status.URLs)
	}
}

// Apps with ports keep serving for drainSeconds after they are told to stop,
// by the kubelet's own sleep (distroless images have none), and get the usual
// 30 seconds to exit after that. Apps without ports stop at once.
func TestAppDrainsBeforeStopping(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "drain")
	podSpec := func(name string) corev1.PodSpec {
		t.Helper()
		var d appsv1.Deployment
		eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Namespace: "drain", Name: name}, &d) })
		return d.Spec.Template.Spec
	}
	preStopSleep := func(ps corev1.PodSpec) int64 {
		l := ps.Containers[0].Lifecycle
		if l == nil || l.PreStop == nil || l.PreStop.Sleep == nil {
			return 0
		}
		if l.PreStop.Exec != nil || l.PreStop.HTTPGet != nil {
			t.Errorf("preStop = %+v, want only the kubelet's sleep", l.PreStop)
		}
		return l.PreStop.Sleep.Seconds
	}
	grace := func(ps corev1.PodSpec) int64 {
		if ps.TerminationGracePeriodSeconds == nil {
			return 30 // Kubernetes' default
		}
		return *ps.TerminationGracePeriodSeconds
	}

	// The CRD defaults drainSeconds to 5.
	app := createApp(t, "drain", "web", imageApp("nginx:1.29"))
	app = waitForApp(t, app, "Progressing")
	if app.Spec.DrainSeconds == nil || *app.Spec.DrainSeconds != 5 {
		t.Errorf("drainSeconds = %v, want the default 5", app.Spec.DrainSeconds)
	}
	ps := podSpec("web")
	if got := preStopSleep(ps); got != 5 {
		t.Errorf("preStop sleep = %d, want 5", got)
	}
	if got := grace(ps); got != 35 {
		t.Errorf("terminationGracePeriodSeconds = %d, want 35 (drain + 30)", got)
	}

	// Tuned, and switched off.
	app.Spec.DrainSeconds = ptr.To[int32](20)
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if ps := podSpec("web"); preStopSleep(ps) != 20 || grace(ps) != 50 {
			return fmt.Errorf("preStop sleep %d, grace %d; want 20, 50", preStopSleep(ps), grace(ps))
		}
		return nil
	})
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	app.Spec.DrainSeconds = ptr.To[int32](0)
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if ps := podSpec("web"); ps.Containers[0].Lifecycle != nil || grace(ps) != 30 {
			return fmt.Errorf("lifecycle %+v, grace %d; want none, 30", ps.Containers[0].Lifecycle, grace(ps))
		}
		return nil
	})

	// No ports, no Service: nothing to drain from.
	worker := createApp(t, "drain", "worker", kwerftv1.AppSpec{Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "busybox:1.37"}}})
	waitForApp(t, worker, "Progressing")
	if ps := podSpec("worker"); ps.Containers[0].Lifecycle != nil || grace(ps) != 30 {
		t.Errorf("worker: lifecycle %+v, grace %d; want none, 30", ps.Containers[0].Lifecycle, grace(ps))
	}

	// Disks of its own: the StatefulSet drains too.
	db := createApp(t, "drain", "db", kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "postgres:17.6"}},
		Ports:   []kwerftv1.AppPort{{Container: 5432}},
		Volumes: []kwerftv1.AppVolume{{Path: "/data", Size: resource.MustParse("1Gi")}},
	})
	waitForApp(t, db, "Progressing")
	var sts appsv1.StatefulSet
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "drain", Name: "db"}, &sts); err != nil {
		t.Fatal(err)
	}
	if got := preStopSleep(sts.Spec.Template.Spec); got != 5 {
		t.Errorf("statefulset preStop sleep = %d, want 5", got)
	}
}

func TestAppBecomesAvailableWhenReplicasAreReady(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "ready")
	app := createApp(t, "ready", "web", imageApp("nginx:1.29"))
	waitForApp(t, app, "Progressing")

	// Play the deployment controller.
	var d appsv1.Deployment
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "ready", Name: "web"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Status.Replicas, d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas = 2, 2, 2, 2
	if err := k8s.Status().Update(ctx, &d); err != nil {
		t.Fatal(err)
	}
	app = waitForApp(t, app, "Available")
	if app.Status.ReadyReplicas != 2 {
		t.Errorf("readyReplicas = %d, want 2", app.Status.ReadyReplicas)
	}
}

func TestAppNewImageCreatesRevision(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "revs")
	app := createApp(t, "revs", "api", imageApp("ghcr.io/acme/api:1.0.0"))
	app = waitForApp(t, app, "Progressing")

	app.Spec.Source.Image.Ref = "ghcr.io/acme/api:1.1.0"
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	app = waitForApp(t, app, "Progressing")
	if app.Status.Revision != 2 || len(app.Status.History) != 2 {
		t.Fatalf("revision %d, history %d; want 2, 2", app.Status.Revision, len(app.Status.History))
	}
	if app.Status.History[0].Image != "ghcr.io/acme/api:1.1.0" || app.Status.History[1].Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("history = %+v", app.Status.History)
	}
}

func TestAppWithVolumesRunsAsStatefulSet(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "db")
	spec := kwerftv1.AppSpec{
		Source:  kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "postgres:17.6"}},
		Ports:   []kwerftv1.AppPort{{Container: 5432}},
		Volumes: []kwerftv1.AppVolume{{Path: "/var/lib/postgresql/data", Size: resource.MustParse("50Gi"), Class: "hcloud-volume"}},
	}
	app := createApp(t, "db", "postgres", spec)
	waitForApp(t, app, "Progressing")

	var sts appsv1.StatefulSet
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "db", Name: "postgres"}, &sts); err != nil {
		t.Fatal(err)
	}
	vct := sts.Spec.VolumeClaimTemplates
	if len(vct) != 1 || *vct[0].Spec.StorageClassName != "hcloud-volumes" {
		t.Errorf("volumeClaimTemplates = %+v", vct)
	}
	if m := sts.Spec.Template.Spec.Containers[0].VolumeMounts; len(m) != 1 || m[0].MountPath != "/var/lib/postgresql/data" {
		t.Errorf("volumeMounts = %+v", m)
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "db", Name: "postgres"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("stateful app must not have a Deployment (err=%v)", err)
	}
	// No public port → no route.
	var routes gwv1.HTTPRouteList
	if err := k8s.List(ctx, &routes, client.InNamespace("db")); err != nil || len(routes.Items) != 0 {
		t.Errorf("routes = %d (err=%v), want 0", len(routes.Items), err)
	}
}

func TestGitAppWaitsForFirstBuild(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "gitproj")
	app := createApp(t, "gitproj", "invoices", kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: "https://github.com/acme/invoices.git"}},
	})
	waitForApp(t, app, "AwaitingBuild")
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "gitproj", Name: "invoices"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("no workload expected before the first build (err=%v)", err)
	}
}

func TestAppOutsideProjectIsRejected(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}); err != nil {
		t.Fatal(err)
	}
	app := createApp(t, "plain", "stray", imageApp("nginx:1.29"))
	waitForApp(t, app, "NotInProject")
}

func TestAppSpecRejectsBothSources(t *testing.T) {
	requireEnvtest(t)
	app := &kwerftv1.App{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "both"},
		Spec: kwerftv1.AppSpec{Source: kwerftv1.AppSource{
			Image: &kwerftv1.ImageSource{Ref: "nginx"},
			Git:   &kwerftv1.GitSource{Repository: "https://example.com/x.git"},
		}},
	}
	err := k8s.Create(context.Background(), app)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}
