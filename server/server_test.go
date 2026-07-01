package server_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/store"

	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/internal/session"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/server"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

type harness struct {
	base   string
	store  *sqlite.Store
	blobs  *localfs.Store
	client *http.Client
}

func newHarness(t *testing.T, extra ...server.Option) *harness {
	t.Helper()
	ctx := t.Context()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))

	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	q := dbqueue.New(st)
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))

	opts := append([]server.Option{
		server.WithAddr("127.0.0.1:0"),
		server.WithBaseURL("http://example.test"),
	}, extra...)
	srv, err := server.New(st, blobs, q, sess, opts...)
	require.NoError(t, err)

	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { <-ctrl.Done() }) // ctx cancels on test end; wait for clean exit

	return &harness{base: "http://" + ctrl.Addr(), store: st, blobs: blobs, client: newClient()}
}

// newClient returns an HTTP client with its own cookie jar that does not follow
// redirects, so tests can inspect 3xx responses and Set-Cookie headers.
func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	return string(h)
}

func (h *harness) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := h.client.Get(h.base + path)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// seed creates a user (password "secret"), a channel, and one published audio
// episode with a done transcript.
func (h *harness) seed(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: string(hash), CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Description: "about", Language: "en",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	pub := time.Now()
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1",
		Title: "First Episode", Description: "hello", MediaKey: "mk1",
		MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio, MediaBytes: 5,
		Status: model.EpisodePublished, TranscriptStatus: model.TranscriptDone,
		PublishedAt: &pub, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, h.store.SaveTranscript(ctx, &model.Transcript{EpisodeID: "e1", Language: "en",
		CreatedAt: time.Now(), Segments: []model.Segment{{StartSecs: 0, EndSecs: 2, Text: "spoken words"}}}))
}

func TestPublicPages(t *testing.T) {
	h := newHarness(t)
	h.seed(t)

	resp, body := h.get(t, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "My Show")

	resp, body = h.get(t, "/c/c1/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "First Episode")

	resp, body = h.get(t, "/e/e1/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "spoken words")
	require.Contains(t, body, "<audio")

	resp, body = h.get(t, "/c/c1/feed.xml")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "rss+xml")
	require.Contains(t, body, "http://example.test/e/e1/")

	resp, _ = h.get(t, "/c/nope/")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	h.seed(t)

	resp, _ := h.get(t, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	requireSecurityHeaders(t, resp)
}

// requireSecurityHeaders asserts the baseline security headers are present.
func requireSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	require.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
}

func TestDraftEpisodeHidden(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	require.NoError(t, h.store.CreateEpisode(t.Context(), &model.Episode{ID: "e2", ChannelID: "c1",
		Title: "Draft Ep", Status: model.EpisodeDraft,
		TranscriptStatus: model.TranscriptNone, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, _ := h.get(t, "/e/e2/")
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "draft must not be publicly viewable")
}

func TestMediaMissingKey(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get(t, "/media/does-not-exist")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestMediaServingHardening verifies the streamed media path defends against a
// stored-XSS upload: a blob whose episode declares text/html must be served
// with nosniff, as an attachment, and with a coerced non-HTML Content-Type,
// while a genuine audio/* blob keeps its type so inline playback still works.
func TestMediaServingHardening(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))

	put := func(key, mime string, body []byte) {
		t.Helper()
		_, err := h.blobs.Put(ctx, key, bytes.NewReader(body))
		require.NoError(t, err)
		require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "ch-" + key, UserID: "u1",
			Title: "T", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
		require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "ep-" + key, ChannelID: "ch-" + key,
			Title: "T", MediaKey: key, MediaMIME: mime, MediaKind: model.MediaAudio, MediaBytes: int64(len(body)),
			Status: model.EpisodeDraft, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	}

	put("evil", "text/html", []byte("<script>alert(1)</script>"))
	put("clip", "audio/mpeg", []byte("ID3 fake mp3 payload"))

	// A channel cover image has no owning episode; the type is sniffed. Store
	// the bytes only (no episode) under a fresh key.
	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	_, err := h.blobs.Put(ctx, "cover", bytes.NewReader(pngBytes))
	require.NoError(t, err)

	// Attacker-labeled text/html is never rendered as HTML.
	resp, _ := h.get(t, "/media/evil")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "attachment", resp.Header.Get("Content-Disposition"))
	require.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))

	// Real audio keeps its type (still nosniff) so <audio> playback works.
	resp, _ = h.get(t, "/media/clip")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "audio/mpeg", resp.Header.Get("Content-Type"))

	// A non-episode PNG (channel cover art) keeps image/png so <img> renders.
	resp, _ = h.get(t, "/media/cover")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "image/png", resp.Header.Get("Content-Type"))
}

// directBlobStore is a fake blob.BlobStore that also implements blob.DirectURL,
// recording the arguments the media handler passes to URL so tests can assert
// the direct-serve path applies the same hardening as the streamed path.
type directBlobStore struct {
	*localfs.Store
	gotContentType        string
	gotContentDisposition string
}

func (d *directBlobStore) URL(_ context.Context, key, contentType, contentDisposition string) (string, error) {
	d.gotContentType = contentType
	d.gotContentDisposition = contentDisposition
	return "https://cdn.example.test/o/" + key, nil
}

var _ blob.DirectURL = (*directBlobStore)(nil)

// TestMediaDirectServingHardening verifies the direct-serve path (blob.DirectURL,
// e.g. the S3 backend) redirects to a presigned URL that carries an attachment
// disposition and a coerced non-HTML content-type for a hostile text/html blob,
// matching the streamed path's stored-XSS defenses.
func TestMediaDirectServingHardening(t *testing.T) {
	ctx := t.Context()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))

	fs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	direct := &directBlobStore{Store: fs}
	q := dbqueue.New(st)
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))

	srv, err := server.New(st, direct, q, sess,
		server.WithAddr("127.0.0.1:0"),
		server.WithBaseURL("http://example.test"))
	require.NoError(t, err)
	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { <-ctrl.Done() })

	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err = fs.Put(ctx, "evil", bytes.NewReader([]byte("<script>alert(1)</script>")))
	require.NoError(t, err)
	require.NoError(t, st.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1",
		Title: "T", MediaKey: "evil", MediaMIME: "text/html", MediaKind: model.MediaAudio,
		MediaBytes: 1, Status: model.EpisodeDraft, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	client := newClient()
	resp, err := client.Get("http://" + ctrl.Addr() + "/media/evil")
	require.NoError(t, err)
	resp.Body.Close()

	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Equal(t, "https://cdn.example.test/o/evil", resp.Header.Get("Location"))
	require.Equal(t, "application/octet-stream", direct.gotContentType,
		"hostile text/html must be coerced before signing the presigned URL")
	require.Equal(t, "attachment", direct.gotContentDisposition,
		"direct path must force an attachment disposition")
}

func TestAuthAndAdmin(t *testing.T) {
	h := newHarness(t)
	h.seed(t)

	// admin requires auth -> redirect to /login
	resp, _ := h.get(t, "/admin/")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/login", resp.Header.Get("Location"))

	// wrong password
	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"nope"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// correct password -> redirect + session cookie stored in jar
	resp, err = h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))

	// now the dashboard is reachable and lists the channel
	resp, body := h.get(t, "/admin/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "My Show")
}

func TestAdminUploadFlow(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: string(hash), CreatedAt: time.Now()}))

	// log in
	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// create a channel
	resp, err = h.client.PostForm(h.base+"/admin/channels", url.Values{"title": {"My Show"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	channels, err := h.store.ListChannelsByUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, channels, 1)
	chID := channels[0].ID

	// upload an episode (multipart)
	audio := []byte("ID3 fake mp3 payload")
	body, contentType := multipartUpload(t, map[string]string{"title": "Hello"}, "media", "clip.mp3", "audio/mpeg", audio)
	resp, err = h.client.Post(h.base+"/admin/channels/"+chID+"/episodes", contentType, body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// episode persisted with detected media kind, and a transcribe job enqueued
	eps, err := h.store.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: chID})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	ep := eps[0]
	require.Equal(t, model.MediaAudio, ep.MediaKind)
	require.EqualValues(t, len(audio), ep.MediaBytes)
	require.Equal(t, model.TranscriptPending, ep.TranscriptStatus)

	job, err := h.store.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, time.Now(), time.Minute)
	require.NoError(t, err, "a transcription job should be queued")
	require.Contains(t, job.Payload, ep.ID)

	// media is served back with the right content type and bytes
	resp, mediaBody := h.get(t, "/media/"+ep.MediaKey)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "audio/mpeg", resp.Header.Get("Content-Type"))
	require.Equal(t, string(audio), mediaBody)

	// publish, then it appears on the public channel page
	resp, err = h.client.PostForm(h.base+"/admin/episodes/"+ep.ID+"/publish", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp, page := h.get(t, "/c/"+chID+"/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, page, "Hello")
}

func TestUploadExceedsCap(t *testing.T) {
	// Cap uploads tiny so a modest body trips the limit; the multipart parse
	// must be bounded, so an oversized body is rejected without spooling it all.
	h := newHarness(t, server.WithMaxUploadBytes(64))
	ctx := t.Context()
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: string(hash), CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// A payload well over the 64-byte cap.
	audio := bytes.Repeat([]byte("x"), 4096)
	body, contentType := multipartUpload(t, map[string]string{"title": "Hello"}, "media", "clip.mp3", "audio/mpeg", audio)
	resp, err = h.client.Post(h.base+"/admin/channels/c1/episodes", contentType, body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

	// Nothing was persisted.
	eps, err := h.store.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
	require.NoError(t, err)
	require.Empty(t, eps)
}

func TestUploadRejectsNonMultipart(t *testing.T) {
	// A non-multipart Content-Type must be rejected up front: otherwise
	// ParseMultipartForm falls back to ParseForm and would read the whole body
	// up to the (large) upload cap. The cap is tiny here only so the body used
	// below is trivially within it — the point is that we reject before parsing.
	h := newHarness(t, server.WithMaxUploadBytes(64))
	ctx := t.Context()
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: string(hash), CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// application/x-www-form-urlencoded is not a multipart upload.
	resp, err = h.client.PostForm(h.base+"/admin/channels/c1/episodes",
		url.Values{"title": {"Hello"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)

	// Nothing was persisted.
	eps, err := h.store.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
	require.NoError(t, err)
	require.Empty(t, eps)
}

func multipartUpload(t *testing.T, fields map[string]string, fileField, filename, mime string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="` + fileField + `"; filename="` + filename + `"`}
	h["Content-Type"] = []string{mime}
	part, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return &buf, mw.FormDataContentType()
}

func TestLifecycleStops(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(t.Context()))
	blobs, _ := localfs.New(t.TempDir())
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	srv, err := server.New(st, blobs, dbqueue.New(st), sess, server.WithAddr("127.0.0.1:0"))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, ctrl.Addr())

	cancel()
	require.NoError(t, ctrl.Wait(), "clean shutdown is not an error")
}

// sanity: reserved slug cannot be created as a channel (exercised via store +
// handler validation path through the admin form would need auth; keep a quick
// guard that the public router does not treat /login as a channel)
func TestReservedPathsNotChannels(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get(t, "/login")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, strings.ToLower(resp.Header.Get("Content-Type")), "rss")
}
