package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

// DefaultRegistryURL is the registry's API inside the cluster.
var DefaultRegistryURL = fmt.Sprintf("http://%s.%s.svc:%d", builds.RegistryService, builds.RegistryNamespace, builds.RegistryPort)

// RegistryKeeper tags the images an App's revisions run with
// builds.KeepTagPrefix + digest, and removes such tags once no revision runs
// the image. The registry's retention keeps keep- tags and drops other old
// tags, so an image a revision may roll back to never disappears, however
// many builds came after it.
type RegistryKeeper struct {
	// URL of the registry API, e.g. DefaultRegistryURL.
	URL  string
	HTTP *http.Client

	mu     sync.Mutex
	synced map[string]string // repository → keep set last written
}

// keepTag is the tag that protects an image with this digest.
func keepTag(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) > 32 {
		hex = hex[:32]
	}
	return builds.KeepTagPrefix + hex
}

// keepSet maps keep tags to digests for the App's revisions that run an
// image of its own repository in the registry.
func keepSet(project, app string, history []kwerftv1.AppRevision) map[string]string {
	prefix := builds.ImageRepository(project, app)
	out := map[string]string{}
	for _, rev := range history {
		name, digest, ok := strings.Cut(rev.Image, "@")
		if !ok || !strings.HasPrefix(digest, "sha256:") {
			continue
		}
		if name != prefix && !strings.HasPrefix(name, prefix+":") {
			continue
		}
		out[keepTag(digest)] = digest
	}
	return out
}

// Keep makes the repository's keep tags match the App's history.
func (k *RegistryKeeper) Keep(ctx context.Context, project, app string, history []kwerftv1.AppRevision) error {
	want := keepSet(project, app, history)
	repo := project + "/" + app
	sig := strings.Join(slices.Sorted(maps.Keys(want)), ",")
	k.mu.Lock()
	done := k.synced[repo] == sig
	k.mu.Unlock()
	if done {
		return nil
	}
	if err := k.sync(ctx, repo, want); err != nil {
		return err
	}
	k.mu.Lock()
	if k.synced == nil {
		k.synced = map[string]string{}
	}
	k.synced[repo] = sig
	k.mu.Unlock()
	return nil
}

func (k *RegistryKeeper) client() *http.Client {
	if k.HTTP != nil {
		return k.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (k *RegistryKeeper) do(ctx context.Context, method, path string, header http.Header, body []byte) (*http.Response, []byte, error) {
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.URL, "/")+path, r)
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(req.Header, header)
	resp, err := k.client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp, data, err
}

var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

func (k *RegistryKeeper) sync(ctx context.Context, repo string, want map[string]string) error {
	resp, data, err := k.do(ctx, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil // nothing pushed yet
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry: list tags of %s: %s", repo, resp.Status)
	}
	var list struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("registry: list tags of %s: %w", repo, err)
	}
	accept := http.Header{"Accept": {manifestTypes}}
	for tag, digest := range want {
		if slices.Contains(list.Tags, tag) {
			continue // the tag names its digest; it cannot point elsewhere
		}
		resp, manifest, err := k.do(ctx, http.MethodGet, "/v2/"+repo+"/manifests/"+digest, accept, nil)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			continue // already gone; nothing to keep
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("registry: get %s@%s: %s", repo, digest, resp.Status)
		}
		put := http.Header{"Content-Type": {resp.Header.Get("Content-Type")}}
		resp, _, err = k.do(ctx, http.MethodPut, "/v2/"+repo+"/manifests/"+tag, put, manifest)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("registry: tag %s:%s: %s", repo, tag, resp.Status)
		}
	}
	for _, tag := range list.Tags {
		if _, keep := want[tag]; keep || !strings.HasPrefix(tag, builds.KeepTagPrefix) {
			continue
		}
		resp, _, err := k.do(ctx, http.MethodDelete, "/v2/"+repo+"/manifests/"+tag, nil, nil)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("registry: untag %s:%s: %s", repo, tag, resp.Status)
		}
	}
	return nil
}
