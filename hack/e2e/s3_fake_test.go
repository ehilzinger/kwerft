package main

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/backups"
)

// fakeS3 is one bucket of an S3-compatible storage: path-style
// ListObjectsV2 (pages of pageSize) and DELETE, every request's SigV4
// signature checked.
type fakeS3 struct {
	*httptest.Server
	bucket   string
	creds    backups.Credentials
	pageSize int

	mu        sync.Mutex
	objects   map[string]bool
	requests  []string
	failFirst map[string]int // DELETE failures before a key goes
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{bucket: "kwerft-e2e", creds: backups.Credentials{AccessKey: "AKTEST", SecretKey: "s3cr3t"}, pageSize: 2,
		objects: map[string]bool{}, failFirst: map[string]int{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeS3) config() s3Config {
	return s3Config{Endpoint: f.URL, Bucket: f.bucket, AccessKey: f.creds.AccessKey, SecretKey: f.creds.SecretKey}
}

func (f *fakeS3) put(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = true
}

func (f *fakeS3) keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// signedOK recomputes the request's signature.
func (f *fakeS3) signedOK(r *http.Request) bool {
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return false
	}
	clone, _ := http.NewRequest(r.Method, "https://"+r.Host+r.URL.RequestURI(), nil)
	backups.Sign(clone, nil, f.creds, "us-east-1", at)
	return clone.Header.Get("Authorization") == r.Header.Get("Authorization")
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if !f.signedOK(r) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`<Error><Code>SignatureDoesNotMatch</Code><Message>bad signature</Message></Error>`))
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code></Error>`))
		return
	}
	switch {
	case r.Method == "GET" && key == "" && r.URL.Query().Get("list-type") == "2":
		q := r.URL.Query()
		var all []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				all = append(all, k)
			}
		}
		slices.Sort(all)
		start, _ := strconv.Atoi(q.Get("continuation-token"))
		end := min(start+f.pageSize, len(all))
		type content struct {
			Key string `xml:"Key"`
		}
		res := struct {
			XMLName               xml.Name  `xml:"ListBucketResult"`
			Contents              []content `xml:"Contents"`
			IsTruncated           bool      `xml:"IsTruncated"`
			NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		}{}
		for _, k := range all[min(start, len(all)):end] {
			res.Contents = append(res.Contents, content{k})
		}
		if end < len(all) {
			res.IsTruncated, res.NextContinuationToken = true, strconv.Itoa(end)
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == "DELETE" && key != "":
		if f.failFirst[key] > 0 {
			f.failFirst[key]--
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`<Error><Code>SlowDown</Code></Error>`))
			return
		}
		delete(f.objects, key)
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}
