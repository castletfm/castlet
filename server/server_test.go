package server_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
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
	"github.com/castletfm/castlet/queue"
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
			Status: model.EpisodePublished, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	}

	put("evil", "text/html", []byte("<script>alert(1)</script>"))
	put("clip", "audio/mpeg", []byte("ID3 fake mp3 payload"))

	// A channel cover image has no owning episode; the type is sniffed. Store the
	// bytes and reference them from a channel's ImageKey (no episode) so the
	// cover-art path is exercised.
	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	_, err := h.blobs.Put(ctx, "cover", bytes.NewReader(pngBytes))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "cover-ch", UserID: "u1",
		Title: "T", Language: "en", ImageKey: "cover", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

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

	// A blob no row references (neither a published episode nor a channel cover)
	// is never served, even to a caller who knows its key.
	_, err = h.blobs.Put(ctx, "orphan", bytes.NewReader([]byte("unreferenced bytes")))
	require.NoError(t, err)
	resp, _ = h.get(t, "/media/orphan")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestMediaPublicationGating verifies /media/{key} only serves media referenced
// by a published episode: a draft's media returns 404 (never 403) even to a
// caller who knows the sha256 key, while a shared key becomes downloadable as
// soon as any referencing episode is published.
func TestMediaPublicationGating(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	body := []byte("ID3 fake mp3 payload")
	_, err := h.blobs.Put(ctx, "mk", bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "draft", ChannelID: "c1",
		Title: "Draft", MediaKey: "mk", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: int64(len(body)), Status: model.EpisodeDraft, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	// Media referenced only by a draft is not downloadable, even with the key.
	resp, _ := h.get(t, "/media/mk")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Once another episode publishes the same content-addressed key, it serves.
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "pub", ChannelID: "c1",
		Title: "Pub", MediaKey: "mk", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: int64(len(body)), Status: model.EpisodePublished, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	resp, served := h.get(t, "/media/mk")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, string(body), served)
}

// TestMediaSharedKeyUsesPublishedMIME verifies that when a draft and a published
// episode share one content-addressed key, the blob is served with the PUBLISHED
// episode's MIME, not the draft's — the draft never dictates the type of a key
// that is public because of a different, published row.
func TestMediaSharedKeyUsesPublishedMIME(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	body := []byte("ID3 fake mp3 payload")
	_, err := h.blobs.Put(ctx, "shared", bytes.NewReader(body))
	require.NoError(t, err)
	// Draft claims a different (video) MIME for the same bytes.
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "draft", ChannelID: "c1",
		Title: "Draft", MediaKey: "shared", MediaMIME: "video/mp4", MediaKind: model.MediaVideo,
		MediaBytes: int64(len(body)), Status: model.EpisodeDraft, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "pub", ChannelID: "c1",
		Title: "Pub", MediaKey: "shared", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: int64(len(body)), Status: model.EpisodePublished, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, _ := h.get(t, "/media/shared")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "audio/mpeg", resp.Header.Get("Content-Type"),
		"a shared key must be served with the published episode's MIME, not the draft's")
}

// TestMediaCoverArtWithDraftReference verifies that a blob referenced as a
// channel's cover art still serves even when a draft episode also references the
// same content-addressed key — episode-publication gating must not hide a public
// non-episode owner's blob.
func TestMediaCoverArtWithDraftReference(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))

	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	_, err := h.blobs.Put(ctx, "img", bytes.NewReader(pngBytes))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", ImageKey: "img", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	// A draft episode also references the same key; it must not gate the cover art.
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "draft", ChannelID: "c1",
		Title: "Draft", MediaKey: "img", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: int64(len(pngBytes)), Status: model.EpisodeDraft, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, _ := h.get(t, "/media/img")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "image/png", resp.Header.Get("Content-Type"),
		"channel cover art must serve even when a draft episode shares its key")
}

// TestMediaDraftOnlyNonImageNotFound verifies that a key referenced solely by a
// draft episode, and not by any channel's cover art, returns a plain 404.
func TestMediaDraftOnlyNonImageNotFound(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	body := []byte("ID3 fake mp3 payload")
	_, err := h.blobs.Put(ctx, "dk", bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "draft", ChannelID: "c1",
		Title: "Draft", MediaKey: "dk", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: int64(len(body)), Status: model.EpisodeDraft, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, _ := h.get(t, "/media/dk")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// directBlobStore is a fake blob.BlobStore that also implements blob.DirectURL,
// recording whether URL was called and the arguments the media handler passes to
// it, so tests can assert the direct-serve path both applies the same hardening
// as the streamed path and gates the redirect on the same publication rules.
type directBlobStore struct {
	*localfs.Store
	urlCalled             bool
	gotContentType        string
	gotContentDisposition string
}

func (d *directBlobStore) URL(_ context.Context, key, contentType, contentDisposition string) (string, error) {
	d.urlCalled = true
	d.gotContentType = contentType
	d.gotContentDisposition = contentDisposition
	return "https://cdn.example.test/o/" + key, nil
}

var _ blob.DirectURL = (*directBlobStore)(nil)

// directHarness spins up a server whose blob store implements blob.DirectURL
// (like the S3 backend), so tests can exercise the redirect path and assert
// whether URL was called via the recorded directBlobStore.
type directHarness struct {
	base   string
	store  *sqlite.Store
	blobs  *localfs.Store
	direct *directBlobStore
	client *http.Client
}

func newDirectHarness(t *testing.T) *directHarness {
	t.Helper()
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

	return &directHarness{base: "http://" + ctrl.Addr(), store: st, blobs: fs, direct: direct, client: newClient()}
}

func (h *directHarness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := h.client.Get(h.base + path)
	require.NoError(t, err)
	resp.Body.Close()
	return resp
}

// TestMediaDirectServingHardening verifies the direct-serve path (blob.DirectURL,
// e.g. the S3 backend) redirects to a presigned URL that carries an attachment
// disposition and a coerced non-HTML content-type for a hostile text/html blob,
// matching the streamed path's stored-XSS defenses.
func TestMediaDirectServingHardening(t *testing.T) {
	h := newDirectHarness(t)
	ctx := t.Context()

	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err := h.blobs.Put(ctx, "evil", bytes.NewReader([]byte("<script>alert(1)</script>")))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1",
		Title: "T", MediaKey: "evil", MediaMIME: "text/html", MediaKind: model.MediaAudio,
		MediaBytes: 1, Status: model.EpisodePublished, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp := h.get(t, "/media/evil")

	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Equal(t, "https://cdn.example.test/o/evil", resp.Header.Get("Location"))
	require.Equal(t, "application/octet-stream", h.direct.gotContentType,
		"hostile text/html must be coerced before signing the presigned URL")
	require.Equal(t, "attachment", h.direct.gotContentDisposition,
		"direct path must force an attachment disposition")
}

// TestMediaDirectDraftOnlyNotFound verifies the direct-serve path gates the
// redirect on publication just like the streamed path: a key referenced solely
// by a draft episode (and no channel cover art) returns a plain 404 without
// signing a presigned URL, so unpublished media is never exposed via the CDN.
func TestMediaDirectDraftOnlyNotFound(t *testing.T) {
	h := newDirectHarness(t)
	ctx := t.Context()

	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err := h.blobs.Put(ctx, "dk", bytes.NewReader([]byte("ID3 fake mp3 payload")))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "draft", ChannelID: "c1",
		Title: "Draft", MediaKey: "dk", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: 1, Status: model.EpisodeDraft, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp := h.get(t, "/media/dk")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.False(t, h.direct.urlCalled,
		"a draft-only key must not be signed into a presigned URL")
}

// TestMediaDirectUnownedNotFound verifies the direct-serve path returns a plain
// 404 for a blob no row references (an orphan) without signing a presigned URL.
func TestMediaDirectUnownedNotFound(t *testing.T) {
	h := newDirectHarness(t)
	ctx := t.Context()

	// The blob exists on disk but no episode or channel references it.
	_, err := h.blobs.Put(ctx, "orphan", bytes.NewReader([]byte("ID3 fake mp3 payload")))
	require.NoError(t, err)

	resp := h.get(t, "/media/orphan")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.False(t, h.direct.urlCalled,
		"an unowned key must not be signed into a presigned URL")
}

// TestMediaDirectCoverArtRedirects verifies the direct-serve path redirects for
// a key owned only by a channel's cover art (no published episode) — the same
// non-episode public owner the streamed path serves — signing a presigned URL.
func TestMediaDirectCoverArtRedirects(t *testing.T) {
	h := newDirectHarness(t)
	ctx := t.Context()

	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}))
	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	_, err := h.blobs.Put(ctx, "img", bytes.NewReader(pngBytes))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", ImageKey: "img", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp := h.get(t, "/media/img")
	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Equal(t, "https://cdn.example.test/o/img", resp.Header.Get("Location"))
	require.True(t, h.direct.urlCalled,
		"channel cover art must be signed into a presigned URL")
	require.Equal(t, "image/png", h.direct.gotContentType,
		"sniffed cover art must be signed with its detected image type")
}

// TestEpisodeDeleteRetainsSharedCoverBlob verifies deleting the last episode
// that references a content-addressed key does NOT delete the blob when a
// channel's cover art still references the same key — media is shared by hash, so
// an identical image and audio upload collide, and dropping the blob would 404
// the still-live cover art.
func TestEpisodeDeleteRetainsSharedCoverBlob(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))
	// The channel's cover art and the episode's media share one key.
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "T", Language: "en", ImageKey: "shared", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	body := []byte("ID3 fake mp3 payload")
	_, err := h.blobs.Put(ctx, "shared", bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1",
		Title: "Ep", MediaKey: "shared", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: int64(len(body)), Status: model.EpisodePublished, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	// log in and delete the episode
	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, err = h.client.PostForm(h.base+"/admin/episodes/e1/delete", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// the episode is gone, but the blob is retained for the channel cover art
	_, err = h.store.EpisodeByID(ctx, "e1")
	require.ErrorIs(t, err, store.ErrNotFound)
	rc, _, err := h.blobs.Get(ctx, "shared")
	require.NoError(t, err, "blob must be retained: a channel image still references it")
	rc.Close()
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

	// while still a draft, the media is not publicly downloadable by key
	resp, _ = h.get(t, "/media/"+ep.MediaKey)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// publish, then it appears on the public channel page
	resp, err = h.client.PostForm(h.base+"/admin/episodes/"+ep.ID+"/publish", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// once published the media is served back with the right content type and bytes
	resp, mediaBody := h.get(t, "/media/"+ep.MediaKey)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "audio/mpeg", resp.Header.Get("Content-Type"))
	require.Equal(t, string(audio), mediaBody)

	resp, page := h.get(t, "/c/"+chID+"/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, page, "Hello")
}

// enqueueFailQueue wraps a real JobQueue but always fails EnqueueTranscription,
// so tests can exercise the handlers' behaviour when a transcription job cannot
// be queued (the atomic enqueue+mark-pending step the upload and re-transcribe
// handlers both go through).
type enqueueFailQueue struct {
	queue.JobQueue
}

func (enqueueFailQueue) EnqueueTranscription(context.Context, string) error {
	return errors.New("enqueue boom")
}

// newHarnessQueue builds a harness backed by the given JobQueue, so a test can
// inject a queue whose Enqueue fails.
func newHarnessQueue(t *testing.T, mkQueue func(store queue.JobQueue) queue.JobQueue) *harness {
	t.Helper()
	ctx := t.Context()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))

	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	q := mkQueue(dbqueue.New(st))
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))

	srv, err := server.New(st, blobs, q, sess,
		server.WithAddr("127.0.0.1:0"),
		server.WithBaseURL("http://example.test"))
	require.NoError(t, err)
	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { <-ctrl.Done() })

	return &harness{base: "http://" + ctrl.Addr(), store: st, blobs: blobs, client: newClient()}
}

// TestUploadEnqueueFailureNotStuckPending verifies that when the transcription
// job cannot be enqueued during upload, the episode is not left stuck 'pending'
// (which the UI would refuse to re-queue): because enqueue and marking the
// episode pending are one atomic step, a failure leaves the episode at its
// initial non-pending 'none' status with no job, and the handler surfaces an
// error to the user.
func TestUploadEnqueueFailureNotStuckPending(t *testing.T) {
	h := newHarnessQueue(t, func(q queue.JobQueue) queue.JobQueue { return enqueueFailQueue{q} })
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	audio := []byte("ID3 fake mp3 payload")
	body, contentType := multipartUpload(t, map[string]string{"title": "Hello"}, "media", "clip.mp3", "audio/mpeg", audio)
	resp, err = h.client.Post(h.base+"/admin/channels/c1/episodes", contentType, body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"a failed enqueue must surface an error, not silently succeed")

	// The episode exists (it owns the media key) but must not be stuck pending:
	// the atomic enqueue rolled back, so it keeps its initial non-pending status.
	eps, err := h.store.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
	require.NoError(t, err)
	require.Len(t, eps, 1)
	require.Equal(t, model.TranscriptNone, eps[0].TranscriptStatus,
		"an episode whose job never queued must not be stuck pending")
}

// TestReTranscribeEnqueueFailureNotStuckPending verifies that when re-enqueuing
// transcription for an existing episode fails, the episode keeps its prior
// (non-pending) status so the UI can still offer a re-transcribe, and the
// handler surfaces an error.
func TestReTranscribeEnqueueFailureNotStuckPending(t *testing.T) {
	h := newHarnessQueue(t, func(q queue.JobQueue) queue.JobQueue { return enqueueFailQueue{q} })
	ctx := t.Context()
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, h.store.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1",
		Title: "Ep", MediaKey: "mk1", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio, MediaBytes: 5,
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptFailed,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp, err = h.client.PostForm(h.base+"/admin/episodes/e1/transcribe", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"a failed re-enqueue must surface an error")

	ep, err := h.store.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptFailed, ep.TranscriptStatus,
		"a failed re-enqueue must not leave the episode stuck pending")
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

// TestUploadOverCapChunkedStallReturnsPromptly guards cell 6 end-to-end against
// a live server: a chunked (unknown-length) multipart upload that crosses the
// cap and then STALLS must be answered with 413 promptly — bounded by the global
// read handling, not held open for the long, size-derived upload deadline. The
// tiny cap makes the upload deadline the 5-minute floor, so if the server drained
// or waited on the stalled body under that deadline this test would block far
// past its own short read deadline and fail.
func TestUploadOverCapChunkedStallReturnsPromptly(t *testing.T) {
	h := newHarness(t, server.WithMaxUploadBytes(64))
	ctx := t.Context()
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: string(hash), CreatedAt: time.Now()}))
	require.NoError(t, h.store.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	// Log in through the client so its jar holds a valid session cookie, which we
	// then replay on a raw connection (the raw request is needed to send an
	// over-cap chunk and then deliberately stall without the client-side
	// write/response race a normal http.Client hits here).
	resp, err := h.client.PostForm(h.base+"/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	u, err := url.Parse(h.base)
	require.NoError(t, err)
	var cookie strings.Builder
	for i, c := range h.client.Jar.Cookies(u) {
		if i > 0 {
			cookie.WriteString("; ")
		}
		cookie.WriteString(c.Name + "=" + c.Value)
	}
	require.NotEmpty(t, cookie.String(), "expected a session cookie after login")

	conn, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	defer conn.Close()

	// Send a complete request head, then one chunk well over the 64-byte cap, and
	// then nothing more (no terminating chunk) — a body that stalls after crossing
	// the cap. Content need not be valid multipart: the cap is hit first.
	head := "POST /admin/channels/c1/episodes HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Cookie: " + cookie.String() + "\r\n" +
		"Content-Type: multipart/form-data; boundary=xyz\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n"
	_, err = io.WriteString(conn, head)
	require.NoError(t, err)
	oversized := strings.Repeat("x", 4096)
	_, err = fmt.Fprintf(conn, "%x\r\n%s\r\n", len(oversized), oversized)
	require.NoError(t, err)

	// Bound our own wait well under the 5-minute upload-deadline floor: a prompt
	// 413 arrives in milliseconds; being held under the long deadline would blow
	// past this.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err, "server must respond promptly, not hold the stalled body")
	require.Contains(t, statusLine, "413",
		"an over-cap chunked upload must be rejected with 413")

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
