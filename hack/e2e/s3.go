package main

// The restore run's bucket: an S3-compatible bucket (Hetzner Object
// Storage) the run writes its backups to, under the prefix <run id>, which
// the run deletes again — always, like the servers. Requests are signed
// with the product's SigV4 implementation (internal/backups, tested
// against AWS's vectors); listing and deleting SSE-C objects needs no key.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ehilzinger/kwerft/internal/backups"
)

// s3Config is the bucket of the restore run (E2E_S3_*).
type s3Config struct {
	Endpoint  string // https://fsn1.your-objectstorage.com
	Region    string // empty: from the endpoint (fsn1), else us-east-1
	Bucket    string
	AccessKey string
	SecretKey string
}

func (c s3Config) region() string {
	return backups.RegionFor(backups.Target{Endpoint: c.Endpoint, Region: c.Region})
}

// bucket lists and deletes objects of one bucket.
type bucket struct {
	cfg  s3Config
	http *http.Client
	now  func() time.Time
}

func newBucket(cfg s3Config, hc *http.Client) *bucket {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &bucket{cfg: cfg, http: hc, now: time.Now}
}

func (b *bucket) request(ctx context.Context, method, key string, q url.Values) ([]byte, error) {
	u, err := url.Parse(strings.TrimRight(b.cfg.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", b.cfg.Endpoint)
	}
	u.Path = "/" + b.cfg.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	backups.Sign(req, nil, backups.Credentials{AccessKey: b.cfg.AccessKey, SecretKey: b.cfg.SecretKey}, b.cfg.region(), b.now())
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		_ = xml.Unmarshal(data, &e)
		return nil, &backups.Error{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
	}
	return data, nil
}

// list returns every key under prefix (ListObjectsV2, all pages).
func (b *bucket) list(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		data, err := b.request(ctx, http.MethodGet, "", q)
		if err != nil {
			return nil, err
		}
		var res struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(data, &res); err != nil {
			return nil, fmt.Errorf("unexpected answer from the storage: %w", err)
		}
		for _, o := range res.Contents {
			keys = append(keys, o.Key)
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return keys, nil
		}
		token = res.NextContinuationToken
	}
}

// deletePrefix deletes every object under prefix ("<run id>/") and returns
// how many it deleted. Failed deletions are retried a few rounds; what is
// left is the error.
func (b *bucket) deletePrefix(ctx context.Context, prefix string) (int, error) {
	if strings.Trim(prefix, "/") == "" {
		return 0, errors.New("refusing to delete the whole bucket")
	}
	deleted := 0
	var first error
	for round := 0; round < 5; round++ {
		keys, err := b.list(ctx, prefix)
		if err != nil {
			return deleted, err
		}
		if len(keys) == 0 {
			return deleted, nil // a failure retried later does not count
		}
		failed := 0
		for _, k := range keys {
			if !strings.HasPrefix(k, prefix) {
				continue // never outside the prefix, whatever the storage answers
			}
			if _, err := b.request(ctx, http.MethodDelete, k, nil); err != nil {
				var se *backups.Error
				if errors.As(err, &se) && se.Status == http.StatusNotFound {
					continue
				}
				failed++
				if first == nil {
					first = fmt.Errorf("delete %s: %w", k, err)
				}
				continue
			}
			deleted++
		}
		if failed == len(keys) {
			return deleted, first
		}
	}
	keys, err := b.list(ctx, prefix)
	if err == nil && len(keys) > 0 {
		return deleted, fmt.Errorf("%d object(s) left under %s: %v", len(keys), prefix, first)
	}
	return deleted, err
}
