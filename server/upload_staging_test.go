package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/store"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingBlobs wraps a BlobStore and records the directory of the *os.File
// handed to Put, so a test can prove the upload was staged under the configured
// upload temp dir rather than the system /tmp.
type recordingBlobs struct {
	blob.BlobStore
	putDir string
}

func (r *recordingBlobs) Put(ctx context.Context, key string, rd io.Reader) (int64, error) {
	if f, ok := rd.(*os.File); ok {
		r.putDir = filepath.Dir(f.Name())
	}
	return r.BlobStore.Put(ctx, key, rd)
}

// newUploadServer builds a Server wired to a real sqlite store, a localfs blob
// store (wrapped to record staging), a real queue, and a temp upload staging
// dir, with a seeded owner (u1) and channel (c1).
func newUploadServer(t *testing.T) (s *Server, st store.Store, blobs *recordingBlobs, uploadDir string) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c", DisplayName: "A", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	fsBlobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	blobs = &recordingBlobs{BlobStore: fsBlobs}

	renderer, err := newTemplateRenderer()
	require.NoError(t, err)

	uploadDir = t.TempDir()
	s = &Server{
		store:          st,
		blobs:          blobs,
		queue:          dbqueue.New(st),
		renderer:       renderer,
		logger:         slog.New(slog.DiscardHandler),
		maxUploadBytes: 1 << 20,
		uploadTempDir:  uploadDir,
		now:            time.Now,
	}
	return s, st, blobs, uploadDir
}

// filePart describes one file part for buildUpload.
type filePart struct {
	field, filename, contentType string
	content                      []byte
}

// buildUpload builds a multipart/form-data body with the given text fields and
// file parts (fields first, then files).
func buildUpload(t *testing.T, fields map[string]string, files []filePart) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	for _, f := range files {
		h := map[string][]string{
			"Content-Disposition": {`form-data; name="` + f.field + `"; filename="` + f.filename + `"`},
			"Content-Type":        {f.contentType},
		}
		part, err := mw.CreatePart(h)
		require.NoError(t, err)
		_, err = part.Write(f.content)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return &buf, mw.FormDataContentType()
}

// newUploadRequest builds a POST to the episode-create route with the given
// multipart body, authenticated as u1 for channel c1.
func newUploadRequest(body *bytes.Buffer, contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/c1/episodes", body)
	req.Header.Set("Content-Type", contentType)
	req.SetPathValue("id", "c1")
	return req.WithContext(context.WithValue(req.Context(), userCtxKey, &model.User{ID: "u1"}))
}

// assertNoStagedFiles fails if any staged upload temp file lingers in dir.
func assertNoStagedFiles(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range ents {
		assert.NotContains(t, e.Name(), "castlet-upload-", "staged upload temp file must be removed")
	}
}

// TestUploadStagesUnderConfiguredTempDir proves an episode upload streams the
// media into the configured upload temp dir (not os.TempDir), stores it content
// -addressed (MediaKey = sha256), creates the episode, and removes the staged
// temp file afterward.
func TestUploadStagesUnderConfiguredTempDir(t *testing.T) {
	s, st, blobs, uploadDir := newUploadServer(t)
	ctx := t.Context()

	content := []byte("ID3 fake mp3 payload for staging test")
	sum := sha256.Sum256(content)
	wantKey := hex.EncodeToString(sum[:])

	body, contentType := buildUpload(t, map[string]string{"title": "Hello", "language": "en"},
		[]filePart{{field: "media", filename: "clip.mp3", contentType: "audio/mpeg", content: content}})
	req := newUploadRequest(body, contentType)
	rec := httptest.NewRecorder()

	s.handleEpisodeCreate(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code, "a valid upload must redirect")

	// The media was staged under the configured upload dir, not os.TempDir().
	assert.Equal(t, uploadDir, blobs.putDir, "media must be staged under the configured upload temp dir")
	assert.NotEqual(t, os.TempDir(), blobs.putDir, "upload must not stage into the system /tmp")

	// The staged temp file is removed after the handler returns.
	assertNoStagedFiles(t, uploadDir)

	// The episode was created, content-addressed by the sha256 of the bytes.
	eps, err := st.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, wantKey, eps[0].MediaKey, "MediaKey must be the sha256 of the uploaded bytes")
	assert.EqualValues(t, len(content), eps[0].MediaBytes)
	assert.Equal(t, model.MediaAudio, eps[0].MediaKind)

	// And the bytes are actually stored under that key.
	rc, n, err := blobs.Get(ctx, wantKey)
	require.NoError(t, err)
	defer rc.Close()
	assert.EqualValues(t, len(content), n)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

// TestUploadUnknownFieldIgnored proves an unknown non-file field is discarded
// (not retained, not rejected): the upload still succeeds using the known fields.
func TestUploadUnknownFieldIgnored(t *testing.T) {
	s, st, _, _ := newUploadServer(t)
	ctx := t.Context()

	body, contentType := buildUpload(t,
		map[string]string{"title": "Hello", "language": "en", "surprise": "ignored", "bogus": "also ignored"},
		[]filePart{{field: "media", filename: "clip.mp3", contentType: "audio/mpeg", content: []byte("audio bytes")}})
	req := newUploadRequest(body, contentType)
	rec := httptest.NewRecorder()

	s.handleEpisodeCreate(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code, "unknown fields must be ignored, not rejected")
	eps, err := st.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, "Hello", eps[0].Title)
	assert.Equal(t, "en", eps[0].Language)
}

// TestUploadOversizedFieldAbortsFast proves an oversized text field is rejected
// promptly (4xx) with the connection aborted — deadline revoked and marked to
// close — rather than draining the field under the long upload deadline.
func TestUploadOversizedFieldAbortsFast(t *testing.T) {
	s, _, _, uploadDir := newUploadServer(t)

	big := strings.Repeat("x", maxUploadFieldBytes+1)
	body, contentType := buildUpload(t, map[string]string{"title": "Hello", "description": big},
		[]filePart{{field: "media", filename: "clip.mp3", contentType: "audio/mpeg", content: []byte("data")}})
	req := newUploadRequest(body, contentType)
	rec := httptest.NewRecorder()
	fake := &deadlineWriter{ResponseWriter: rec}

	s.handleEpisodeCreate(fake, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "an oversized field must be a client error")
	require.Equal(t, "close", rec.Header().Get("Connection"), "the connection must be marked to close, not drained")
	require.True(t, fake.called, "the long read deadline must be revoked")
	require.True(t, fake.deadline.Before(time.Now().Add(time.Minute)), "the read deadline must be expired")
	assertNoStagedFiles(t, uploadDir)
}

// TestUploadDuplicateMediaAbortsFast proves a second media file part is rejected
// (without draining it) and the already-staged first part is cleaned up.
func TestUploadDuplicateMediaAbortsFast(t *testing.T) {
	s, _, _, uploadDir := newUploadServer(t)

	body, contentType := buildUpload(t, map[string]string{"title": "Hello"}, []filePart{
		{field: "media", filename: "one.mp3", contentType: "audio/mpeg", content: []byte("first")},
		{field: "media", filename: "two.mp3", contentType: "audio/mpeg", content: []byte("second")},
	})
	req := newUploadRequest(body, contentType)
	rec := httptest.NewRecorder()
	fake := &deadlineWriter{ResponseWriter: rec}

	s.handleEpisodeCreate(fake, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "a duplicate media part must be rejected")
	require.Equal(t, "close", rec.Header().Get("Connection"))
	require.True(t, fake.called)
	assertNoStagedFiles(t, uploadDir)
}

// countingReader counts the bytes read through it, so a test can prove the
// handler stops reading the body early instead of consuming the whole thing.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestUploadAggregateFieldBudgetHardCap proves the total text ever read is
// hard-capped at ~maxUploadFieldsBytes regardless of how the client splits it:
// a body whose fields sum far over the aggregate budget is rejected after
// reading only about the budget, not the whole (much larger) body.
func TestUploadAggregateFieldBudgetHardCap(t *testing.T) {
	s, _, _, uploadDir := newUploadServer(t)
	s.maxUploadBytes = 8 << 20 // well above the body so the field cap (not the body cap) trips

	// Many fields, each under the per-field cap, summing far over the aggregate
	// budget. Names are unknown (read+discarded) but still counted against it.
	fields := map[string]string{"title": "Hello"}
	const per = 40 << 10 // 40 KiB < maxUploadFieldBytes (64 KiB)
	const count = 50     // 50*40KiB = 2 MiB >> maxUploadFieldsBytes (256 KiB)
	for i := range count {
		fields["f"+strconv.Itoa(i)] = strings.Repeat("x", per)
	}
	body, contentType := buildUpload(t, fields,
		[]filePart{{field: "media", filename: "clip.mp3", contentType: "audio/mpeg", content: []byte("small media")}})
	total := int64(body.Len())

	counter := &countingReader{r: body}
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/c1/episodes", counter)
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = total
	req.SetPathValue("id", "c1")
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, &model.User{ID: "u1"}))
	rec := httptest.NewRecorder()
	fake := &deadlineWriter{ResponseWriter: rec}

	s.handleEpisodeCreate(fake, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "fields over the aggregate budget must be rejected")
	require.Equal(t, "close", rec.Header().Get("Connection"))
	require.True(t, fake.called)
	// The handler must have stopped reading well before the whole body: the hard
	// cap bounds the text read to ~maxUploadFieldsBytes plus one field + multipart
	// framing, far below the ~2 MiB body.
	assert.Less(t, counter.n, int64(maxUploadFieldsBytes+2*maxUploadFieldBytes),
		"total text read must be bounded by the aggregate budget, not the field sum")
	assert.Less(t, counter.n, total/2, "must stop reading well before consuming the whole body")
	assertNoStagedFiles(t, uploadDir)
}

// failingBlobs makes Put fail, to simulate a storage backend fault.
type failingBlobs struct{ blob.BlobStore }

func (failingBlobs) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("blob store unavailable")
}

// TestUploadServerStagingFailureReturns500 proves a server-side staging fault
// (here os.CreateTemp failing because the staging dir does not exist) yields a
// 5xx, not a client 4xx.
func TestUploadServerStagingFailureReturns500(t *testing.T) {
	s, _, _, uploadDir := newUploadServer(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	s.uploadTempDir = missing // CreateTemp will fail here

	body, contentType := buildUpload(t, map[string]string{"title": "Hello"},
		[]filePart{{field: "media", filename: "clip.mp3", contentType: "audio/mpeg", content: []byte("audio")}})
	req := newUploadRequest(body, contentType)
	rec := httptest.NewRecorder()

	s.handleEpisodeCreate(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a temp-file staging failure is a server error, not a client 4xx")
	assertNoStagedFiles(t, uploadDir) // original staging dir stays empty; no leak
}

// TestUploadBlobPutFailureReturns500 proves a blob.Put failure yields a 5xx.
func TestUploadBlobPutFailureReturns500(t *testing.T) {
	s, _, blobs, _ := newUploadServer(t)
	s.blobs = failingBlobs{blobs}

	body, contentType := buildUpload(t, map[string]string{"title": "Hello"},
		[]filePart{{field: "media", filename: "clip.mp3", contentType: "audio/mpeg", content: []byte("audio")}})
	req := newUploadRequest(body, contentType)
	rec := httptest.NewRecorder()

	s.handleEpisodeCreate(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code, "a blob.Put failure must be a 500")
}
