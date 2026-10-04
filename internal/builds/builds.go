// Package builds holds what the Build reconciler, the webhook handler and
// the console share about Git builds: where builds run, where images go, and
// how a Build is created.
package builds

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// Namespace runs every build Job and holds the GitConnection
	// credentials. Apps' projects cannot see into it; the console shows
	// build logs after checking the user may read the Build.
	Namespace = "kwerft-builds"

	// RegistryHost is the in-cluster registry (zot) as image references name
	// it. Nodes' containerd resolves it through a mirror (k3s
	// registries.yaml); build pods push to it through BuildKit's registry
	// config. Plain HTTP inside the cluster.
	RegistryHost = "registry.kwerft.internal:5000"
	// RegistryService is zot's Service in RegistryNamespace, on RegistryPort.
	RegistryService   = "kwerft-registry"
	RegistryNamespace = "kwerft-system"
	RegistryPort      = 5000

	// LabelBuild marks a build Job and its pod with the Build's name; the
	// Build's namespace is in LabelProject (controllers.LabelProject).
	LabelBuild = "kwerft.dev/build"

	// SecretPrefix + connection name is a GitConnection's credentials Secret
	// in Namespace.
	SecretPrefix = "git-"

	// Keys of the credentials Secret.
	KeyToken          = "token"
	KeySSHPrivateKey  = "ssh-privatekey"
	KeyKnownHosts     = "known_hosts"
	KeyGitHubAppKey   = "github-app-private-key"
	KeyWebhookSecret  = "webhook-secret"
	labelApp          = "kwerft.dev/app"
	shortCommitLength = 12
)

// CredentialsSecret is the Secret name for a GitConnection.
func CredentialsSecret(connection string) string { return SecretPrefix + connection }

// ImageRepository is where an App's builds are pushed:
// registry.kwerft.internal:5000/<project>/<app>.
func ImageRepository(project, app string) string {
	return RegistryHost + "/" + project + "/" + app
}

// ImageRef tags one build's image with its commit.
func ImageRef(project, app, commit string) string {
	return ImageRepository(project, app) + ":" + Short(commit)
}

// CacheRef is the App's BuildKit layer cache, kept in the registry.
func CacheRef(project, app string) string {
	return ImageRepository(project, app) + ":buildcache"
}

// Short is the abbreviated commit used in tags and the UI.
func Short(commit string) string {
	if len(commit) > shortCommitLength {
		return commit[:shortCommitLength]
	}
	return commit
}

// Commit is what a trigger knows about the commit to build.
type Commit struct {
	SHA, Branch, Message, Author string
}

// Request is everything New needs besides the App.
type Request struct {
	Commit      Commit
	Trigger     string // push | manual | pull-request
	RequestedBy string
	PullRequest int64
	// Deploy rolls the App out on success; pull-request builds never do.
	Deploy bool
}

// New returns a Build for app (which must have a Git source), ready to be
// created in the App's namespace. The name is generated: <app>-<commit>-xxxxx.
func New(app *kwerftv1.App, req Request) (*kwerftv1.Build, error) {
	git := app.Spec.Source.Git
	if git == nil {
		return nil, fmt.Errorf("app %s has no Git source", app.Name)
	}
	if req.Trigger == "pull-request" {
		req.Deploy = false
	}
	msg := req.Commit.Message
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i] // the subject line
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	prefix := app.Name
	if len(prefix) > 40 {
		prefix = strings.TrimRight(prefix[:40], "-")
	}
	return &kwerftv1.Build{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: prefix + "-" + Short(req.Commit.SHA)[:7] + "-",
			Namespace:    app.Namespace,
			Labels:       map[string]string{labelApp: app.Name},
		},
		Spec: kwerftv1.BuildSpec{
			App: app.Name,
			Source: kwerftv1.BuildSource{
				Repository: git.Repository,
				Path:       git.Path,
				Builder:    orDefault(git.Builder, "dockerfile"),
				Dockerfile: git.Dockerfile,
				Connection: git.Connection,
			},
			Commit:      req.Commit.SHA,
			Branch:      req.Commit.Branch,
			Message:     msg,
			Author:      req.Commit.Author,
			Trigger:     req.Trigger,
			RequestedBy: req.RequestedBy,
			PullRequest: req.PullRequest,
			Deploy:      req.Deploy,
		},
	}, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
