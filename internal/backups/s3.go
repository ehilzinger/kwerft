package backups

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A small S3 client: just enough to check that a bucket can hold backups
// (list, write and delete one object), signed with AWS Signature Version 4.
// Path-style URLs (https://<endpoint>/<bucket>/<key>), as Velero is
// configured (s3ForcePathStyle): Hetzner Object Storage endpoints look like
// https://fsn1.your-objectstorage.com.

// Target is a bucket and a prefix in it.
type Target struct {
	Endpoint string // https://host[:port]
	Region   string // signing region; empty: guessed from the endpoint
	Bucket   string
	Prefix   string // without leading or trailing slash; may be empty
}

// Credentials of an S3 access key.
type Credentials struct {
	AccessKey string
	SecretKey string
}

// Error is an S3 error answer.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Client talks to S3-compatible storage.
type Client struct {
	HTTP *http.Client
	Now  func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// CheckResult is what Check found.
type CheckResult struct {
	// HasObjects: the prefix already holds objects (earlier backups of this
	// console, or someone else's: two consoles must not share a prefix).
	HasObjects bool `json:"hasObjects"`
}

// Phases of a check, for messages.
var (
	ErrList   = errors.New("list")
	ErrWrite  = errors.New("write")
	ErrDelete = errors.New("delete")
)

// CheckError says which step of the check failed and why.
type CheckError struct {
	Step error // ErrList, ErrWrite or ErrDelete
	Err  error // an *Error, or a transport error
}

func (e *CheckError) Error() string { return e.Step.Error() + ": " + e.Err.Error() }
func (e *CheckError) Unwrap() error { return e.Err }

// Check lists the prefix, writes a small object below it and deletes it
// again: Velero needs all three.
func (c *Client) Check(ctx context.Context, t Target, cr Credentials) (*CheckResult, error) {
	keys, err := c.List(ctx, t, cr, prefixDir(t.Prefix), 1)
	if err != nil {
		return nil, &CheckError{Step: ErrList, Err: err}
	}
	out := &CheckResult{HasObjects: len(keys) > 0}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	key := prefixDir(t.Prefix) + ".kwerft-check-" + hex.EncodeToString(b)
	if err := c.do(ctx, t, cr, http.MethodPut, key, nil, []byte("kwerft connection check\n"), nil); err != nil {
		return out, &CheckError{Step: ErrWrite, Err: err}
	}
	if err := c.do(ctx, t, cr, http.MethodDelete, key, nil, nil, nil); err != nil {
		return out, &CheckError{Step: ErrDelete, Err: err}
	}
	return out, nil
}

// List returns up to max keys under prefix (ListObjectsV2).
func (c *Client) List(ctx context.Context, t Target, cr Credentials, prefix string, max int) ([]string, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {fmt.Sprint(max)}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var res struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := c.do(ctx, t, cr, http.MethodGet, "", q, nil, &res); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(res.Contents))
	for _, o := range res.Contents {
		keys = append(keys, o.Key)
	}
	return keys, nil
}

func prefixDir(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func (c *Client) do(ctx context.Context, t Target, cr Credentials, method, key string, q url.Values, body []byte, into any) error {
	u, err := url.Parse(strings.TrimRight(t.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid endpoint %q", t.Endpoint)
	}
	u.Path = "/" + t.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = ""
	u.RawQuery = canonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	if body != nil {
		req.Header.Set("Content-Type", "text/plain")
	}
	Sign(req, body, cr, RegionFor(t), c.now())
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(raw, &x) == nil {
			e.Code, e.Message = x.Code, x.Message
		}
		return e
	}
	if into != nil {
		if err := xml.Unmarshal(raw, into); err != nil {
			return fmt.Errorf("unexpected answer from the storage: %w", err)
		}
	}
	return nil
}

var locationRE = regexp.MustCompile(`^([a-z]{2,4}[0-9]{1,2})\.`)

// RegionFor is the signing region: the given one, else the location the
// endpoint starts with (fsn1.your-objectstorage.com → fsn1), else
// us-east-1.
func RegionFor(t Target) string {
	if t.Region != "" {
		return t.Region
	}
	if u, err := url.Parse(t.Endpoint); err == nil {
		if m := locationRE.FindStringSubmatch(u.Hostname()); m != nil {
			return m[1]
		}
	}
	return "us-east-1"
}

// ---- Signature Version 4 ------------------------------------------------------------

const (
	sigAlgorithm = "AWS4-HMAC-SHA256"
	amzDate      = "20060102T150405Z"
)

// Sign adds the SigV4 headers (x-amz-date, x-amz-content-sha256,
// Authorization) for service s3. Every header already set is signed, and
// host.
func Sign(req *http.Request, body []byte, cr Credentials, region string, now time.Time) {
	now = now.UTC()
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Date", now.Format(amzDate))
	req.Header.Set("X-Amz-Content-Sha256", payload)

	headers := map[string]string{"host": req.URL.Host}
	if req.Host != "" {
		headers["host"] = req.Host
	}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(trimAll(v), ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	slices.Sort(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signed,
		payload,
	}, "\n")
	day := now.Format("20060102")
	scope := day + "/" + region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(canonical))
	toSign := sigAlgorithm + "\n" + now.Format(amzDate) + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	key := hmacSHA256([]byte("AWS4"+cr.SecretKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", sigAlgorithm+" Credential="+cr.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func trimAll(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.Join(strings.Fields(s), " ")
	}
	return out
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalPath is the URI-encoded path (each segment once, S3 style).
func canonicalPath(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		vals := slices.Clone(q[k])
		slices.Sort(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode encodes everything but the RFC 3986 unreserved characters.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
