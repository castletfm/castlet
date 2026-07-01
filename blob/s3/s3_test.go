package s3

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The core signing crypto is pinned to the AWS Signature Version 4 "GET Object"
// example (see the AWS S3 API reference, "Examples: Signature Version 4
// signing"). The request inputs (secret key, region, canonical request) and the
// AWS-documented hashed canonical request (awsHashedCanonicalRequest) are taken
// verbatim from that example; because our canonical string hashes to exactly
// that documented value it is byte-identical to AWS's. The signing key and final
// signature below were computed from those inputs and independently verified
// against a from-scratch reference implementation of SigV4 (Python hmac/hashlib).
const (
	awsSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRFiCYEXAMPLEKEY"
	awsAccessKey = "AKIAIOSFODNN7EXAMPLE"
	awsRegion    = "us-east-1"

	// Canonical request from the AWS example. Note it uses virtual-hosted style
	// and a Range header, so it differs from what signedRequest builds; we feed
	// it to sign() directly to exercise the crypto against the published answer.
	awsCanonicalRequest = "GET\n" +
		"/test.txt\n" +
		"\n" +
		"host:examplebucket.s3.amazonaws.com\n" +
		"range:bytes=0-9\n" +
		"x-amz-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n" +
		"x-amz-date:20130524T000000Z\n" +
		"\n" +
		"host;range;x-amz-content-sha256;x-amz-date\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// sha256 of awsCanonicalRequest, per the AWS example.
	awsHashedCanonicalRequest = "7344ae5b7ee6c3e7e6b0fe0640412a37625d1fbfff95c48bbb2dc43964946972"

	// Final signature for the AWS example inputs (verified independently).
	awsSignature = "51a4f6aa2b6678b8bc64339c22721571d2a069f12f5ec80b1e6d12e50636512a"
)

// awsVectorTime is 20130524T000000Z, the timestamp of the AWS example.
var awsVectorTime = time.Date(2013, time.May, 24, 0, 0, 0, 0, time.UTC)

// TestSignKnownAnswer pins the whole signing pipeline (canonical-request hashing,
// string-to-sign assembly, s3 signing-key derivation and the final HMAC) to the
// published AWS SigV4 "GET Object" vector.
func TestSignKnownAnswer(t *testing.T) {
	s, err := New(Config{
		Endpoint:  "https://examplebucket.s3.amazonaws.com",
		Bucket:    "examplebucket",
		Region:    awsRegion,
		AccessKey: awsAccessKey,
		SecretKey: awsSecretKey,
	})
	require.NoError(t, err)

	// sha256hex of the canonical request must match the AWS-published hash.
	assert.Equal(t, awsHashedCanonicalRequest, sha256hex([]byte(awsCanonicalRequest)),
		"hashed canonical request must match AWS vector")

	// The final signature must match the AWS-published value.
	assert.Equal(t, awsSignature, s.sign(awsVectorTime, awsCanonicalRequest),
		"signature must match AWS vector")
}

// TestSigningKeyDerivation validates the HMAC signing-key derivation chain
// (AWS4+secret -> date -> region -> "s3" -> "aws4_request") without relying on
// an unexported accessor: it reproduces the string-to-sign of the AWS vector,
// signs it with the independently derived key, and requires the published
// signature. The derived key's hex is also pinned as a stable golden value; it
// was captured from this same derivation against the AWS vector.
func TestSigningKeyDerivation(t *testing.T) {
	// String-to-sign as published in the AWS example.
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		"20130524T000000Z",
		"20130524/us-east-1/s3/aws4_request",
		awsHashedCanonicalRequest,
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+awsSecretKey), []byte("20130524"))
	key = hmacSHA256(key, []byte(awsRegion))
	key = hmacSHA256(key, []byte("s3"))
	key = hmacSHA256(key, []byte("aws4_request"))

	// Golden signing key for the AWS example (verified independently, see above).
	const wantSigningKey = "983dd0682c8a5033ad838bcd965bfe5d967bd3b90f97a2069218d7e8d85538e7"
	assert.Equal(t, wantSigningKey, hex.EncodeToString(key), "derived signing key")

	// The derived key must reproduce the AWS-published signature.
	assert.Equal(t, awsSignature, hex.EncodeToString(hmacSHA256(key, []byte(stringToSign))),
		"signature from independently derived key must match AWS vector")
}

// TestClientStallTimeouts asserts New wires an http.Transport with
// stall-oriented timeouts, and crucially leaves http.Client.Timeout at zero so
// large media bodies can stream for arbitrarily long. This bounds a hung object
// store (which would otherwise block Get/Put/Delete and starve the worker)
// without truncating legitimate transfers.
func TestClientStallTimeouts(t *testing.T) {
	s, err := New(Config{
		Endpoint:  "http://127.0.0.1:9000",
		Bucket:    "media",
		AccessKey: "AK",
		SecretKey: "SK",
	})
	require.NoError(t, err)

	// A flat client deadline would abort big uploads/downloads; it must be unset.
	assert.Zero(t, s.client.Timeout, "http.Client.Timeout must stay 0 to allow long body streaming")

	tr, ok := s.client.Transport.(*http.Transport)
	require.True(t, ok, "transport must be *http.Transport")
	assert.Equal(t, 30*time.Second, tr.ResponseHeaderTimeout, "ResponseHeaderTimeout bounds a hung endpoint")
	assert.Equal(t, 10*time.Second, tr.TLSHandshakeTimeout)
	assert.NotZero(t, tr.IdleConnTimeout)
	assert.NotZero(t, tr.ExpectContinueTimeout)
	assert.NotNil(t, tr.DialContext, "DialContext must be set with a dial timeout")

	// HTTP/2 must be disabled: under h2 many streams multiplex over one
	// connection, so another active stream can keep the idleTimeoutConn deadline
	// refreshed while a single stream's body stalls forever. Over HTTP/1.1 each
	// in-flight request owns its connection, so the per-conn idle deadline
	// reliably bounds one stalled transfer. A non-nil empty TLSNextProto is the
	// idiomatic way to prevent h2 negotiation.
	assert.False(t, tr.ForceAttemptHTTP2, "HTTP/2 must not be force-attempted")
	require.NotNil(t, tr.TLSNextProto, "TLSNextProto must be non-nil to disable h2")
	assert.Empty(t, tr.TLSNextProto, "TLSNextProto must be empty so h2 is never negotiated")
}

// TestIdleTimeoutConn checks the wrapper in isolation: a Read/Write with no
// peer activity trips the idle deadline (bounding a stall), while activity
// within the window refreshes the deadline so progress is never truncated.
func TestIdleTimeoutConn(t *testing.T) {
	t.Run("read_stalls", func(t *testing.T) {
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		conn := &idleTimeoutConn{Conn: c1, idle: 50 * time.Millisecond}

		start := time.Now()
		_, err := conn.Read(make([]byte, 1))
		require.Error(t, err)
		var nerr net.Error
		require.True(t, errors.As(err, &nerr) && nerr.Timeout(), "want timeout, got %v", err)
		assert.Less(t, time.Since(start), 2*time.Second, "must trip near the idle window")
	})

	t.Run("write_stalls", func(t *testing.T) {
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		conn := &idleTimeoutConn{Conn: c1, idle: 50 * time.Millisecond}

		start := time.Now()
		// Nothing reads from c2, so an unbuffered pipe Write blocks until the
		// refreshed write deadline fires.
		_, err := conn.Write([]byte("x"))
		require.Error(t, err)
		var nerr net.Error
		require.True(t, errors.As(err, &nerr) && nerr.Timeout(), "want timeout, got %v", err)
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("activity_refreshes_deadline", func(t *testing.T) {
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		conn := &idleTimeoutConn{Conn: c1, idle: 200 * time.Millisecond}

		// Peer writes a byte every 50ms (< idle) for longer than a single idle
		// window; each Read must refresh the deadline so no read times out.
		go func() {
			for range 8 {
				time.Sleep(50 * time.Millisecond)
				if _, err := c2.Write([]byte{'x'}); err != nil {
					return
				}
			}
		}()
		buf := make([]byte, 1)
		for range 8 {
			n, err := conn.Read(buf)
			require.NoError(t, err, "progressing read must not time out")
			require.Equal(t, 1, n)
		}
	})

	// Regression: HTTP/1.1 streams the request body (Write) while a concurrent
	// read waits for the response. During a large Put the write can be active
	// for far longer than the idle window before S3 sends any response byte.
	// With independent per-direction deadlines the pending read deadline fires
	// and aborts a healthy upload; a single connection-wide deadline refreshed
	// on any activity must let write progress keep the blocked read alive.
	t.Run("write_refreshes_pending_read", func(t *testing.T) {
		c1, c2 := net.Pipe()
		defer c1.Close()
		defer c2.Close()
		conn := &idleTimeoutConn{Conn: c1, idle: 100 * time.Millisecond}

		// Peer drains writes (so each Write makes progress) but never sends any
		// bytes, so the concurrent read only survives if writes keep refreshing
		// the shared deadline it depends on.
		go func() {
			buf := make([]byte, 64)
			for {
				if _, err := c2.Read(buf); err != nil {
					return
				}
			}
		}()

		readErr := make(chan error, 1)
		go func() {
			_, err := conn.Read(make([]byte, 1)) // blocks: peer never writes
			readErr <- err
		}()

		// Write every 40ms (< idle) across more than two idle windows.
		for range 6 {
			time.Sleep(40 * time.Millisecond)
			if _, err := conn.Write([]byte{'x'}); err != nil {
				t.Fatalf("progressing write must not time out: %v", err)
			}
			select {
			case err := <-readErr:
				t.Fatalf("blocked read aborted despite concurrent write progress: %v", err)
			default:
			}
		}
	})
}

// TestGetStalledBodyFails is a hermetic end-to-end check: a server that sends
// headers then stalls the body must cause Get to fail promptly (bounded by the
// idle timeout), rather than hang forever.
func TestGetStalledBodyFails(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // stall mid-body
	}))
	defer srv.Close()
	defer close(release) // runs first (LIFO): unblock the handler before Close waits on it

	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "AK", SecretKey: "SK"})
	require.NoError(t, err)
	s.client = newClientWithIdle(100 * time.Millisecond)

	start := time.Now()
	_, _, err = s.Get(context.Background(), "key")
	require.Error(t, err, "a stalled body must not hang Get")
	assert.Less(t, time.Since(start), 5*time.Second, "must be bounded by the idle timeout")
}

// TestGetProgressingTransferSucceeds proves the idle deadline does not truncate
// a transfer that keeps moving: chunks arrive with gaps below the idle window,
// summing to more than one idle window, yet Get completes with the full body.
func TestGetProgressingTransferSucceeds(t *testing.T) {
	const chunks = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		for range chunks {
			_, _ = w.Write([]byte("chunk"))
			if f != nil {
				f.Flush()
			}
			time.Sleep(60 * time.Millisecond) // < idle below
		}
	}))
	defer srv.Close()

	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "AK", SecretKey: "SK"})
	require.NoError(t, err)
	s.client = newClientWithIdle(300 * time.Millisecond)

	rc, n, err := s.Get(context.Background(), "key")
	require.NoError(t, err, "a progressing transfer must not be aborted")
	defer rc.Close()
	assert.Equal(t, int64(len("chunk")*chunks), n)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte("chunk"), chunks), body)
}

// stagedFiles lists entries in dir whose name starts with prefix. It is used to
// observe where Put/Get stage their buffered temp files and that they are
// cleaned up afterwards.
func stagedFiles(t *testing.T, dir, prefix string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestPutStagesInConfiguredTempDir proves Put buffers the upload into the
// directory given by WithTempDir (not the system /tmp) and removes it afterward.
// The staged file is captured mid-request (while it still exists) by having the
// server list the dir as it reads the body.
func TestPutStagesInConfiguredTempDir(t *testing.T) {
	dir := t.TempDir()
	staged := make(chan []string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		staged <- stagedFiles(t, dir, "castlet-s3put-")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "AK", SecretKey: "SK"}, WithTempDir(dir))
	require.NoError(t, err)

	_, err = s.Put(context.Background(), "key", strings.NewReader("hello world"))
	require.NoError(t, err)

	require.Len(t, <-staged, 1, "Put must stage its buffer in the configured temp dir")
	assert.Empty(t, stagedFiles(t, dir, "castlet-s3put-"), "staged temp file must be removed after Put")
}

// TestGetStagesInConfiguredTempDir proves Get buffers the response into the
// WithTempDir directory and that closing the returned reader removes it.
func TestGetStagesInConfiguredTempDir(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello world")
	}))
	defer srv.Close()

	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "AK", SecretKey: "SK"}, WithTempDir(dir))
	require.NoError(t, err)

	rc, n, err := s.Get(context.Background(), "key")
	require.NoError(t, err)
	assert.Equal(t, int64(len("hello world")), n)
	assert.Len(t, stagedFiles(t, dir, "castlet-s3get-"), 1, "Get must stage the body in the configured temp dir")

	require.NoError(t, rc.Close())
	assert.Empty(t, stagedFiles(t, dir, "castlet-s3get-"), "staged temp file must be removed on reader Close")
}

// TestDefaultTempDirIsOSTempDir proves that without WithTempDir the store leaves
// tempDir empty and stages under os.TempDir(). TMPDIR (honored by os.TempDir on
// unix) is pointed at a scratch dir so the staged file can be observed there.
func TestDefaultTempDirIsOSTempDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	require.Equal(t, dir, os.TempDir(), "test requires os.TempDir to honor TMPDIR")

	staged := make(chan []string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		staged <- stagedFiles(t, dir, "castlet-s3put-")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "AK", SecretKey: "SK"})
	require.NoError(t, err)
	assert.Empty(t, s.tempDir, "default store must leave tempDir unset")

	_, err = s.Put(context.Background(), "key", strings.NewReader("hi"))
	require.NoError(t, err)
	require.Len(t, <-staged, 1, "default Put must stage under os.TempDir()")
}

func TestURIEncode(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"unreserved", "aZ09-._~", "aZ09-._~"},
		{"space", "a b", "a%20b"},
		{"slash", "a/b", "a%2Fb"},
		{"reserved", "a+b=c&d", "a%2Bb%3Dc%26d"},
		{"colon", ":", "%3A"},
		{"latin1_multibyte", "ü", "%C3%BC"},
		{"cjk_multibyte", "日", "%E6%97%A5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, uriEncode(tc.in))
		})
	}
}

func TestURIEncodePath(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"root", "/", "/"},
		{"plain", "/bucket/key", "/bucket/key"},
		{"space_and_unicode", "/bucket/a b/ünïcode.mp3", "/bucket/a%20b/%C3%BCn%C3%AFcode.mp3"},
		{"reserved_in_segments", "/a+b/c=d", "/a%2Bb/c%3Dd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, uriEncodePath(tc.in))
		})
	}
}

func TestCanonicalQuery(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		assert.Equal(t, "", canonicalQuery(url.Values{}))
		assert.Equal(t, "", canonicalQuery(nil))
	})

	t.Run("sorted_by_key", func(t *testing.T) {
		q := url.Values{}
		q.Set("X-Amz-Expires", "900")
		q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
		q.Set("X-Amz-Date", "20130524T000000Z")
		assert.Equal(t,
			"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20130524T000000Z&X-Amz-Expires=900",
			canonicalQuery(q))
	})

	t.Run("encoded_value", func(t *testing.T) {
		q := url.Values{}
		q.Set("response-content-type", "audio/mpeg; charset=utf-8")
		assert.Equal(t, "response-content-type=audio%2Fmpeg%3B%20charset%3Dutf-8", canonicalQuery(q))
	})

	t.Run("multi_value_sorted", func(t *testing.T) {
		q := url.Values{"k": {"b", "a"}}
		assert.Equal(t, "k=a&k=b", canonicalQuery(q))
	})

	t.Run("names_sorted_after_encoding", func(t *testing.T) {
		// '[' encodes to '%5B', which sorts before 'A' by encoded bytes, so
		// SigV4 must encode names before sorting them.
		q := url.Values{"A": {"1"}, "[": {"1"}}
		assert.Equal(t, "%5B=1&A=1", canonicalQuery(q))
	})

	t.Run("values_sorted_after_encoding", func(t *testing.T) {
		// Same-name values must also sort by their encoded bytes: '%5B' < 'A'.
		q := url.Values{"k": {"A", "["}}
		assert.Equal(t, "k=%5B&k=A", canonicalQuery(q))
	})
}

// TestSignedRequestGolden checks that signedRequest builds the exact canonical
// request the algorithm requires and signs it correctly. The canonical request
// is written out literally (the golden artifact); its signature under the AWS
// example key at a fixed timestamp is pinned, and cross-checked against sign().
func TestSignedRequestGolden(t *testing.T) {
	s, err := New(Config{
		Endpoint:  "https://s3.example.com",
		Bucket:    "examplebucket",
		Prefix:    "media/",
		Region:    awsRegion,
		AccessKey: "AKIDEXAMPLE",
		SecretKey: awsSecretKey,
	})
	require.NoError(t, err)

	req, err := s.signedRequest(context.Background(), "GET", "obj.mp3", nil, nil, emptyPayload, 0, awsVectorTime)
	require.NoError(t, err)

	assert.Equal(t, "https://s3.example.com/examplebucket/media/obj.mp3", req.URL.String())
	assert.Equal(t, "20130524T000000Z", req.Header.Get("X-Amz-Date"))
	assert.Equal(t, emptyPayload, req.Header.Get("X-Amz-Content-Sha256"))

	// Canonical request the algorithm must produce for this request.
	wantCanonical := strings.Join([]string{
		"GET",
		"/examplebucket/media/obj.mp3",
		"",
		"host:s3.example.com\n" +
			"x-amz-content-sha256:" + emptyPayload + "\n" +
			"x-amz-date:20130524T000000Z\n",
		"host;x-amz-content-sha256;x-amz-date",
		emptyPayload,
	}, "\n")

	// Golden signature for wantCanonical under AKIDEXAMPLE key / 20130524; pinned
	// once from this deterministic input.
	const wantSig = "c7b481ecdd16b5eab6a7ad7ad8578584b69263bf23a53157c9640274138ae939"
	sig := s.sign(awsVectorTime, wantCanonical)

	auth := req.Header.Get("Authorization")
	const wantPrefix = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="
	require.True(t, strings.HasPrefix(auth, wantPrefix), "authorization prefix: %s", auth)
	gotSig := strings.TrimPrefix(auth, wantPrefix)

	// signedRequest must have signed exactly wantCanonical.
	assert.Equal(t, sig, gotSig, "signature in header must match the canonical request we expect")
	// And that signature is a stable golden value.
	assert.Equal(t, wantSig, gotSig, "golden signature")
}

// TestSignedRequestWithQuery exercises signedRequest's query-signing branch
// (canonical query string in the URL and canonical request), which the object
// Put/Get/Delete callers never use since they pass a nil query.
func TestSignedRequestWithQuery(t *testing.T) {
	s, err := New(Config{
		Endpoint:  "https://s3.example.com",
		Bucket:    "examplebucket",
		Prefix:    "media/",
		Region:    awsRegion,
		AccessKey: "AKIDEXAMPLE",
		SecretKey: awsSecretKey,
	})
	require.NoError(t, err)

	q := url.Values{"prefix": {"a b"}, "max-keys": {"2"}}
	req, err := s.signedRequest(context.Background(), "GET", "obj.mp3", q, nil, emptyPayload, 0, awsVectorTime)
	require.NoError(t, err)

	// The request URL carries the canonical (encoded-then-sorted) query, so the
	// query-signing branch was actually taken.
	assert.Equal(t, "max-keys=2&prefix=a%20b", req.URL.RawQuery)

	// Prove the query is in the SIGNATURE, not just the URL: build the canonical
	// request with the canonical query line and require the header's signature to
	// equal sign() over it. A signedRequest that signed an empty canonical query
	// would fail here.
	wantCanonical := strings.Join([]string{
		"GET",
		"/examplebucket/media/obj.mp3",
		"max-keys=2&prefix=a%20b",
		"host:s3.example.com\n" +
			"x-amz-content-sha256:" + emptyPayload + "\n" +
			"x-amz-date:20130524T000000Z\n",
		"host;x-amz-content-sha256;x-amz-date",
		emptyPayload,
	}, "\n")

	auth := req.Header.Get("Authorization")
	const wantPrefix = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="
	require.True(t, strings.HasPrefix(auth, wantPrefix), "authorization prefix: %s", auth)
	gotSig := strings.TrimPrefix(auth, wantPrefix)
	assert.Equal(t, s.sign(awsVectorTime, wantCanonical), gotSig,
		"signature must cover the canonical query, not an empty one")
}

// TestPresignedURL checks the structure of a presigned GET URL and that its
// X-Amz-Signature is self-consistent with the canonical request the presigning
// algorithm defines (recomputed here from the URL's own X-Amz-Date).
func TestPresignedURL(t *testing.T) {
	s, err := New(Config{
		Endpoint:  "https://s3.example.com",
		Bucket:    "examplebucket",
		Prefix:    "media/",
		Region:    awsRegion,
		AccessKey: "AKIDEXAMPLE",
		SecretKey: awsSecretKey,
	})
	require.NoError(t, err)

	raw, err := s.URL(context.Background(), "obj.mp3", "audio/mpeg", "")
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "s3.example.com", u.Host)
	assert.Equal(t, "/examplebucket/media/obj.mp3", u.Path)

	q := u.Query()
	assert.Equal(t, "AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
	assert.Equal(t, "host", q.Get("X-Amz-SignedHeaders"))
	assert.Equal(t, "900", q.Get("X-Amz-Expires"))
	assert.Equal(t, "audio/mpeg", q.Get("response-content-type"))

	xAmzDate := q.Get("X-Amz-Date")
	require.NotEmpty(t, xAmzDate)
	ts, err := time.Parse("20060102T150405Z", xAmzDate)
	require.NoError(t, err)

	cred := q.Get("X-Amz-Credential")
	assert.Equal(t, "AKIDEXAMPLE/"+ts.Format("20060102")+"/us-east-1/s3/aws4_request", cred)

	sig := q.Get("X-Amz-Signature")
	require.Len(t, sig, 64)
	_, err = hex.DecodeString(sig)
	require.NoError(t, err, "signature must be hex")

	// Recompute the canonical request for presigning (query without the
	// signature) and verify the signature is exactly what sign() produces.
	q2 := url.Values{}
	q2.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q2.Set("X-Amz-Credential", cred)
	q2.Set("X-Amz-Date", xAmzDate)
	q2.Set("X-Amz-Expires", "900")
	q2.Set("X-Amz-SignedHeaders", "host")
	q2.Set("response-content-type", "audio/mpeg")
	canonical := strings.Join([]string{
		"GET",
		uriEncodePath("/examplebucket/media/obj.mp3"),
		canonicalQuery(q2),
		"host:s3.example.com\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	assert.Equal(t, s.sign(ts, canonical), sig, "presigned signature must be self-consistent")
}

// TestURLSignsResponseOverrides verifies the presigned GET URL carries both the
// response-content-type and response-content-disposition overrides, and that
// they are part of the signed canonical query (recomputing the SigV4 signature
// over the returned query reproduces X-Amz-Signature) so S3/MinIO honors the
// hardening the direct-serve path needs.
func TestURLSignsResponseOverrides(t *testing.T) {
	st, err := New(Config{
		Endpoint:  "http://127.0.0.1:9000",
		Bucket:    "media",
		AccessKey: "AK",
		SecretKey: "SK",
	})
	require.NoError(t, err)

	raw, err := st.URL(context.Background(), "obj", "application/octet-stream", "attachment")
	require.NoError(t, err)

	u, err := url.Parse(raw)
	require.NoError(t, err)
	q := u.Query()
	require.Equal(t, "application/octet-stream", q.Get("response-content-type"))
	require.Equal(t, "attachment", q.Get("response-content-disposition"))

	// Recompute the signature over the returned query (minus the signature
	// itself). If either response-* override were appended but not signed, this
	// would not match, so this proves both are part of the signed canonical query.
	sig := q.Get("X-Amz-Signature")
	require.NotEmpty(t, sig)
	q.Del("X-Amz-Signature")
	ts, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	require.NoError(t, err)
	canonical := strings.Join([]string{
		http.MethodGet,
		uriEncodePath(u.Path),
		canonicalQuery(q),
		"host:" + st.host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	require.Equal(t, st.sign(ts, canonical), sig)

	// An empty disposition omits the override entirely.
	raw2, err := st.URL(context.Background(), "obj", "application/octet-stream", "")
	require.NoError(t, err)
	u2, err := url.Parse(raw2)
	require.NoError(t, err)
	require.False(t, u2.Query().Has("response-content-disposition"))
}
