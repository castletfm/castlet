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

// buildMultipart builds a multipart/form-data body with the given text fields
// and a single "media" file part.
func buildMultipart(t *testing.T, fields map[string]string, filename, contentType string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	h := map[string][]string{
		"Content-Disposition": {`form-data; name="media"; filename="` + filename + `"`},
		"Content-Type":        {contentType},
	}
	part, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return &buf, mw.FormDataContentType()
}

// TestUploadStagesUnderConfiguredTempDir proves an episode upload streams the
// media into the configured upload temp dir (not os.TempDir), stores it content
// -addressed (MediaKey = sha256), creates the episode, and removes the staged
// temp file afterward.
func TestUploadStagesUnderConfiguredTempDir(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c", DisplayName: "A", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	fsBlobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	blobs := &recordingBlobs{BlobStore: fsBlobs}

	renderer, err := newTemplateRenderer()
	require.NoError(t, err)

	uploadDir := t.TempDir()
	s := &Server{
		store:          st,
		blobs:          blobs,
		queue:          dbqueue.New(st),
		renderer:       renderer,
		logger:         slog.New(slog.DiscardHandler),
		maxUploadBytes: 1 << 20,
		uploadTempDir:  uploadDir,
		now:            time.Now,
	}

	content := []byte("ID3 fake mp3 payload for staging test")
	sum := sha256.Sum256(content)
	wantKey := hex.EncodeToString(sum[:])

	body, contentType := buildMultipart(t, map[string]string{"title": "Hello", "language": "en"}, "clip.mp3", "audio/mpeg", content)
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/c1/episodes", body)
	req.Header.Set("Content-Type", contentType)
	req.SetPathValue("id", "c1")
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, &model.User{ID: "u1"}))
	rec := httptest.NewRecorder()

	s.handleEpisodeCreate(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code, "a valid upload must redirect")

	// The media was staged under the configured upload dir, not os.TempDir().
	assert.Equal(t, uploadDir, blobs.putDir, "media must be staged under the configured upload temp dir")
	assert.NotEqual(t, os.TempDir(), blobs.putDir, "upload must not stage into the system /tmp")

	// The staged temp file is removed after the handler returns.
	ents, err := os.ReadDir(uploadDir)
	require.NoError(t, err)
	for _, e := range ents {
		assert.NotContains(t, e.Name(), "castlet-upload-", "staged upload must be removed after the request")
	}

	// The episode was created, content-addressed by the sha256 of the bytes.
	eps, err := st.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, wantKey, eps[0].MediaKey, "MediaKey must be the sha256 of the uploaded bytes")
	assert.EqualValues(t, len(content), eps[0].MediaBytes)
	assert.Equal(t, model.MediaAudio, eps[0].MediaKind)

	// And the bytes are actually stored under that key.
	rc, n, err := fsBlobs.Get(ctx, wantKey)
	require.NoError(t, err)
	defer rc.Close()
	assert.EqualValues(t, len(content), n)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}
