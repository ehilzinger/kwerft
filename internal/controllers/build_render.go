package controllers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

const (
	// DefaultBuildKitImage is rootless BuildKit; its buildctl-daemonless.sh
	// runs buildkitd inside the build container, so builds need no daemon.
	// The clone init container uses its git and ssh as well.
	DefaultBuildKitImage = "docker.io/moby/buildkit:v0.33.1-rootless"
	// DefaultRailpackImage holds the railpack CLI (for `railpack prepare`)
	// and is the BuildKit frontend that turns the plan into an image, so
	// plan and frontend always have the same version.
	DefaultRailpackImage = "ghcr.io/railwayapp/railpack-frontend:v0.40.1"
	// DefaultBuildTimeout ends a build that runs longer (activeDeadlineSeconds).
	DefaultBuildTimeout = 30 * time.Minute

	// buildLogTTL keeps a finished build's Job and pod, and with them the
	// build log the console streams, for a day. The kubelet removes the pod's
	// emptyDir volumes when it terminates, so a kept pod holds no disk.
	buildLogTTL = 24 * 60 * 60 // seconds

	// LabelGitToken marks the short-lived Secret holding a GitHub App
	// installation token for one build's clone.
	LabelGitToken = "kwerft.dev/git-token"

	// The rootless BuildKit image runs as this user (it cannot change).
	buildUID = 1000

	registryHostAlias = "registry.kwerft.internal"

	// Build container resources, within the kwerft-builds LimitRange (at
	// most 6Gi per container) and quota.
	buildCPURequest    = "500m"
	buildMemoryRequest = "1Gi"
	buildCPULimit      = "2"
	buildMemoryLimit   = "3Gi"
	// Scratch space: the checkout, and BuildKit's state (base images,
	// layers, the build's own cache). Past these the pod is evicted.
	workspaceSize     = "4Gi"
	buildkitStateSize = "20Gi"
)

// buildRun is a Build resolved for its Job: everything decided once, when the
// build starts.
type buildRun struct {
	build         *kwerftv1.Build
	project       string // the Build's namespace
	jobName       string
	image         string // builds.ImageRef
	cache         string // builds.CacheRef
	buildkitImage string
	railpackImage string
	registryIP    string
	timeout       time.Duration

	contextDir string // relative to the repository root, "" for the root
	dockerfile string // relative to contextDir

	// Clone credentials: auth is none, token or ssh. A token comes from the
	// connection's Secret (token auth) or from a per-build Secret holding a
	// GitHub App installation token.
	auth        string
	username    string
	credentials string // Secret in builds.Namespace
}

// buildJobName names a build's Job in kwerft-builds, unique across projects
// and short enough for the job-name label pods get (63).
func buildJobName(namespace, build string) string {
	name := namespace + "-" + build
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(namespace + "/" + build))
	return strings.TrimRight(name[:52], "-") + "-" + hex.EncodeToString(sum[:])[:10]
}

// tokenSecretName is the per-build Secret for a GitHub App token.
func tokenSecretName(job string) string { return job + "-git" }

func buildLabels(b *kwerftv1.Build) map[string]string {
	return map[string]string{
		builds.LabelBuild: b.Name,
		LabelProject:      b.Namespace,
		LabelManagedBy:    ManagedByKwerft,
	}
}

// cleanRepoPath makes a user-given path relative to the repository root
// without leaving it ("../x" stays inside). The build script checks again
// after resolving symlinks.
func cleanRepoPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

var scpLikeRepo = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^/\s-][^\s]*$`)

// repoTransport classifies a repository URL: "https", "ssh", or "" for
// anything else (file://, ext::, git://, options in disguise), which builds
// refuse.
func repoTransport(repo string) string {
	switch {
	case strings.HasPrefix(repo, "https://"):
		if u, err := url.Parse(repo); err == nil && u.Host != "" && u.User == nil {
			return "https"
		}
	case strings.HasPrefix(repo, "ssh://"):
		if u, err := url.Parse(repo); err == nil && u.Host != "" {
			return "ssh"
		}
	case scpLikeRepo.MatchString(repo):
		return "ssh"
	}
	return ""
}

// repoHost is the host part of an HTTPS repository URL, lower-case.
func repoHost(repo string) string {
	u, err := url.Parse(repo)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// repoOwner is the first path segment of an HTTPS repository URL.
func repoOwner(repo string) string {
	u, err := url.Parse(repo)
	if err != nil {
		return ""
	}
	owner, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	return owner
}

// gitUsername is the HTTP user name a provider expects next to a token.
func gitUsername(p kwerftv1.GitProvider) string {
	switch p {
	case kwerftv1.GitHub:
		return "x-access-token"
	case kwerftv1.GitLab:
		return "oauth2"
	default:
		return "kwerft" // Gitea and Forgejo accept any user name with a token
	}
}

func (r *buildRun) builder() string {
	if r.build.Spec.Source.Builder == "railpack" {
		return "railpack"
	}
	return "dockerfile"
}

func (r *buildRun) job() *batchv1ac.JobApplyConfiguration {
	labels := buildLabels(r.build)
	spec := corev1ac.PodSpec().
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithPriorityClassName(BatchPriorityClass).
		WithAutomountServiceAccountToken(false).
		WithEnableServiceLinks(false).
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithRunAsNonRoot(true).
			WithRunAsUser(buildUID).
			WithRunAsGroup(buildUID).
			WithFSGroup(buildUID).
			WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault))).
		WithHostAliases(corev1ac.HostAlias().WithIP(r.registryIP).WithHostnames(registryHostAlias)).
		WithInitContainers(r.cloneContainer()).
		WithContainers(r.buildContainer()).
		WithVolumes(
			emptyDir("workspace", workspaceSize),
			// The clone's home: git config and a copy of the SSH key, in memory.
			corev1ac.Volume().WithName("scratch").WithEmptyDir(corev1ac.EmptyDirVolumeSource().
				WithMedium(corev1.StorageMediumMemory).WithSizeLimit(resource.MustParse("16Mi"))),
			emptyDir("buildkit", buildkitStateSize),
		)
	if r.builder() == "railpack" {
		spec.WithInitContainers(r.prepareContainer())
		spec.WithVolumes(emptyDir("tmp", "1Gi"))
	}
	if r.auth != "none" {
		var items []*corev1ac.KeyToPathApplyConfiguration
		switch r.auth {
		case "token":
			items = append(items, corev1ac.KeyToPath().WithKey(builds.KeyToken).WithPath("token"))
		case "ssh":
			items = append(items,
				corev1ac.KeyToPath().WithKey(builds.KeySSHPrivateKey).WithPath("ssh-privatekey"),
				corev1ac.KeyToPath().WithKey(builds.KeyKnownHosts).WithPath("known_hosts"))
		}
		// Only the keys the clone needs: never the webhook secret or a
		// GitHub App's private key.
		spec.WithVolumes(corev1ac.Volume().WithName("credentials").WithSecret(corev1ac.SecretVolumeSource().
			WithSecretName(r.credentials).WithItems(items...).WithDefaultMode(0o440).
			// A missing key fails the clone with a message instead of
			// leaving the pod unable to start.
			WithOptional(true)))
	}

	return batchv1ac.Job(r.jobName, builds.Namespace).
		WithLabels(labels).
		WithAnnotations(map[string]string{"kwerft.dev/commit": r.build.Spec.Commit}).
		WithSpec(batchv1ac.JobSpec().
			WithBackoffLimit(0).
			WithActiveDeadlineSeconds(max(1, int64(math.Ceil(r.timeout.Seconds())))).
			WithTTLSecondsAfterFinished(buildLogTTL).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(labels).
				// The build log: kubectl logs and the console read this one.
				WithAnnotations(map[string]string{"kubectl.kubernetes.io/default-container": builds.ContainerBuild}).
				WithSpec(spec)))
}

// emptyDir is a scratch volume; past its size limit the kubelet evicts the
// pod, so one build cannot fill the node's disk.
func emptyDir(name, size string) *corev1ac.VolumeApplyConfiguration {
	return corev1ac.Volume().WithName(name).WithEmptyDir(corev1ac.EmptyDirVolumeSource().WithSizeLimit(resource.MustParse(size)))
}

func env(name, value string) *corev1ac.EnvVarApplyConfiguration {
	return corev1ac.EnvVar().WithName(name).WithValue(value)
}

// requirements sets requests and limits on every container, as the
// kwerft-builds quota requires. Unlike Apps, builds get a CPU limit: a
// build must not starve the node's services.
func requirements(cpuReq, memReq, cpuLimit, memLimit string) *corev1ac.ResourceRequirementsApplyConfiguration {
	return corev1ac.ResourceRequirements().
		WithRequests(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuReq), corev1.ResourceMemory: resource.MustParse(memReq)}).
		WithLimits(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuLimit), corev1.ResourceMemory: resource.MustParse(memLimit)})
}

// lockedDown is the security context of the helper containers: nothing
// beyond reading and writing the shared volumes.
func lockedDown() *corev1ac.SecurityContextApplyConfiguration {
	return corev1ac.SecurityContext().
		WithAllowPrivilegeEscalation(false).
		WithReadOnlyRootFilesystem(true).
		WithCapabilities(corev1ac.Capabilities().WithDrop(corev1.Capability("ALL")))
}

// cloneContainer fetches exactly the commit into /workspace/src. It is the
// only container that sees credentials; the build container, where the
// repository's own code runs, never mounts them.
func (r *buildRun) cloneContainer() *corev1ac.ContainerApplyConfiguration {
	c := corev1ac.Container().
		WithName(builds.ContainerClone).
		WithImage(r.buildkitImage).
		WithCommand("sh", "-c", cloneScript).
		WithEnv(
			env("KWERFT_REPOSITORY", r.build.Spec.Source.Repository),
			env("KWERFT_COMMIT", r.build.Spec.Commit),
			env("KWERFT_GIT_AUTH", r.auth),
			env("KWERFT_GIT_USERNAME", r.username),
		).
		WithResources(requirements("100m", "128Mi", "1", "1Gi")).
		WithSecurityContext(lockedDown()).
		WithTerminationMessagePolicy(corev1.TerminationMessageFallbackToLogsOnError).
		WithVolumeMounts(
			corev1ac.VolumeMount().WithName("workspace").WithMountPath("/workspace"),
			corev1ac.VolumeMount().WithName("scratch").WithMountPath("/scratch"),
		)
	if r.auth != "none" {
		c.WithVolumeMounts(corev1ac.VolumeMount().WithName("credentials").WithMountPath("/credentials").WithReadOnly(true))
	}
	return c
}

// prepareContainer runs `railpack prepare`, which writes the build plan the
// Railpack frontend turns into an image. Like the clone, it never fails
// itself; the build container reports its failure.
func (r *buildRun) prepareContainer() *corev1ac.ContainerApplyConfiguration {
	return corev1ac.Container().
		WithName(builds.ContainerPrepare).
		WithImage(r.railpackImage).
		WithCommand("sh", "-c", prepareScript, "prepare", path.Join("/workspace/src", r.contextDir)).
		WithEnv(env("HOME", "/tmp"), env("NO_COLOR", "1")).
		WithResources(requirements("100m", "128Mi", "1", "1Gi")).
		WithSecurityContext(lockedDown()).
		WithTerminationMessagePolicy(corev1.TerminationMessageFallbackToLogsOnError).
		WithVolumeMounts(
			corev1ac.VolumeMount().WithName("workspace").WithMountPath("/workspace"),
			corev1ac.VolumeMount().WithName("tmp").WithMountPath("/tmp"),
		)
}

// buildContainer runs rootless BuildKit. It needs seccomp and AppArmor
// unconfined (to create user namespaces and mounts) and may escalate
// privileges (newuidmap is setuid); kwerft-builds is the only namespace that
// allows this. The workspace is read-only here.
func (r *buildRun) buildContainer() *corev1ac.ContainerApplyConfiguration {
	return corev1ac.Container().
		WithName(builds.ContainerBuild).
		WithImage(r.buildkitImage).
		WithCommand("sh", "-c", buildScript).
		WithEnv(
			env("KWERFT_BUILDER", r.builder()),
			env("KWERFT_CONTEXT", r.contextDir),
			env("KWERFT_DOCKERFILE", r.dockerfile),
			env("KWERFT_IMAGE", r.image),
			env("KWERFT_CACHE", r.cache),
			env("KWERFT_REGISTRY", builds.RegistryHost),
			env("KWERFT_RAILPACK_FRONTEND", r.railpackImage),
			env("KWERFT_CACHE_KEY", r.project+"/"+r.build.Spec.App),
			// Rootless buildkitd can take a while to start on a busy node.
			env("BUILDCTL_CONNECT_RETRIES_MAX", "60"),
		).
		WithResources(requirements(buildCPURequest, buildMemoryRequest, buildCPULimit, buildMemoryLimit)).
		WithSecurityContext(corev1ac.SecurityContext().
			WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeUnconfined)).
			WithAppArmorProfile(corev1ac.AppArmorProfile().WithType(corev1.AppArmorProfileTypeUnconfined))).
		WithTerminationMessagePolicy(corev1.TerminationMessageFallbackToLogsOnError).
		WithVolumeMounts(
			corev1ac.VolumeMount().WithName("workspace").WithMountPath("/workspace").WithReadOnly(true),
			corev1ac.VolumeMount().WithName("buildkit").WithMountPath("/home/user/.local/share/buildkit"),
		)
}

// tokenSecret holds a GitHub App installation token for one clone. The Job
// owns it, so it goes with the Job at the latest; the reconciler deletes it
// as soon as the build finishes.
func (r *buildRun) tokenSecret(token string, jobUID string) *corev1ac.SecretApplyConfiguration {
	labels := buildLabels(r.build)
	labels[LabelGitToken] = "true"
	return corev1ac.Secret(r.credentials, builds.Namespace).
		WithLabels(labels).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion("batch/v1").WithKind("Job").WithName(r.jobName).WithUID(types.UID(jobUID)).WithController(true)).
		WithType(corev1.SecretTypeOpaque).
		WithStringData(map[string]string{builds.KeyToken: token})
}

// The build log is the build container's log alone (the console streams
// one container), so the clone and prepare steps run as init containers that
// never fail: each writes its output to /workspace/.kwerft/<step>.log, its
// exit code to <step>.status and a known failure to <step>.error. The build
// container prints those logs first and fails for them with "Clone failed:"
// or "Railpack:". Credentials stay in the clone container; the build
// container, where the repository's code runs, never sees them.

// cloneScript runs in the BuildKit image (git, ssh, BusyBox). Every input
// comes from the environment, never from string interpolation. Credentials
// reach git through a credential helper (token) or GIT_SSH_COMMAND (key), so
// they are in no URL, argument or log line.
const cloneScript = `k=/workspace/.kwerft
mkdir -p "$k"
(
set -eu
fail() { printf '%s' "$1" >"$k/clone.error"; echo "error: $1" >&2; exit 1; }
export HOME=/scratch GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1
git config --global protocol.allow never
git config --global protocol.https.allow always
git config --global protocol.ssh.allow always
git config --global advice.detachedHead false
case "$KWERFT_GIT_AUTH" in
token)
  [ -s /credentials/token ] || fail "the Git connection has no access token"
  git config --global credential.helper '!f() { test "$1" = get || exit 0; echo "username=$KWERFT_GIT_USERNAME"; printf "password=%s\n" "$(cat /credentials/token)"; }; f'
  ;;
ssh)
  [ -s /credentials/ssh-privatekey ] || fail "the Git connection has no SSH key"
  [ -s /credentials/known_hosts ] || fail "the Git connection has no known_hosts entry for the server"
  { cat /credentials/ssh-privatekey; echo; } >/scratch/id_key
  chmod 600 /scratch/id_key
  export GIT_SSH_COMMAND="ssh -i /scratch/id_key -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/credentials/known_hosts"
  ;;
esac
echo "Cloning $KWERFT_REPOSITORY at $KWERFT_COMMIT"
git init -q /workspace/src
cd /workspace/src
git remote add origin "$KWERFT_REPOSITORY"
if ! git fetch --depth 1 origin "$KWERFT_COMMIT"; then
  echo "Fetching the single commit failed; fetching all branches and tags"
  if ! git fetch origin '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*' 2>/scratch/fetch.err; then
    cat /scratch/fetch.err >&2
    fail "$(grep -v '^[[:space:]]*$' /scratch/fetch.err | tail -n 1)"
  fi
fi
git checkout -q --detach "$KWERFT_COMMIT" 2>/dev/null || fail "commit $KWERFT_COMMIT not found in $KWERFT_REPOSITORY"
echo "Checked out $(git log -1 --format='%h %s')"
) >"$k/clone.log" 2>&1
echo $? >"$k/clone.status"
cat "$k/clone.log"
`

// prepareScript runs `railpack prepare` (in the Railpack image) after a
// successful clone; $1 is the build directory.
const prepareScript = `k=/workspace/.kwerft
[ "$(cat "$k/clone.status" 2>/dev/null)" = 0 ] || exit 0
/railpack prepare "$1" --plan-out /workspace/plan/railpack-plan.json --info-out /workspace/plan/railpack-info.json >"$k/prepare.log" 2>&1
echo $? >"$k/prepare.status"
cat "$k/prepare.log"
`

// buildScript prints the earlier steps' logs, then runs buildctl against a
// buildkitd it starts itself (buildctl-daemonless.sh). The registry is plain
// HTTP; the pod resolves its name through a host alias to the registry
// Service. On success the image digest goes to the termination log as
// "digest=sha256:..."; a known failure as "error=<message>"; otherwise the
// kubelet falls back to the log tail.
const buildScript = `set -eu
fail() { printf 'error=%s' "$1" >/dev/termination-log; echo "error: $1" >&2; exit 1; }
lastline() { grep -v '^[[:space:]]*$' "$1" 2>/dev/null | tail -n 1; }
k=/workspace/.kwerft
cat "$k/clone.log" 2>/dev/null || true
if [ "$(cat "$k/clone.status" 2>/dev/null)" != 0 ]; then
  msg=$(cat "$k/clone.error" 2>/dev/null || true)
  [ -n "$msg" ] || msg=$(lastline "$k/clone.log")
  fail "Clone failed: ${msg:-no output}"
fi
if [ "$KWERFT_BUILDER" = railpack ]; then
  cat "$k/prepare.log" 2>/dev/null || true
  [ "$(cat "$k/prepare.status" 2>/dev/null)" = 0 ] || fail "Railpack: $(lastline "$k/prepare.log")"
fi
src=/workspace/src
ctx=$(cd "$src/$KWERFT_CONTEXT" 2>/dev/null && pwd -P) || fail "directory /$KWERFT_CONTEXT not found in the repository"
case "$ctx/" in "$src"/*) ;; *) fail "the build directory points outside the repository" ;; esac
mkdir -p /tmp/kwerft
printf '[registry."%s"]\n  http = true\n  insecure = true\n' "$KWERFT_REGISTRY" >/tmp/kwerft/buildkitd.toml
export BUILDKITD_FLAGS="--oci-worker-no-process-sandbox --config=/tmp/kwerft/buildkitd.toml"
case "$KWERFT_BUILDER" in
railpack)
  [ -f /workspace/plan/railpack-plan.json ] || fail "Railpack wrote no build plan"
  set -- --frontend gateway.v0 --opt "source=$KWERFT_RAILPACK_FRONTEND" \
    --local "context=$ctx" --local dockerfile=/workspace/plan --opt filename=railpack-plan.json \
    --opt "build-arg:cache-key=$KWERFT_CACHE_KEY"
  ;;
*)
  df="$ctx/$KWERFT_DOCKERFILE"
  [ -f "$df" ] || fail "Dockerfile not found: ${df#"$src"/}"
  dfdir=$(cd "$(dirname "$df")" && pwd -P)
  case "$dfdir/" in "$src"/*) ;; *) fail "the Dockerfile points outside the repository" ;; esac
  set -- --frontend dockerfile.v0 --local "context=$ctx" --local "dockerfile=$dfdir" --opt "filename=$(basename "$df")"
  ;;
esac
echo "Building $KWERFT_IMAGE"
buildctl-daemonless.sh build --progress=plain "$@" \
  --output "type=image,name=$KWERFT_IMAGE,push=true,registry.insecure=true" \
  --import-cache "type=registry,ref=$KWERFT_CACHE,registry.insecure=true" \
  --export-cache "type=registry,ref=$KWERFT_CACHE,mode=max,image-manifest=true,oci-mediatypes=true,registry.insecure=true" \
  --metadata-file /tmp/kwerft/metadata.json
digest=$(tr -d ' \n\t' </tmp/kwerft/metadata.json | sed -n 's/.*"containerimage.digest":"\(sha256:[0-9a-f]*\)".*/\1/p')
[ -n "$digest" ] || fail "BuildKit did not report the image digest"
printf 'digest=%s' "$digest" >/dev/termination-log
echo "Pushed $KWERFT_IMAGE@$digest"
`

// buildResult is what a build pod's termination messages say.
type buildResult struct {
	digest  string
	message string // why it failed, one line
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// parseTermination reads a container's termination message: our own
// "digest=" / "error=" lines, or the log tail the kubelet put there.
func parseTermination(msg string) buildResult {
	msg = strings.TrimSpace(ansiEscape.ReplaceAllString(msg, ""))
	if d, ok := strings.CutPrefix(msg, "digest="); ok {
		return buildResult{digest: strings.TrimSpace(d)}
	}
	if e, ok := strings.CutPrefix(msg, "error="); ok {
		return buildResult{message: oneLine(e)}
	}
	lines := strings.Split(msg, "\n")
	// The last line that reads like an error, else the last line.
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "error") || strings.HasPrefix(low, "fatal") || strings.Contains(low, "error:") {
			return buildResult{message: oneLine(l)}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return buildResult{message: oneLine(l)}
		}
	}
	return buildResult{}
}

// oneLine shortens a message for Build.status.message.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const limit = 300
	if len(s) > limit {
		s = s[:limit-3] + "..."
	}
	return s
}

// humanDuration prints 30m for 30m0s and 1h for 1h0m0s.
func humanDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// deployImage is what an App runs for a successful build: the pushed tag
// pinned to its digest, so a later push to the same tag cannot change what
// runs.
func deployImage(b *kwerftv1.Build) string {
	if b.Status.Image == "" {
		return ""
	}
	if b.Status.Digest != "" {
		return b.Status.Image + "@" + b.Status.Digest
	}
	return b.Status.Image
}

func describeBuild(b *kwerftv1.Build, namespace, app string) string {
	if b.Status.Number == 0 {
		if b.Namespace == namespace {
			return b.Name
		}
		return b.Namespace + "/" + b.Name
	}
	switch {
	case b.Namespace != namespace:
		return fmt.Sprintf("%s/%s #%d", b.Namespace, b.Spec.App, b.Status.Number)
	case b.Spec.App != app:
		return fmt.Sprintf("%s #%d", b.Spec.App, b.Status.Number)
	}
	return fmt.Sprintf("#%d", b.Status.Number)
}
