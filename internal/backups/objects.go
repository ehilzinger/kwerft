package backups

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// Objects: what the etcd snapshot uploader (etcd.go) and `kwerft
// etcd-snapshot fetch` need beyond the connection check — listing with
// pagination, PUT, GET into a writer, DELETE, and multipart uploads that a
// restarted uploader can resume. Objects are written with SSE-C headers
// (sse.go) when a key is given; a part of a multipart upload needs them
// too.

// Object is one entry of a listing.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ListObjects returns every object under prefix (ListObjectsV2, all pages).
func (c *Client) ListObjects(ctx context.Context, t Target, cr Credentials, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		var res struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := c.do(ctx, t, cr, http.MethodGet, "", q, nil, nil, &res); err != nil {
			return nil, err
		}
		for _, o := range res.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" || res.NextContinuationToken == token {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

// sseHeaders are the SSE-C headers for key (none for a nil key).
func sseHeaders(sseKey []byte) http.Header {
	h := make(http.Header)
	if sseKey != nil {
		SetSSEC(h, sseKey)
	}
	return h
}

// PutObject writes body to key in one request, encrypted with sseKey.
func (c *Client) PutObject(ctx context.Context, t Target, cr Credentials, key string, body []byte, sseKey []byte) error {
	h := sseHeaders(sseKey)
	h.Set("Content-Type", "application/octet-stream")
	if body == nil {
		body = []byte{}
	}
	return c.do(ctx, t, cr, http.MethodPut, key, nil, h, body, nil)
}

// GetObject copies key, decrypted with sseKey, into w and returns how many
// bytes it wrote.
func (c *Client) GetObject(ctx context.Context, t Target, cr Credentials, key string, sseKey []byte, w io.Writer) (int64, error) {
	resp, err := c.send(ctx, t, cr, http.MethodGet, key, nil, sseHeaders(sseKey), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, resp.Body)
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = fmt.Errorf("the storage sent %d of %d bytes", n, resp.ContentLength)
	}
	return n, err
}

// DeleteObject removes key (no error when it is gone already).
func (c *Client) DeleteObject(ctx context.Context, t Target, cr Credentials, key string) error {
	err := c.do(ctx, t, cr, http.MethodDelete, key, nil, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// IsNotFound reports an S3 404 (NoSuchKey, NoSuchUpload).
func IsNotFound(err error) bool {
	var se *Error
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// ---- multipart uploads ----------------------------------------------------------------

// Upload is an unfinished multipart upload.
type Upload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// Part is an uploaded part of a multipart upload.
type Part struct {
	Number int
	Size   int64
	ETag   string
}

// ListMultipartUploads returns the unfinished multipart uploads under
// prefix (all pages).
func (c *Client) ListMultipartUploads(ctx context.Context, t Target, cr Credentials, prefix string) ([]Upload, error) {
	var out []Upload
	keyMarker, idMarker := "", ""
	for {
		q := url.Values{"uploads": {""}, "prefix": {prefix}}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
			q.Set("upload-id-marker", idMarker)
		}
		var res struct {
			Uploads []struct {
				Key       string    `xml:"Key"`
				UploadID  string    `xml:"UploadId"`
				Initiated time.Time `xml:"Initiated"`
			} `xml:"Upload"`
			IsTruncated        bool   `xml:"IsTruncated"`
			NextKeyMarker      string `xml:"NextKeyMarker"`
			NextUploadIDMarker string `xml:"NextUploadIdMarker"`
		}
		if err := c.do(ctx, t, cr, http.MethodGet, "", q, nil, nil, &res); err != nil {
			return nil, err
		}
		for _, u := range res.Uploads {
			out = append(out, Upload{Key: u.Key, UploadID: u.UploadID, Initiated: u.Initiated})
		}
		if !res.IsTruncated || res.NextKeyMarker == "" || res.NextKeyMarker == keyMarker && res.NextUploadIDMarker == idMarker {
			return out, nil
		}
		keyMarker, idMarker = res.NextKeyMarker, res.NextUploadIDMarker
	}
}

// CreateMultipartUpload starts a multipart upload of key, encrypted with
// sseKey, and returns its id.
func (c *Client) CreateMultipartUpload(ctx context.Context, t Target, cr Credentials, key string, sseKey []byte) (string, error) {
	h := sseHeaders(sseKey)
	h.Set("Content-Type", "application/octet-stream")
	var res struct {
		UploadID string `xml:"UploadId"`
	}
	if err := c.do(ctx, t, cr, http.MethodPost, key, url.Values{"uploads": {""}}, h, nil, &res); err != nil {
		return "", err
	}
	if res.UploadID == "" {
		return "", errors.New("the storage started a multipart upload without an id")
	}
	return res.UploadID, nil
}

// UploadPart uploads part n (from 1) and returns its ETag.
func (c *Client) UploadPart(ctx context.Context, t Target, cr Credentials, key, uploadID string, n int, body []byte, sseKey []byte) (string, error) {
	q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}}
	h := sseHeaders(sseKey)
	h.Set("Content-Type", "application/octet-stream")
	resp, err := c.send(ctx, t, cr, http.MethodPut, key, q, h, body)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("the storage answered part %d without an ETag", n)
	}
	return etag, nil
}

// ListParts returns the parts uploaded so far (all pages).
func (c *Client) ListParts(ctx context.Context, t Target, cr Credentials, key, uploadID string) ([]Part, error) {
	var out []Part
	marker := ""
	for {
		q := url.Values{"uploadId": {uploadID}}
		if marker != "" {
			q.Set("part-number-marker", marker)
		}
		var res struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				Size   int64  `xml:"Size"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
			IsTruncated          bool   `xml:"IsTruncated"`
			NextPartNumberMarker string `xml:"NextPartNumberMarker"`
		}
		if err := c.do(ctx, t, cr, http.MethodGet, key, q, nil, nil, &res); err != nil {
			return nil, err
		}
		for _, p := range res.Parts {
			out = append(out, Part{Number: p.Number, Size: p.Size, ETag: p.ETag})
		}
		if !res.IsTruncated || res.NextPartNumberMarker == "" || res.NextPartNumberMarker == marker {
			return out, nil
		}
		marker = res.NextPartNumberMarker
	}
}

// CompleteMultipartUpload joins the parts into the object. The SSE-C
// headers go along (as the AWS SDK sends them; stores that need none
// ignore them).
func (c *Client) CompleteMultipartUpload(ctx context.Context, t Target, cr Credentials, key, uploadID string, parts []Part, sseKey []byte) error {
	type xmlPart struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	body := struct {
		XMLName xml.Name  `xml:"CompleteMultipartUpload"`
		Parts   []xmlPart `xml:"Part"`
	}{}
	for _, p := range parts {
		body.Parts = append(body.Parts, xmlPart{Number: p.Number, ETag: p.ETag})
	}
	raw, err := xml.Marshal(body)
	if err != nil {
		return err
	}
	h := sseHeaders(sseKey)
	h.Set("Content-Type", "application/xml")
	var got []byte
	if err := c.do(ctx, t, cr, http.MethodPost, key, url.Values{"uploadId": {uploadID}}, h, raw, &got); err != nil {
		return err
	}
	// S3 may answer 200 and still fail: the error is in the body.
	if bytes.Contains(got, []byte("<Error>")) {
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		_ = xml.Unmarshal(got, &x)
		return &Error{Status: http.StatusOK, Code: x.Code, Message: x.Message}
	}
	return nil
}

// AbortMultipartUpload drops an unfinished upload and its parts.
func (c *Client) AbortMultipartUpload(ctx context.Context, t Target, cr Credentials, key, uploadID string) error {
	err := c.do(ctx, t, cr, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// MinPartSize is S3's smallest part (but the last).
const MinPartSize = 5 << 20

// UploadFile writes size bytes of r to key, encrypted with sseKey: in one
// PUT up to partSize, else as a multipart upload of partSize parts, one
// part in memory at a time. An unfinished upload of the same key (an
// uploader that stopped half way) is resumed: parts already there with the
// right size are kept. A resumed upload the storage refuses (another key,
// say) is aborted, so the next attempt starts afresh.
func (c *Client) UploadFile(ctx context.Context, t Target, cr Credentials, key string, r io.ReaderAt, size int64, sseKey []byte, partSize int64) error {
	partSize = max(partSize, MinPartSize)
	if size <= partSize {
		buf := make([]byte, size)
		if _, err := r.ReadAt(buf, 0); err != nil && !(errors.Is(err, io.EOF) && size == 0) {
			return err
		}
		return c.PutObject(ctx, t, cr, key, buf, sseKey)
	}
	uploadID, done, err := c.resumable(ctx, t, cr, key)
	if err != nil {
		return err
	}
	resumed := uploadID != ""
	if !resumed {
		if uploadID, err = c.CreateMultipartUpload(ctx, t, cr, key, sseKey); err != nil {
			return err
		}
	}
	n := int((size + partSize - 1) / partSize)
	parts := make([]Part, 0, n)
	buf := make([]byte, partSize)
	for i := 1; i <= n; i++ {
		off := int64(i-1) * partSize
		l := min(partSize, size-off)
		if p, ok := done[i]; ok && p.Size == l && p.ETag != "" {
			parts = append(parts, p)
			continue
		}
		if _, err := r.ReadAt(buf[:l], off); err != nil && !(errors.Is(err, io.EOF) && off+l == size) {
			return err
		}
		etag, err := c.UploadPart(ctx, t, cr, key, uploadID, i, buf[:l], sseKey)
		if err != nil {
			var se *Error
			if resumed && errors.As(err, &se) && se.Status/100 == 4 {
				_ = c.AbortMultipartUpload(ctx, t, cr, key, uploadID)
			}
			return fmt.Errorf("part %d of %d: %w", i, n, err)
		}
		parts = append(parts, Part{Number: i, Size: l, ETag: etag})
	}
	if err := c.CompleteMultipartUpload(ctx, t, cr, key, uploadID, parts, sseKey); err != nil {
		var se *Error
		if errors.As(err, &se) && se.Status/100 == 4 {
			_ = c.AbortMultipartUpload(ctx, t, cr, key, uploadID)
		}
		return err
	}
	return nil
}

// resumable finds the newest unfinished upload of exactly key and its
// parts; older ones of the same key are aborted. Resuming is an
// optimization: a store that cannot list uploads gets a fresh one.
func (c *Client) resumable(ctx context.Context, t Target, cr Credentials, key string) (string, map[int]Part, error) {
	uploads, err := c.ListMultipartUploads(ctx, t, cr, key)
	var se *Error
	if errors.As(err, &se) && (se.Status == http.StatusNotImplemented || se.Status == http.StatusBadRequest) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	uploads = slices.DeleteFunc(uploads, func(u Upload) bool { return u.Key != key })
	if len(uploads) == 0 {
		return "", nil, nil
	}
	slices.SortFunc(uploads, func(a, b Upload) int { return b.Initiated.Compare(a.Initiated) })
	for _, u := range uploads[1:] {
		_ = c.AbortMultipartUpload(ctx, t, cr, key, u.UploadID)
	}
	parts, err := c.ListParts(ctx, t, cr, key, uploads[0].UploadID)
	if IsNotFound(err) || errors.As(err, &se) && (se.Status == http.StatusNotImplemented || se.Status == http.StatusBadRequest) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	done := make(map[int]Part, len(parts))
	for _, p := range parts {
		done[p.Number] = p
	}
	return uploads[0].UploadID, done, nil
}
