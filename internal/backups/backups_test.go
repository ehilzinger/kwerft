package backups

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecoveryKey(t *testing.T) {
	k := NewRecoveryKey()
	if len(k) != 52+12 || strings.Count(k, "-") != 12 {
		t.Fatalf("format: %q", k)
	}
	if k == NewRecoveryKey() {
		t.Fatal("two keys are equal")
	}
	// Typed sloppily, it reads back the same.
	sloppy := strings.ToLower(strings.ReplaceAll(k, "-", " "))
	if got, err := NormalizeRecoveryKey("  " + sloppy + "\n"); err != nil || got != k {
		t.Fatalf("normalize: %q %v, want %q", got, err, k)
	}
	for _, bad := range []string{"", "abcd", strings.Repeat("A", 51), strings.Repeat("1", 52), k + "A"} {
		if _, err := NormalizeRecoveryKey(bad); !errors.Is(err, ErrRecoveryKey) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The examples of the AWS documentation ("Signature Calculations for the
// Authorization Header", S3).
func TestSignAWSExamples(t *testing.T) {
	cr := Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	get, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	get.Header.Set("Range", "bytes=0-9")
	Sign(get, nil, cr, "us-east-1", at)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := get.Header.Get("Authorization"); got != want {
		t.Errorf("GET object:\n got %s\nwant %s", got, want)
	}

	list, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	Sign(list, nil, cr, "us-east-1", at)
	want = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
	if got := list.Header.Get("Authorization"); got != want {
		t.Errorf("list:\n got %s\nwant %s", got, want)
	}
}

func TestRegionFor(t *testing.T) {
	for in, want := range map[Target]string{
		{Endpoint: "https://fsn1.your-objectstorage.com"}:               "fsn1",
		{Endpoint: "https://hel1.your-objectstorage.com/"}:              "hel1",
		{Endpoint: "https://s3.example.com"}:                            "us-east-1",
		{Endpoint: "https://fsn1.your-objectstorage.com", Region: "eu"}: "eu",
	} {
		if got := RegionFor(in); got != want {
			t.Errorf("%+v: %s, want %s", in, got, want)
		}
	}
}

// fakeS3 keeps objects of one bucket and checks the access key.
type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	key      string
	readOnly bool
	objects  map[string]string
	requests []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<Error><Code>" + code + "</Code><Message>no</Message></Error>"))
	}
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential="+f.key+"/") || r.Header.Get("X-Amz-Date") == "" {
		fail(403, "InvalidAccessKeyId")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != f.bucket {
		fail(404, "NoSuchBucket")
		return
	}
	switch {
	case r.Method == "GET" && key == "":
		var b strings.Builder
		b.WriteString("<ListBucketResult>")
		for k := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				b.WriteString("<Contents><Key>" + k + "</Key></Contents>")
			}
		}
		b.WriteString("</ListBucketResult>")
		_, _ = w.Write([]byte(b.String()))
	case r.Method == "PUT" && f.readOnly:
		fail(403, "AccessDenied")
	case r.Method == "PUT":
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		f.objects[key] = buf.String()
	case r.Method == "DELETE":
		delete(f.objects, key)
		w.WriteHeader(204)
	default:
		fail(400, "Unsupported")
	}
}

func TestCheck(t *testing.T) {
	f := &fakeS3{bucket: "acme", key: "AK", objects: map[string]string{}}
	srv := httptest.NewTLSServer(f)
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	ctx := context.Background()
	target := Target{Endpoint: srv.URL, Bucket: "acme", Prefix: "ops.example.com"}

	res, err := c.Check(ctx, target, Credentials{AccessKey: "AK", SecretKey: "SK"})
	if err != nil || res.HasObjects {
		t.Fatalf("empty bucket: %+v %v", res, err)
	}
	if len(f.objects) != 0 || len(f.requests) != 3 || !strings.HasPrefix(f.requests[1], "PUT /acme/ops.example.com/.kwerft-check-") {
		t.Errorf("left %v after %v", f.objects, f.requests)
	}
	f.objects["ops.example.com/velero/backups/x"] = "1"
	if res, err := c.Check(ctx, target, Credentials{AccessKey: "AK", SecretKey: "SK"}); err != nil || !res.HasObjects {
		t.Errorf("with objects: %+v %v", res, err)
	}

	var ce *CheckError
	var se *Error
	_, err = c.Check(ctx, target, Credentials{AccessKey: "wrong", SecretKey: "SK"})
	if !errors.As(err, &ce) || ce.Step != ErrList || !errors.As(err, &se) || se.Code != "InvalidAccessKeyId" {
		t.Errorf("wrong key: %v", err)
	}
	_, err = c.Check(ctx, Target{Endpoint: srv.URL, Bucket: "other"}, Credentials{AccessKey: "AK", SecretKey: "SK"})
	if !errors.As(err, &se) || se.Code != "NoSuchBucket" {
		t.Errorf("wrong bucket: %v", err)
	}
	f.readOnly = true
	_, err = c.Check(ctx, target, Credentials{AccessKey: "AK", SecretKey: "SK"})
	if !errors.As(err, &ce) || ce.Step != ErrWrite {
		t.Errorf("read-only: %v", err)
	}
}

// Tarball builds a backup contents tarball from path → JSON.
func Tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestContents(t *testing.T) {
	raw := Tarball(t, map[string]string{
		"metadata/version": "1.1.0",
		"resources/projects.kwerft.dev/cluster/shop.json":                                  `{"kind":"Project"}`,
		"resources/apps.kwerft.dev/v1alpha1-preferredversion/namespaces/shop/web.json":     `{"kind":"App","v":1}`,
		"resources/apps.kwerft.dev/namespaces/shop/web.json":                               `{"kind":"App"}`,
		"resources/apps.kwerft.dev/namespaces/shop/worker.json":                            `{"kind":"App"}`,
		"resources/secrets/namespaces/shop/db.json":                                        `{"kind":"Secret"}`,
		"resources/volumes.kwerft.dev/v1alpha1-preferredversion/namespaces/shop/data.json": `{"kind":"Volume"}`,
	})
	c, err := ReadContents(bytes.NewReader(raw), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Get("projects.kwerft.dev", "", "shop"); !ok || string(got) != `{"kind":"Project"}` {
		t.Errorf("project: %s %v", got, ok)
	}
	if got, _ := c.Get("apps.kwerft.dev", "shop", "web"); string(got) != `{"kind":"App"}` {
		t.Errorf("the unversioned path wins: %s", got)
	}
	if _, ok := c.Get("volumes.kwerft.dev", "shop", "data"); !ok {
		t.Error("versioned only")
	}
	if _, ok := c.Get("secrets", "shop", "db"); !ok {
		t.Error("core resource")
	}
	if n := len(c.Names("apps.kwerft.dev", "shop")); n != 2 {
		t.Errorf("names: %d", n)
	}
	if _, err := ReadContents(bytes.NewReader(raw), 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("limit: %v", err)
	}
}
