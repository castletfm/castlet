package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
