package upgrades

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// KubeCluster is the runner's Cluster on the Kubernetes API, with the
// runner's service account (charts/kwerft templates/upgrade-runner.yaml).
type KubeCluster struct {
	Client    client.Client
	Name      string // the Upgrade
	Namespace string // kwerft-system: the console, the log ConfigMap

	// HTTP is used for the console checks; nil makes one with a 10 s
	// timeout.
	HTTP *http.Client
	// ConsoleServiceURL overrides http://<console Service ClusterIP> (tests).
	ConsoleServiceURL string
	// PublicURL overrides https://<console domain> (tests); HTTP's
	// transport's RootCAs are trusted for it.
	PublicURL string
}

// Names the verification looks at (the chart's and the installer's).
const (
	consoleDeployment = "kwerft"
	consoleService    = "kwerft"
	nodeAgentDS       = "kwerft-node-agent"
	hubbleNamespace   = "kube-system"
	hubbleRelay       = "hubble-relay"
	acmeIssuer        = "letsencrypt"
)

func (k *KubeCluster) Upgrade(ctx context.Context) (*kwerftv1.Upgrade, error) {
	var u kwerftv1.Upgrade
	if err := k.Client.Get(ctx, client.ObjectKey{Name: k.Name}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (k *KubeCluster) UpdateStatus(ctx context.Context, mutate func(*kwerftv1.Upgrade) error) (*kwerftv1.Upgrade, error) {
	var out *kwerftv1.Upgrade
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		u, err := k.Upgrade(ctx)
		if err != nil {
			return err
		}
		if err := mutate(u); err != nil {
			return err
		}
		if err := k.Client.Status().Update(ctx, u); err != nil {
			return err
		}
		out = u
		return nil
	})
	return out, err
}

func (k *KubeCluster) Annotate(ctx context.Context, key, value string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: value}}})
	return k.Client.Patch(ctx, &kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: k.Name}}, client.RawPatch(types.MergePatchType, patch))
}

func (k *KubeCluster) AppReplicas(ctx context.Context) (map[string]int32, error) {
	var apps kwerftv1.AppList
	if err := k.Client.List(ctx, &apps); err != nil {
		if meta.IsNoMatchError(err) {
			return map[string]int32{}, nil
		}
		return nil, err
	}
	out := map[string]int32{}
	for _, a := range apps.Items {
		out[a.Namespace+"/"+a.Name] = a.Status.ReadyReplicas
	}
	return out, nil
}

func (k *KubeCluster) WriteLog(ctx context.Context, data []byte) error {
	var u kwerftv1.Upgrade
	if err := k.Client.Get(ctx, client.ObjectKey{Name: k.Name}, &u); err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: LogConfigMapName(k.Name), Namespace: k.Namespace}}
	desired := map[string]string{LogKey: strings.ToValidUTF8(string(data), "?")}
	err := k.Client.Get(ctx, client.ObjectKeyFromObject(cm), cm)
	switch {
	case apierrors.IsNotFound(err):
		cm.Labels = map[string]string{LabelUpgrade: k.Name}
		cm.OwnerReferences = []metav1.OwnerReference{upgradeOwner(&u)}
		cm.Data = desired
		return k.Client.Create(ctx, cm)
	case err != nil:
		return err
	}
	cm.Data = desired
	return k.Client.Update(ctx, cm)
}

// upgradeOwner makes the Upgrade own an object, so deleting it removes the
// object (kwerft.dev Upgrades are cluster-scoped, which namespaced
// dependents may name).
func upgradeOwner(u *kwerftv1.Upgrade) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: kwerftv1.GroupVersion.String(), Kind: "Upgrade", Name: u.Name, UID: u.UID}
}

// UpgradeOwner is upgradeOwner for the controller.
func UpgradeOwner(u *kwerftv1.Upgrade) metav1.OwnerReference { return upgradeOwner(u) }

func (k *KubeCluster) http() *http.Client {
	if k.HTTP != nil {
		return k.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Verify checks what docs/phase6-upgrades.md › Kwerft upgrade › Verify
// lists. Each problem is one short sentence.
func (k *KubeCluster) Verify(ctx context.Context, t VerifyTarget) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	console := k.consoleMode(ctx)
	if p := k.deploymentReady(ctx, k.Namespace, consoleDeployment); p != "" {
		add("the console %s", p)
	} else if v, err := k.consoleVersion(ctx, console); err != nil {
		add("the console's version: %v", err)
	} else if strings.TrimPrefix(v, "v") != strings.TrimPrefix(t.Version, "v") {
		add("the console reports %s, not %s", v, t.Version)
	}
	if p := k.crdsEstablished(ctx); p != "" {
		add("%s", p)
	}
	var ds appsv1.DaemonSet
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: nodeAgentDS}, &ds); err == nil {
		s := ds.Status
		if s.ObservedGeneration < ds.Generation || s.UpdatedNumberScheduled < s.DesiredNumberScheduled || s.NumberAvailable < s.DesiredNumberScheduled {
			add("the node agent is rolling out (%d of %d updated and available)", min(s.UpdatedNumberScheduled, s.NumberAvailable), s.DesiredNumberScheduled)
		}
	} else if !apierrors.IsNotFound(err) {
		add("the node agent: %v", err)
	}
	if console {
		if p := k.publicCheck(ctx); p != "" {
			add("%s", p)
		}
	}
	if p := k.appsBack(ctx, t.Apps); p != "" {
		add("%s", p)
	}
	if p := k.deploymentReady(ctx, hubbleNamespace, hubbleRelay); p != "" && !strings.HasPrefix(p, "is missing") {
		add("the Hubble relay %s", p)
	}
	return problems
}

// consoleMode: a console (not an agent) runs here; agents have no Service.
func (k *KubeCluster) consoleMode(ctx context.Context) bool {
	var svc corev1.Service
	return k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: consoleService}, &svc) == nil
}

// deploymentReady returns "" when ready, otherwise what is wrong.
func (k *KubeCluster) deploymentReady(ctx context.Context, ns, name string) string {
	var d appsv1.Deployment
	if err := k.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
		if apierrors.IsNotFound(err) {
			return "is missing"
		}
		return err.Error()
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	if s.ObservedGeneration < d.Generation || s.UpdatedReplicas < want || s.AvailableReplicas < want || s.Replicas > want {
		return fmt.Sprintf("is not ready (%d of %d updated and available)", min(s.UpdatedReplicas, s.AvailableReplicas), want)
	}
	return ""
}

// consoleVersion asks the console (GET /api/v1/version through its
// Service), or reads an agent's image tag.
func (k *KubeCluster) consoleVersion(ctx context.Context, console bool) (string, error) {
	if !console {
		var d appsv1.Deployment
		if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: consoleDeployment}, &d); err != nil {
			return "", err
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "kwerft" {
				_, _, tag, err := SplitRef(c.Image)
				return tag, err
			}
		}
		return "", fmt.Errorf("no kwerft container")
	}
	base := k.ConsoleServiceURL
	if base == "" {
		var svc corev1.Service
		if err := k.Client.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: consoleService}, &svc); err != nil {
			return "", err
		}
		port := int32(80)
		if len(svc.Spec.Ports) > 0 {
			port = svc.Spec.Ports[0].Port
		}
		base = "http://" + net.JoinHostPort(svc.Spec.ClusterIP, fmt.Sprint(port))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := k.http().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil {
		return "", err
	}
	return v.Version, nil
}

func (k *KubeCluster) crdsEstablished(ctx context.Context) string {
	var crds apiextensionsv1.CustomResourceDefinitionList
	if err := k.Client.List(ctx, &crds); err != nil {
		return "CRDs: " + err.Error()
	}
	var waiting []string
	for _, c := range crds.Items {
		if c.Spec.Group != kwerftv1.GroupVersion.Group {
			continue
		}
		established := false
		for _, cond := range c.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
		}
		if !established {
			waiting = append(waiting, c.Name)
		}
	}
	if len(waiting) > 0 {
		return "CRDs not established: " + strings.Join(waiting, ", ")
	}
	return ""
}

// publicCheck: https://<console domain>/ answers with a valid certificate
// (any status). With Let's Encrypt staging (e2e) the chain is not checked.
func (k *KubeCluster) publicCheck(ctx context.Context) string {
	target := k.PublicURL
	verifyChain := true
	if target == "" {
		var s kwerftv1.ConsoleSettings
		if err := k.Client.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return "console settings: " + err.Error()
		}
		host := s.Status.ConsoleDomain
		if host == "" {
			host = s.Spec.ConsoleDomain
		}
		if host == "" {
			return "" // nothing to check
		}
		target = "https://" + host + "/"
		verifyChain = !k.stagingIssuer(ctx)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if !verifyChain {
		// Hostname and validity are checked; the staging CA is not trusted.
		tr.TLSClientConfig.InsecureSkipVerify = true
		tr.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("no certificate")
			}
			leaf := cs.PeerCertificates[0]
			if now := time.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return fmt.Errorf("certificate not valid now")
			}
			return leaf.VerifyHostname(cs.ServerName)
		}
	}
	if k.HTTP != nil && k.HTTP.Transport != nil {
		if t, ok := k.HTTP.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
			tr.TLSClientConfig.RootCAs = t.TLSClientConfig.RootCAs
		}
	}
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err.Error()
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Sprintf("%s does not answer with a valid certificate: %v", target, err)
	}
	_ = resp.Body.Close()
	return ""
}

// stagingIssuer: the installer's ClusterIssuer uses Let's Encrypt staging
// (install.sh --acme-server staging).
func (k *KubeCluster) stagingIssuer(ctx context.Context) bool {
	ci := &unstructured.Unstructured{}
	ci.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"})
	if err := k.Client.Get(ctx, client.ObjectKey{Name: acmeIssuer}, ci); err != nil {
		return false
	}
	server, _, _ := unstructured.NestedString(ci.Object, "spec", "acme", "server")
	return strings.Contains(server, "staging")
}

func (k *KubeCluster) appsBack(ctx context.Context, before map[string]int32) string {
	if len(before) == 0 {
		return ""
	}
	now, err := k.AppReplicas(ctx)
	if err != nil {
		return "Apps: " + err.Error()
	}
	return AppsBehind(before, now)
}

// AppsBehind names the Apps with fewer ready replicas now than before
// ("" when none); Apps that are gone are not counted.
func AppsBehind(before, now map[string]int32) string {
	var behind []string
	for app, want := range before {
		got, ok := now[app]
		if ok && got < want {
			behind = append(behind, fmt.Sprintf("%s (%d of %d ready)", app, got, want))
		}
	}
	if len(behind) == 0 {
		return ""
	}
	slices.Sort(behind)
	if len(behind) > 5 {
		behind = append(behind[:5], fmt.Sprintf("and %d more", len(behind)-5))
	}
	return "Apps with fewer ready replicas than before: " + strings.Join(behind, ", ")
}
