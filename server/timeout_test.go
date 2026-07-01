package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
)

// TestUploadReadTimeout guards the read-deadline derivation: at the default
// 512 MiB cap the window must be generously larger than both the global
// readTimeout and the floor, otherwise extending the deadline would not let a
// large upload over a slow link outlast the global body-drip cap. A tiny cap
// must fall back to the floor.
func TestUploadReadTimeout(t *testing.T) {
	s := &Server{maxUploadBytes: 512 << 20} // default cap
	got := s.uploadReadTimeout()
	if got <= readTimeout {
		t.Fatalf("uploadReadTimeout (%s) must exceed readTimeout (%s)", got, readTimeout)
	}
	if got <= uploadTimeoutFloor {
		t.Fatalf("uploadReadTimeout (%s) must exceed the floor (%s)", got, uploadTimeoutFloor)
	}
	if want := time.Hour; got < want {
		t.Fatalf("uploadReadTimeout (%s) for the default cap must be generous (>= %s)", got, want)
	}

	small := &Server{maxUploadBytes: 1 << 10} // 1 KiB, well below the floor rate
	if got := small.uploadReadTimeout(); got != uploadTimeoutFloor {
		t.Fatalf("uploadReadTimeout for a tiny cap = %s, want floor %s", got, uploadTimeoutFloor)
	}

	// A cap that is not an exact multiple of minUploadRate must round the window
	// UP: floor division would leave the trailing bytes without enough time, so
	// a client sending at exactly minUploadRate could be cut off. Use a cap above
	// the floor so the rate-derived value (not the floor) is exercised.
	const notMultiple = 400*minUploadRate + 1 // ~50 MiB + 1 byte, above the floor rate
	big := &Server{maxUploadBytes: notMultiple}
	got = big.uploadReadTimeout()
	if floored := time.Duration(notMultiple/minUploadRate) * time.Second; got <= floored {
		t.Fatalf("uploadReadTimeout (%s) must round up past the floored value (%s)", got, floored)
	}
	if need := time.Duration(minUploadRate) * got / time.Second; need < notMultiple {
		t.Fatalf("uploadReadTimeout (%s) delivers only %d bytes at minUploadRate, want >= %d", got, need, notMultiple)
	}
}

// TestEpisodeCreateNonMultipartSkipsDeadline guards the ordering of
// handleEpisodeCreate: the long per-upload read deadline must be extended only
// after the cheap no-body checks (ownership + multipart Content-Type) pass. A
// non-multipart request must be rejected with 415 under the global ReadTimeout,
// i.e. without SetReadDeadline ever being called — otherwise net/http could
// drain the unread body under the long upload deadline instead.
func TestEpisodeCreateNonMultipartSkipsDeadline(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	renderer, err := newTemplateRenderer()
	require.NoError(t, err)
	s := &Server{store: st, renderer: renderer, logger: slog.Default(),
		siteName: "Castlet", maxUploadBytes: 64, now: time.Now}

	rec := httptest.NewRecorder()
	fake := &deadlineWriter{ResponseWriter: rec}
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/c1/episodes",
		strings.NewReader("title=Hello"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "c1")
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, &model.User{ID: "u1"}))

	s.handleEpisodeCreate(fake, req)

	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	require.False(t, fake.called,
		"read deadline must not be extended before the multipart Content-Type check passes")
}

// TestEpisodeCreateMissingBoundarySkipsDeadline guards FINDING 1: a
// multipart/form-data request without a boundary param passes the media-type
// check but can only fail once ParseMultipartForm reads the body. It must be
// rejected up front (4xx) under the global ReadTimeout, i.e. without the long
// upload deadline ever being extended.
func TestEpisodeCreateMissingBoundarySkipsDeadline(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	renderer, err := newTemplateRenderer()
	require.NoError(t, err)
	s := &Server{store: st, renderer: renderer, logger: slog.Default(),
		siteName: "Castlet", maxUploadBytes: 64, now: time.Now}

	rec := httptest.NewRecorder()
	fake := &deadlineWriter{ResponseWriter: rec}
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/c1/episodes",
		strings.NewReader("body"))
	req.Header.Set("Content-Type", "multipart/form-data") // no boundary param
	req.SetPathValue("id", "c1")
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, &model.User{ID: "u1"}))

	s.handleEpisodeCreate(fake, req)

	require.GreaterOrEqual(t, rec.Code, http.StatusBadRequest)
	require.Less(t, rec.Code, http.StatusInternalServerError)
	require.False(t, fake.called,
		"read deadline must not be extended before the boundary param check passes")
}

// TestEpisodeCreateOverCapContentLengthSkipsDeadline guards FINDING 2: a request
// declaring a Content-Length above the upload cap is doomed, so it must return
// 413 before the body is wrapped or the long upload deadline extended — a bad
// request stays bounded by the global ReadTimeout.
func TestEpisodeCreateOverCapContentLengthSkipsDeadline(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "My Show", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	renderer, err := newTemplateRenderer()
	require.NoError(t, err)
	s := &Server{store: st, renderer: renderer, logger: slog.Default(),
		siteName: "Castlet", maxUploadBytes: 64, now: time.Now}

	rec := httptest.NewRecorder()
	fake := &deadlineWriter{ResponseWriter: rec}
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/c1/episodes",
		strings.NewReader("body"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.ContentLength = s.maxUploadBytes + 1 // declared over the cap
	req.SetPathValue("id", "c1")
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, &model.User{ID: "u1"}))

	s.handleEpisodeCreate(fake, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.False(t, fake.called,
		"read deadline must not be extended for a known over-cap upload")
}

// deadlineWriter is a fake deadline-capable ResponseWriter recording the last
// read deadline it was asked to set.
type deadlineWriter struct {
	http.ResponseWriter
	deadline time.Time
	called   bool
}

func (w *deadlineWriter) SetReadDeadline(t time.Time) error {
	w.deadline = t
	w.called = true
	return nil
}

// TestStatusWriterUnwrapReachesDeadline guards the upload read-deadline: because
// logRequests wraps the ResponseWriter in a statusWriter, SetReadDeadline only
// takes effect if statusWriter implements Unwrap so http.ResponseController can
// traverse to the deadline-capable writer. Without Unwrap the controller returns
// http.ErrNotSupported and the deadline silently no-ops.
func TestStatusWriterUnwrapReachesDeadline(t *testing.T) {
	fake := &deadlineWriter{}
	sw := &statusWriter{ResponseWriter: fake, status: http.StatusOK}

	want := time.Unix(1234567890, 0)
	if err := http.NewResponseController(sw).SetReadDeadline(want); err != nil {
		t.Fatalf("SetReadDeadline through statusWriter: %v", err)
	}
	if !fake.called {
		t.Fatal("SetReadDeadline did not reach the underlying writer (statusWriter.Unwrap missing?)")
	}
	if !fake.deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", fake.deadline, want)
	}
}
