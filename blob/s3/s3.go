// Package s3 is a blob.BlobStore backed by any S3-compatible object store
// (AWS S3, MinIO, Google Cloud Storage via its S3 interoperability API). It
// speaks the S3 REST API directly with the standard library — request signing
// is AWS Signature Version 4, implemented here — so there is no SDK dependency.
//
// Because Castlet content-addresses media, the server can serve bytes straight
// from the object store: URL returns a short-lived presigned GET link and the
// media handler redirects to it, so audio never streams through Castlet.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/castletfm/castlet/blob"
)

// emptyPayload is sha256("") — the content hash for a request with no body.
const emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Config configures New. It is JSON-decodable so it can be supplied via a blob
// store config file.
type Config struct {
	Endpoint  string `json:"endpoint"`   // base URL, e.g. https://s3.amazonaws.com or http://127.0.0.1:9000
	Region    string `json:"region"`     // signing region; defaults to us-east-1
	Bucket    string `json:"bucket"`     //
	AccessKey string `json:"access_key"` //
	SecretKey string `json:"secret_key"` //
	Prefix    string `json:"prefix"`     // optional key prefix within the bucket (e.g. "media/")
}

// Store is an S3-compatible BlobStore. It addresses objects path-style
// (endpoint/bucket/key), which every S3-compatible store accepts.
type Store struct {
	cfg    Config
	client *http.Client
	scheme string
	host   string
}

var (
	_ blob.BlobStore = (*Store)(nil)
	_ blob.DirectURL = (*Store)(nil)
)

// New validates cfg and returns a Store. It does not contact the store.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3: endpoint, bucket, access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("s3: invalid endpoint %q", cfg.Endpoint)
	}
	return &Store{cfg: cfg, client: &http.Client{}, scheme: u.Scheme, host: u.Host}, nil
}

// objectPath is the path-style request path: /bucket/prefix+key.
func (s *Store) objectPath(key string) string {
	return "/" + s.cfg.Bucket + "/" + s.cfg.Prefix + key
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	// Buffer to a temp file so we can compute the payload hash (required to sign
	// the request) and the content length, and replay the body if needed.
	tmp, err := os.CreateTemp("", "castlet-s3put-*")
	if err != nil {
		return 0, fmt.Errorf("s3: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return 0, fmt.Errorf("s3: buffer upload: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	req, err := s.signedRequest(ctx, http.MethodPut, key, nil, tmp, hex.EncodeToString(hasher.Sum(nil)), n, time.Now())
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("s3: put %q: %w", key, err)
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 {
		return 0, statusError("put", key, resp)
	}
	return n, nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	req, err := s.signedRequest(ctx, http.MethodGet, key, nil, nil, emptyPayload, 0, time.Now())
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("s3: get %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return nil, 0, blob.ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		err := statusError("get", key, resp)
		drain(resp)
		return nil, 0, err
	}
	// The interface promises a seekable reader (for HTTP range serving), but an
	// S3 body is a stream; buffer it to a temp file that cleans itself up.
	tmp, err := os.CreateTemp("", "castlet-s3get-*")
	if err != nil {
		drain(resp)
		return nil, 0, fmt.Errorf("s3: temp file: %w", err)
	}
	n, err := io.Copy(tmp, resp.Body)
	resp.Body.Close()
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, fmt.Errorf("s3: download %q: %w", key, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	return &tempReader{tmp}, n, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	req, err := s.signedRequest(ctx, http.MethodDelete, key, nil, nil, emptyPayload, 0, time.Now())
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	defer drain(resp)
	// S3 delete is idempotent: 204 on success, 404 if already gone.
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return statusError("delete", key, resp)
	}
	return nil
}

// URL returns a presigned GET URL valid for 15 minutes, with the response
// Content-Type and Content-Disposition overridden so the client receives the
// sanitized media type and (for hardening) the requested disposition.
//
// S3 presigned GET overrides can only set response-* parameters that map to
// response headers S3 supports — response-content-type and
// response-content-disposition among them — but NOT arbitrary headers, so
// X-Content-Type-Options: nosniff cannot be forced onto the S3 response via the
// URL. The direct path's XSS mitigation is therefore the caller's content-type
// coercion (unsafe types become application/octet-stream) plus the attachment
// disposition signed here.
func (s *Store) URL(ctx context.Context, key, contentType, contentDisposition string) (string, error) {
	t := time.Now().UTC()
	scope := t.Format("20060102") + "/" + s.cfg.Region + "/s3/aws4_request"
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", s.cfg.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", t.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", "900")
	q.Set("X-Amz-SignedHeaders", "host")
	// These response-* overrides must be part of the signed canonical query
	// (canonicalQuery below feeds both the signature and the returned URL).
	if contentType != "" {
		q.Set("response-content-type", contentType)
	}
	if contentDisposition != "" {
		q.Set("response-content-disposition", contentDisposition)
	}
	path := s.objectPath(key)
	canonical := strings.Join([]string{
		http.MethodGet,
		uriEncodePath(path),
		canonicalQuery(q),
		"host:" + s.host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	q.Set("X-Amz-Signature", s.sign(t, canonical))
	return s.scheme + "://" + s.host + path + "?" + canonicalQuery(q), nil
}

// signedRequest builds an HTTP request and adds a SigV4 Authorization header.
func (s *Store) signedRequest(ctx context.Context, method, key string, query url.Values, body io.Reader, payloadHash string, size int64, t time.Time) (*http.Request, error) {
	t = t.UTC()
	rawURL := s.scheme + "://" + s.host + s.objectPath(key)
	if len(query) > 0 {
		rawURL += "?" + canonicalQuery(query)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	amzdate := t.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzdate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	scope := t.Format("20060102") + "/" + s.cfg.Region + "/s3/aws4_request"
	canonical := strings.Join([]string{
		method,
		uriEncodePath(req.URL.Path),
		canonicalQuery(query),
		"host:" + s.host + "\n" + "x-amz-content-sha256:" + payloadHash + "\n" + "x-amz-date:" + amzdate + "\n",
		"host;x-amz-content-sha256;x-amz-date",
		payloadHash,
	}, "\n")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.cfg.AccessKey+"/"+scope+
		", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+s.sign(t, canonical))
	return req, nil
}

// sign turns a canonical request into a SigV4 signature.
func (s *Store) sign(t time.Time, canonicalRequest string) string {
	datestamp := t.Format("20060102")
	scope := datestamp + "/" + s.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		t.Format("20060102T150405Z"),
		scope,
		sha256hex([]byte(canonicalRequest)),
	}, "\n")
	key := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), []byte(datestamp))
	key = hmacSHA256(key, []byte(s.cfg.Region))
	key = hmacSHA256(key, []byte("s3"))
	key = hmacSHA256(key, []byte("aws4_request"))
	return hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalQuery renders query params RFC3986-encoded then sorted, the form
// SigV4 requires (and a valid query string for the actual request too). SigV4
// mandates encoding each name and value first and sorting by the encoded bytes
// (name, then value): encoding can change the ordering (e.g. '%5B' < 'A'), so
// sorting the raw params first would produce a wrong canonical query.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	type pair struct{ name, value string }
	pairs := make([]pair, 0, len(q))
	for k, vals := range q {
		ek := uriEncode(k)
		for _, v := range vals {
			pairs = append(pairs, pair{ek, uriEncode(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].name != pairs[j].name {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].value < pairs[j].value
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.name + "=" + p.value
	}
	return strings.Join(parts, "&")
}

// uriEncodePath encodes a path, leaving '/' as segment separators.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg)
	}
	return strings.Join(segs, "/")
}

// uriEncode percent-encodes per RFC 3986 (AWS-style: only unreserved chars are
// left literal).
func uriEncode(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func statusError(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("s3: %s %q: %s: %s", op, key, resp.Status, strings.TrimSpace(string(body)))
}

func drain(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
	}
}

// tempReader is a seekable reader over a temp file that removes the file on Close.
type tempReader struct{ f *os.File }

func (t *tempReader) Read(p []byte) (int, error)                { return t.f.Read(p) }
func (t *tempReader) Seek(off int64, whence int) (int64, error) { return t.f.Seek(off, whence) }
func (t *tempReader) Close() error {
	name := t.f.Name()
	err := t.f.Close()
	os.Remove(name)
	return err
}
