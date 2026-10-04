package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Pods of Apps and Tasks: the replicas table, live logs (api_logs.go), shells
// (api_shell.go) and their recordings (recording.go).
//
// Everything here reaches Kubernetes as the signed-in user, like every write
// in api_workloads.go: pods, logs and pod metrics are sensitive reads (env
// values, application output), and a shell is the most powerful thing the
// console offers. The chart's kwerft:pods-read and kwerft:pods-exec
// ClusterRoles, bound in each project namespace by the Project reconciler,
// decide; the console asks Kubernetes (SelfSubjectAccessReview) only to grey
// out buttons and to refuse early with a clear message.

// podsAPI holds the pod endpoints and what they share.
type podsAPI struct {
	*api
	// backend reaches Kubernetes as a console user. Tests swap it for a fake.
	backend func(p *principal) (podBackend, error)
	logs    logLimits
	shell   shellLimits
	// streams and shells bound concurrent log streams and shell sessions.
	streams *slots
	shells  *slots
	rec     *recorder // nil when no recordings directory is configured

	// running counts streams and sessions, so tests can wait for cleanup.
	running atomic.Int32
}

func (a *api) registerPods(mux *http.ServeMux) {
	p := &podsAPI{
		api:     a,
		logs:    defaultLogLimits,
		shell:   defaultShellLimits,
		streams: newSlots(6, 200),
		shells:  newSlots(3, 30),
	}
	p.backend = p.kubeBackend
	if a.cfg.RecordingsDir != "" {
		p.rec = &recorder{dir: a.cfg.RecordingsDir, retention: recordingRetention, maxBytes: maxRecordingBytes, log: a.cfg.Logger}
	}
	if a.cfg.podsHook != nil {
		a.cfg.podsHook(p)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(p.requireBackend(h)) }

	mux.HandleFunc("GET /api/v1/projects/{project}/apps/{app}/pods", read(p.appPods))
	mux.HandleFunc("GET /api/v1/projects/{project}/tasks/{task}/pods", read(p.taskPods))
	// Log streams are reads, but long-lived ones with application output:
	// refuse them from other sites like a write.
	mux.HandleFunc("GET /api/v1/projects/{project}/apps/{app}/logs", a.sameOrigin(read(p.appLogs)))
	mux.HandleFunc("GET /api/v1/projects/{project}/tasks/{task}/logs", a.sameOrigin(read(p.taskLogs)))
	// WebSocket: Origin first (browsers do not apply SameSite or CORS to the
	// handshake the way they do to fetch), then the session cookie.
	mux.HandleFunc("GET /api/v1/projects/{project}/apps/{app}/pods/{pod}/shell", p.wsOrigin(read(p.appShell)))

	recordings := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireRole(h, "owner", "admin"))
	}
	mux.HandleFunc("GET /api/v1/recordings", recordings(p.recordingList))
	mux.HandleFunc("GET /api/v1/recordings/{id}", recordings(p.recordingGet))
}

func (p *podsAPI) requireBackend(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.cfg.Kube == nil && p.cfg.podsHook == nil {
			writeError(w, http.StatusServiceUnavailable, "This console is not connected to a Kubernetes cluster.")
			return
		}
		next(w, r)
	}
}

// ---- Kubernetes, as the user -------------------------------------------------

// podBackend is everything the pod endpoints ask of Kubernetes. The real one
// impersonates the user; tests use a fake, since envtest has no kubelet to
// serve logs or exec.
type podBackend interface {
	// app and task return a NotFound error when the object does not exist.
	app(ctx context.Context, project, name string) error
	task(ctx context.Context, project, name string) (*kwerftv1.Task, error)
	listPods(ctx context.Context, namespace string, sel labels.Selector) ([]corev1.Pod, error)
	getPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	// podMetrics returns CPU (millicores) and memory (bytes) per pod; an error
	// means metrics are not available (no metrics-server, or no access).
	podMetrics(ctx context.Context, namespace string, sel labels.Selector) (map[string]podUsage, error)
	// allowed asks Kubernetes whether the user may do verb on pods/<sub>.
	allowed(ctx context.Context, namespace, verb, subresource, name string) (bool, error)
	logs(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
	exec(ctx context.Context, namespace, pod string, opts *corev1.PodExecOptions, s execStreams) error
	// addDebugContainer adds an ephemeral container to a running pod.
	addDebugContainer(ctx context.Context, namespace, pod string, ec corev1.EphemeralContainer) error
}

type podUsage struct{ cpuMillis, memoryBytes int64 }

type execStreams struct {
	stdin  io.Reader
	stdout io.Writer
	resize remotecommand.TerminalSizeQueue
}

func (p *podsAPI) kubeBackend(pr *principal) (podBackend, error) {
	email, role := pr.user.Email, pr.user.Role
	c, err := p.cfg.Kube.For(email, role)
	if err != nil {
		return nil, err
	}
	cs, err := p.cfg.Kube.Clientset(email, role)
	if err != nil {
		return nil, err
	}
	cfg, err := p.cfg.Kube.RESTConfig(email, role)
	if err != nil {
		return nil, err
	}
	return &kubePods{c: c, cs: cs, cfg: cfg}, nil
}

type kubePods struct {
	c   client.Client
	cs  kubernetes.Interface
	cfg *rest.Config
}

func (k *kubePods) app(ctx context.Context, project, name string) error {
	return k.c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &kwerftv1.App{})
}

func (k *kubePods) task(ctx context.Context, project, name string) (*kwerftv1.Task, error) {
	var t kwerftv1.Task
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (k *kubePods) listPods(ctx context.Context, namespace string, sel labels.Selector) ([]corev1.Pod, error) {
	list, err := k.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k *kubePods) getPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return k.cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

// podMetrics reads metrics.k8s.io directly: the API is small, and this keeps
// k8s.io/metrics out of the dependencies.
func (k *kubePods) podMetrics(ctx context.Context, namespace string, sel labels.Selector) (map[string]podUsage, error) {
	var list struct {
		Items []struct {
			Metadata   metav1.ObjectMeta `json:"metadata"`
			Containers []struct {
				Usage corev1.ResourceList `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := k.cs.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", namespace, "pods").
		Param("labelSelector", sel.String()).
		Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make(map[string]podUsage, len(list.Items))
	for _, item := range list.Items {
		var u podUsage
		for _, c := range item.Containers {
			if q, ok := c.Usage[corev1.ResourceCPU]; ok {
				u.cpuMillis += q.MilliValue()
			}
			if q, ok := c.Usage[corev1.ResourceMemory]; ok {
				u.memoryBytes += q.Value()
			}
		}
		out[item.Metadata.Name] = u
	}
	return out, nil
}

func (k *kubePods) allowed(ctx context.Context, namespace, verb, subresource, name string) (bool, error) {
	review, err := k.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace, Verb: verb, Resource: "pods", Subresource: subresource, Name: name,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return review.Status.Allowed, nil
}

func (k *kubePods) logs(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	return k.cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

func (k *kubePods) addDebugContainer(ctx context.Context, namespace, name string, ec corev1.EphemeralContainer) error {
	pods := k.cs.CoreV1().Pods(namespace)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, ec)
	_, err = pods.UpdateEphemeralContainers(ctx, name, pod, metav1.UpdateOptions{FieldManager: "kwerft"})
	return err
}

// exec runs a command in a container over the WebSocket protocol (v5),
// falling back to SPDY for API servers or proxies that cannot upgrade to it,
// as kubectl does. Both carry the user's Impersonate-* headers.
func (k *kubePods) exec(ctx context.Context, namespace, pod string, opts *corev1.PodExecOptions, s execStreams) error {
	u := k.cs.CoreV1().RESTClient().Post().
		Namespace(namespace).Resource("pods").Name(pod).SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec).URL()
	ws, err := remotecommand.NewWebSocketExecutor(k.cfg, "GET", u.String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(k.cfg, "POST", u)
	if err != nil {
		return err
	}
	ex, err := remotecommand.NewFallbackExecutor(ws, spdy, shouldFallBack)
	if err != nil {
		return err
	}
	return ex.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: s.stdin, Stdout: s.stdout, Tty: true, TerminalSizeQueue: s.resize,
	})
}

// ---- replicas ------------------------------------------------------------------

type containerJSON struct {
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	State    string `json:"state"` // running | waiting | terminated
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	Restarts int32  `json:"restarts"`
	// LastTermination is why the previous instance ended (after a crash its
	// logs are available with previous=1).
	LastTermination *terminationJSON `json:"lastTermination,omitempty"`
}

type terminationJSON struct {
	Reason     string    `json:"reason"`
	ExitCode   int32     `json:"exitCode"`
	FinishedAt time.Time `json:"finishedAt"`
}

type podJSON struct {
	Name  string `json:"name"`
	Node  string `json:"node"`
	Phase string `json:"phase"` // Kubernetes pod phase
	// Status is what `kubectl get pods` would show: Running,
	// CrashLoopBackOff, ContainerCreating, Terminating, Completed, ...
	Status     string          `json:"status"`
	Tone       string          `json:"tone"` // ok | warn | bad | mute, for the pill
	Message    string          `json:"message,omitempty"`
	Ready      bool            `json:"ready"`
	Restarts   int32           `json:"restarts"`
	Created    time.Time       `json:"created"`
	CPUMillis  *int64          `json:"cpuMillis"`   // null without metrics
	Memory     *int64          `json:"memoryBytes"` // null without metrics
	Containers []containerJSON `json:"containers"`
}

type podsJSON struct {
	Pods []podJSON `json:"pods"`
	// Metrics is false when metrics.k8s.io did not answer (no metrics-server).
	Metrics bool `json:"metrics"`
	// Access is what Kubernetes RBAC lets the user do with these pods. The
	// console greys out buttons with it; the endpoints enforce it anyway.
	Access struct {
		Logs bool `json:"logs"`
		Exec bool `json:"exec"`
	} `json:"access"`
}

func (p *podsAPI) appPods(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	p.listTargetPods(w, r, "app", project, name, appSelector(name))
}

func (p *podsAPI) taskPods(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("task")
	p.listTargetPods(w, r, "task", project, name, taskSelector(name))
}

func appSelector(app string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{controllers.LabelApp: app})
}

func taskSelector(task string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{controllers.LabelTask: task})
}

// target checks that the App or Task exists (and that the user may read it).
func (p *podsAPI) target(ctx context.Context, b podBackend, kind, project, name string) (*kwerftv1.Task, error) {
	if kind == "task" {
		return b.task(ctx, project, name)
	}
	return nil, b.app(ctx, project, name)
}

func targetNotFound(kind, project, name string) string {
	if kind == "task" {
		return fmt.Sprintf("Task %q in project %q not found.", name, project)
	}
	return appNotFound(project, name)
}

func (p *podsAPI) listTargetPods(w http.ResponseWriter, r *http.Request, kind, project, name string, sel labels.Selector) {
	pr := r.Context().Value(ctxKey{}).(*principal)
	b, err := p.backend(pr)
	if err != nil {
		p.internalError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	target := appTarget(project, name)
	if _, err := p.target(ctx, b, kind, project, name); err != nil {
		p.kubeError(w, r, pr, kind+".pods", target, targetNotFound(kind, project, name), err)
		return
	}
	pods, err := b.listPods(ctx, project, sel)
	if err != nil {
		p.kubeError(w, r, pr, kind+".pods", target, targetNotFound(kind, project, name), err)
		return
	}
	out := podsJSON{Pods: make([]podJSON, 0, len(pods))}
	usage, merr := b.podMetrics(ctx, project, sel)
	out.Metrics = merr == nil
	now := p.now()
	for i := range pods {
		pj := podSummary(&pods[i], now)
		if u, ok := usage[pj.Name]; ok {
			pj.CPUMillis, pj.Memory = &u.cpuMillis, &u.memoryBytes
		}
		out.Pods = append(out.Pods, pj)
	}
	slices.SortFunc(out.Pods, func(x, y podJSON) int {
		if c := x.Created.Compare(y.Created); c != 0 {
			return c
		}
		return strings.Compare(x.Name, y.Name)
	})
	// Errors here only grey out a button; the endpoints ask Kubernetes again.
	out.Access.Logs, _ = b.allowed(ctx, project, "get", "log", "")
	out.Access.Exec, _ = b.allowed(ctx, project, "create", "exec", "")
	writeJSON(w, http.StatusOK, out)
}

// podSummary condenses a pod the way `kubectl get pods` does.
func podSummary(pod *corev1.Pod, now time.Time) podJSON {
	out := podJSON{
		Name: pod.Name, Node: pod.Spec.NodeName, Phase: string(pod.Status.Phase),
		Status: string(pod.Status.Phase), Created: pod.CreationTimestamp.UTC(),
		Containers: make([]containerJSON, 0, len(pod.Spec.Containers)),
	}
	if out.Status == "" {
		out.Status = "Pending"
	}
	if pod.Status.Reason != "" { // Evicted, NodeLost, ...
		out.Status, out.Message = pod.Status.Reason, pod.Status.Message
	}
	statuses := map[string]corev1.ContainerStatus{}
	for _, cs := range pod.Status.ContainerStatuses {
		statuses[cs.Name] = cs
	}
	ready, worst := 0, ""
	for _, c := range pod.Spec.Containers {
		cj := containerJSON{Name: c.Name, State: "waiting", Reason: "Pending"}
		if cs, ok := statuses[c.Name]; ok {
			cj = containerSummary(cs)
		}
		if cj.Ready {
			ready++
		}
		out.Restarts += cj.Restarts
		// A waiting or failed container explains the pod better than its phase.
		switch {
		case cj.State == "waiting" && cj.Reason != "" && worst == "":
			worst, out.Message = cj.Reason, cj.Message
		case cj.State == "terminated" && cj.Reason != "" && cj.Reason != "Completed" && worst == "":
			worst, out.Message = cj.Reason, cj.Message
		case cj.State == "terminated" && cj.Reason == "Completed" && pod.Status.Phase == corev1.PodSucceeded:
			worst = "Completed"
		}
		out.Containers = append(out.Containers, cj)
	}
	out.Ready = len(pod.Spec.Containers) > 0 && ready == len(pod.Spec.Containers) && pod.Status.Phase == corev1.PodRunning
	if worst != "" && worst != "Pending" {
		out.Status = worst
	}
	if pod.DeletionTimestamp != nil {
		out.Status = "Terminating"
	}
	out.Tone = podTone(out, pod)
	return out
}

func containerSummary(cs corev1.ContainerStatus) containerJSON {
	cj := containerJSON{Name: cs.Name, Ready: cs.Ready, Restarts: cs.RestartCount}
	switch s := cs.State; {
	case s.Running != nil:
		cj.State = "running"
	case s.Terminated != nil:
		cj.State, cj.Reason, cj.Message = "terminated", s.Terminated.Reason, s.Terminated.Message
		if cj.Reason == "" {
			cj.Reason = fmt.Sprintf("ExitCode:%d", s.Terminated.ExitCode)
		}
	case s.Waiting != nil:
		cj.State, cj.Reason, cj.Message = "waiting", s.Waiting.Reason, s.Waiting.Message
	default:
		cj.State = "waiting"
	}
	if t := cs.LastTerminationState.Terminated; t != nil {
		cj.LastTermination = &terminationJSON{Reason: t.Reason, ExitCode: t.ExitCode, FinishedAt: t.FinishedAt.UTC()}
	}
	return cj
}

func podTone(pj podJSON, pod *corev1.Pod) string {
	switch {
	case pj.Status == "Terminating" || pj.Status == "Completed":
		return "mute"
	case pod.Status.Phase == corev1.PodFailed, strings.Contains(pj.Status, "BackOff"), strings.HasPrefix(pj.Status, "Err"),
		pj.Status == "OOMKilled", pj.Status == "Error", pj.Status == "Evicted", strings.HasPrefix(pj.Status, "ExitCode"),
		strings.HasPrefix(pj.Status, "CreateContainer"), strings.HasPrefix(pj.Status, "Invalid"):
		return "bad"
	case pj.Ready:
		return "ok"
	default:
		return "warn"
	}
}

// ---- shared helpers ----------------------------------------------------------

// slots bounds concurrent long-lived connections per user and in total, so
// one browser (or script) cannot exhaust the console or the API server.
type slots struct {
	mu       sync.Mutex
	perUser  map[string]int
	total    int
	maxUser  int
	maxTotal int
}

func newSlots(maxUser, maxTotal int) *slots {
	return &slots{perUser: map[string]int{}, maxUser: maxUser, maxTotal: maxTotal}
}

// acquire returns a release func, or nil when the limit is reached.
func (s *slots) acquire(user string) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perUser[user] >= s.maxUser || s.total >= s.maxTotal {
		return nil
	}
	s.perUser[user]++
	s.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.perUser[user]--
			s.total--
			if s.perUser[user] == 0 {
				delete(s.perUser, user)
			}
		})
	}
}

// sessionAlive reports whether the user's console session still exists, so
// long-lived streams end when someone signs out or is signed out.
func (p *podsAPI) sessionAlive(ctx context.Context, pr *principal) bool {
	_, _, err := p.store.SessionByHash(ctx, pr.idHash, p.now())
	return err == nil
}
