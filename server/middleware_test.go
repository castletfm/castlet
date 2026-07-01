package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// newRecoverServer builds a minimal Server with just the pieces recoverPanic
// needs: a renderer for the 500 page and a (silent) logger.
func newRecoverServer(t *testing.T) *Server {
	t.Helper()
	r, err := newTemplateRenderer()
	require.NoError(t, err)
	return &Server{renderer: r, logger: slog.New(slog.DiscardHandler)}
}

func TestRecoverPanic(t *testing.T) {
	s := newRecoverServer(t)

	// A route that panics is turned into a 500, and a subsequent request through
	// the same handler still succeeds — the panic did not take the server down.
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("kaboom")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())
}

func TestRecoverPanicUncomparableValue(t *testing.T) {
	s := newRecoverServer(t)

	// A panic value can be uncomparable (a map here); recovery must still yield
	// a 500 rather than crashing on an == comparison against ErrAbortHandler.
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(map[string]string{"x": "y"})
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRecoverPanicIsLogged(t *testing.T) {
	// A panicking request, wrapped as in the real handler chain (logRequests
	// outermost), must still emit the completion line recording the 500.
	var buf bytes.Buffer
	s := newRecoverServer(t)
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))

	h := s.logRequests(s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, buf.String(), "msg=request")
	require.Contains(t, buf.String(), "status=500")
}

func TestRecoverPanicAfterCommitAborts(t *testing.T) {
	// A handler that commits a 200 status and part of a body before panicking is
	// past the point of no return: the status is already on the wire. recoverPanic
	// must leave the committed response intact and abort the connection (via
	// ErrAbortHandler) instead of appending a bogus 500 page.
	var buf bytes.Buffer
	s := newRecoverServer(t)
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))

	h := s.logRequests(s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
		panic("kaboom")
	})))

	rec := httptest.NewRecorder()
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	})

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "partial", rec.Body.String())
	require.Contains(t, buf.String(), "panic recovered")
}

func TestRecoverPanicRethrowsAbortHandler(t *testing.T) {
	s := newRecoverServer(t)
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}
