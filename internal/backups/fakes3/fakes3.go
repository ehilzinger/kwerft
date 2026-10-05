// Package fakes3 is an S3-compatible bucket for tests: objects, listing
// with pages, multipart uploads, and SSE-C as Hetzner Object Storage does
// it (an object written with a customer key is read only with the same
// key). Every request's SigV4 signature is checked against the one access
// key it knows.
package fakes3

import (
	"bytes"
	"crypto/md5" //nolint:gosec // SSE-C names MD5 for the key's checksum
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ehilzinger/kwerft/internal/backups"
)

// Object is a stored object.
type Object struct {
	Data     []byte
	KeyMD5   string // base64 MD5 of the SSE-C key it was written with; "" without
	Modified time.Time
}

type part struct {
	data []byte
	etag string
}

type upload struct {
	key       string
	keyMD5    string
	initiated time.Time
	parts     map[int]part
}

// Bucket is one bucket. Lock Mu to read its fields while it serves.
type Bucket struct {
	Mu        sync.Mutex
	Name      string
	AccessKey string
	SecretKey string
	Objects   map[string]*Object
	// PageSize of listings (default 1000).
	PageSize int
	// MinPartSize: every part but the last of a multipart upload must be
	// at least this big (S3: 5 MiB).
	MinPartSize int
	// FailPart, when it returns true, answers that UploadPart with a 500.
	FailPart func(key string, n int) bool
	// Requests: "METHOD /key?sorted,query,names".
	Requests []string
	// Now for LastModified (default time.Now).
	Now func() time.Time

	uploads map[string]*upload
	nextID  int
}

// New makes an empty bucket.
func New(name, access, secret string) *Bucket {
	return &Bucket{Name: name, AccessKey: access, SecretKey: secret, Objects: map[string]*Object{}, MinPartSize: 5 << 20}
}

// Uploads reports the unfinished multipart uploads' keys.
func (b *Bucket) Uploads() []string {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	var out []string
	for _, u := range b.uploads {
		out = append(out, u.key)
	}
	slices.Sort(out)
	return out
}

// Count counts requests that start with prefix ("PUT /acme/x?partNumber").
func (b *Bucket) Count(prefix string) int {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	n := 0
	for _, r := range b.Requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (b *Bucket) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
}

// checkSignature recomputes the request's SigV4 signature over the headers
// it names.
func (b *Bucket) checkSignature(r *http.Request, body []byte) bool {
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential="+b.AccessKey+"/") {
		return false
	}
	_, signed, ok := strings.Cut(auth, "SignedHeaders=")
	if !ok {
		return false
	}
	signed, _, _ = strings.Cut(signed, ",")
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return false
	}
	_, scope, _ := strings.Cut(auth, "Credential="+b.AccessKey+"/")
	parts := strings.Split(scope, "/")
	if len(parts) < 2 {
		return false
	}
	u := *r.URL
	u.Scheme, u.Host = "https", r.Host
	re, _ := http.NewRequest(r.Method, u.String(), nil)
	for _, h := range strings.Split(signed, ";") {
		if h == "host" || h == "x-amz-date" || h == "x-amz-content-sha256" {
			continue
		}
		re.Header[http.CanonicalHeaderKey(h)] = r.Header.Values(h)
	}
	backups.Sign(re, body, backups.Credentials{AccessKey: b.AccessKey, SecretKey: b.SecretKey}, parts[1], at)
	return re.Header.Get("Authorization") == auth && re.Header.Get("X-Amz-Content-Sha256") == r.Header.Get("X-Amz-Content-Sha256")
}

// sseOf checks a request's SSE-C headers: "" without any, the key's MD5
// with valid ones, ok false for incomplete or inconsistent ones.
func sseOf(r *http.Request) (string, bool) {
	alg, key, sum := r.Header.Get(backups.HeaderSSECAlgorithm), r.Header.Get(backups.HeaderSSECKey), r.Header.Get(backups.HeaderSSECKeyMD5)
	if alg == "" && key == "" && sum == "" {
		return "", true
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if alg != "AES256" || err != nil || len(raw) != 32 {
		return "", false
	}
	h := md5.Sum(raw) //nolint:gosec // see the import
	if base64.StdEncoding.EncodeToString(h[:]) != sum {
		return "", false
	}
	return sum, true
}

func (b *Bucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.Mu.Lock()
	defer b.Mu.Unlock()
	q := r.URL.Query()
	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	b.Requests = append(b.Requests, r.Method+" "+r.URL.Path+"?"+strings.Join(names, ","))
	if !b.checkSignature(r, body) {
		fail(w, 403, "SignatureDoesNotMatch", "bad signature")
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != b.Name {
		fail(w, 404, "NoSuchBucket", "no such bucket")
		return
	}
	sse, ok := sseOf(r)
	if !ok {
		fail(w, 400, "InvalidArgument", "bad SSE-C headers")
		return
	}
	if b.uploads == nil {
		b.uploads = map[string]*upload{}
	}
	_, isUploads := q["uploads"]
	uploadID := q.Get("uploadId")
	switch {
	case r.Method == "GET" && key == "" && isUploads:
		b.listUploads(w, q.Get("prefix"))
	case r.Method == "GET" && key == "":
		b.list(w, q.Get("prefix"), q.Get("continuation-token"))
	case r.Method == "POST" && isUploads:
		b.nextID++
		id := "upload-" + strconv.Itoa(b.nextID)
		b.uploads[id] = &upload{key: key, keyMD5: sse, initiated: b.now(), parts: map[int]part{}}
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", key, id)
	case r.Method == "PUT" && uploadID != "":
		u := b.uploads[uploadID]
		n, _ := strconv.Atoi(q.Get("partNumber"))
		switch {
		case u == nil || u.key != key:
			fail(w, 404, "NoSuchUpload", "no such upload")
		case u.keyMD5 != sse:
			fail(w, 400, "InvalidRequest", "the part's SSE-C key differs from the upload's")
		case n < 1 || n > 10000:
			fail(w, 400, "InvalidArgument", "part number")
		case b.FailPart != nil && b.FailPart(key, n):
			fail(w, 500, "InternalError", "try again")
		default:
			sum := md5.Sum(body) //nolint:gosec // an ETag
			etag := `"` + hex.EncodeToString(sum[:]) + `"`
			u.parts[n] = part{data: body, etag: etag}
			w.Header().Set("ETag", etag)
		}
	case r.Method == "GET" && uploadID != "":
		u := b.uploads[uploadID]
		if u == nil || u.key != key {
			fail(w, 404, "NoSuchUpload", "no such upload")
			return
		}
		var out strings.Builder
		out.WriteString("<ListPartsResult>")
		marker, _ := strconv.Atoi(q.Get("part-number-marker"))
		nums := make([]int, 0, len(u.parts))
		for n := range u.parts {
			if n > marker {
				nums = append(nums, n)
			}
		}
		slices.Sort(nums)
		page := b.pageSize()
		truncated := len(nums) > page
		if truncated {
			nums = nums[:page]
		}
		for _, n := range nums {
			_, _ = fmt.Fprintf(&out, "<Part><PartNumber>%d</PartNumber><Size>%d</Size><ETag>%s</ETag></Part>", n, len(u.parts[n].data), u.parts[n].etag)
		}
		if truncated {
			_, _ = fmt.Fprintf(&out, "<IsTruncated>true</IsTruncated><NextPartNumberMarker>%d</NextPartNumberMarker>", nums[len(nums)-1])
		}
		out.WriteString("</ListPartsResult>")
		_, _ = w.Write([]byte(out.String()))
	case r.Method == "POST" && uploadID != "":
		b.complete(w, key, uploadID, body, sse)
	case r.Method == "DELETE" && uploadID != "":
		if u := b.uploads[uploadID]; u == nil || u.key != key {
			fail(w, 404, "NoSuchUpload", "no such upload")
			return
		}
		delete(b.uploads, uploadID)
		w.WriteHeader(204)
	case r.Method == "PUT":
		b.Objects[key] = &Object{Data: body, KeyMD5: sse, Modified: b.now()}
	case r.Method == "GET":
		o := b.Objects[key]
		switch {
		case o == nil:
			fail(w, 404, "NoSuchKey", "no such key")
		case o.KeyMD5 != sse:
			fail(w, 400, "InvalidRequest", "the object was stored with another (or no) customer key")
		default:
			w.Header().Set("Content-Length", strconv.Itoa(len(o.Data)))
			_, _ = w.Write(o.Data)
		}
	case r.Method == "DELETE":
		if _, ok := b.Objects[key]; !ok {
			w.WriteHeader(204) // S3 answers 204 for a missing key too
			return
		}
		delete(b.Objects, key)
		w.WriteHeader(204)
	default:
		fail(w, 400, "Unsupported", r.Method)
	}
}

func (b *Bucket) pageSize() int {
	if b.PageSize > 0 {
		return b.PageSize
	}
	return 1000
}

func (b *Bucket) list(w http.ResponseWriter, prefix, token string) {
	var keys []string
	for k := range b.Objects {
		if strings.HasPrefix(k, prefix) && k > token {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	truncated := len(keys) > b.pageSize()
	if truncated {
		keys = keys[:b.pageSize()]
	}
	var out strings.Builder
	out.WriteString("<ListBucketResult>")
	for _, k := range keys {
		o := b.Objects[k]
		_, _ = fmt.Fprintf(&out, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>", k, len(o.Data), o.Modified.Format(time.RFC3339Nano))
	}
	if truncated {
		_, _ = fmt.Fprintf(&out, "<IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken>", keys[len(keys)-1])
	}
	out.WriteString("</ListBucketResult>")
	_, _ = w.Write([]byte(out.String()))
}

func (b *Bucket) listUploads(w http.ResponseWriter, prefix string) {
	ids := make([]string, 0, len(b.uploads))
	for id, u := range b.uploads {
		if strings.HasPrefix(u.key, prefix) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var out strings.Builder
	out.WriteString("<ListMultipartUploadsResult>")
	for _, id := range ids {
		u := b.uploads[id]
		_, _ = fmt.Fprintf(&out, "<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>", u.key, id, u.initiated.Format(time.RFC3339Nano))
	}
	out.WriteString("</ListMultipartUploadsResult>")
	_, _ = w.Write([]byte(out.String()))
}

func (b *Bucket) complete(w http.ResponseWriter, key, id string, body []byte, sse string) {
	u := b.uploads[id]
	if u == nil || u.key != key {
		fail(w, 404, "NoSuchUpload", "no such upload")
		return
	}
	var req struct {
		Parts []struct {
			Number int    `xml:"PartNumber"`
			ETag   string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		fail(w, 400, "MalformedXML", "parts")
		return
	}
	if sse != "" && sse != u.keyMD5 {
		fail(w, 400, "InvalidRequest", "another customer key")
		return
	}
	var data bytes.Buffer
	for i, p := range req.Parts {
		got, ok := u.parts[p.Number]
		if !ok || got.etag != p.ETag || i > 0 && p.Number <= req.Parts[i-1].Number {
			fail(w, 400, "InvalidPart", fmt.Sprintf("part %d", p.Number))
			return
		}
		if i < len(req.Parts)-1 && len(got.data) < b.MinPartSize {
			fail(w, 400, "EntityTooSmall", fmt.Sprintf("part %d is %d bytes", p.Number, len(got.data)))
			return
		}
		data.Write(got.data)
	}
	b.Objects[key] = &Object{Data: data.Bytes(), KeyMD5: u.keyMD5, Modified: b.now()}
	delete(b.uploads, id)
	_, _ = fmt.Fprintf(w, "<CompleteMultipartUploadResult><Key>%s</Key></CompleteMultipartUploadResult>", key)
}

// KeyMD5 is the base64 MD5 of an SSE-C key, as the bucket records it.
func KeyMD5(key []byte) string {
	h := md5.Sum(key) //nolint:gosec // see the import
	return base64.StdEncoding.EncodeToString(h[:])
}
