package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/feed"
	"github.com/castletfm/castlet/store"
)

// handleMedia serves a stored media object. When the blob store can serve bytes
// directly (object storage), it redirects to a presigned URL so audio never
// flows through Castlet; otherwise it streams the object with HTTP range support
// so audio/video players can seek. Either way the Content-Type is sanitized (see
// effectiveContentType): episode media uses its stored MIME, other blobs (e.g.
// channel cover art) are sniffed, and only an allowlist of safe types is served.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	// Resolve visibility and the Content-Type source together so a blob is served
	// under the same MIME as the row that makes it public. Media is
	// content-addressed, so one key may be shared by a draft episode, a published
	// episode, and a channel's cover art.
	mime := ""
	if ep, eerr := s.store.PublishedEpisodeByMediaKey(r.Context(), key); eerr == nil {
		// A published episode references the key: serve it with ITS MIME, never a
		// draft's, even when a draft happens to share the same key.
		mime = ep.MediaMIME
	} else if !errors.Is(eerr, store.ErrNotFound) {
		s.serverError(w, r, eerr)
		return
	} else {
		// No published episode references the key. It is public only when a
		// non-episode owner does — a channel's cover art; the MIME stays empty so
		// the bytes are sniffed below. Otherwise the blob is private (referenced
		// solely by draft episodes) or unowned entirely: report a plain 404 (not
		// 403, matching the existence-non-disclosure convention in handleEpisode).
		cover, cerr := s.store.ChannelImageKeyExists(r.Context(), key)
		if cerr != nil {
			s.serverError(w, r, cerr)
			return
		}
		if !cover {
			http.NotFound(w, r)
			return
		}
	}

	// Direct-serving backend: redirect to the object store. Decided once at
	// startup (s.directBlobs is nil for streaming backends). The sanitized type
	// is passed through so the object store signs the same safe Content-Type.
	if s.directBlobs != nil {
		ct, err := s.effectiveContentType(r.Context(), key, mime)
		if errors.Is(err, blob.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		// Match the streamed path's hardening: force an attachment disposition so
		// a hostile blob cannot render as a top-level page. The content-type is
		// already coerced to a safe type by effectiveContentType. nosniff cannot
		// be signed onto an S3 response (see blob/s3 URL), so the octet-stream
		// coercion for unsafe types plus this attachment disposition are the
		// mitigation on the direct path.
		url, err := s.directBlobs.URL(r.Context(), key, ct, "attachment")
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		http.Redirect(w, r, url, http.StatusFound)
		return
	}

	rc, size, err := s.blobs.Get(r.Context(), key)
	if errors.Is(err, blob.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rc.Close()

	ct := mime
	if ct == "" {
		// No owning episode (e.g. channel cover art): sniff from the bytes.
		if ct, err = sniffContentType(rc); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	ct = safeContentType(ct)

	// The bytes stream from Castlet's own origin and their MIME is
	// client-influenced at upload time (see detectUploadMIME), so a hostile
	// upload labeled text/html could otherwise execute as stored XSS. Refuse
	// sniffing, force a download on top-level navigation, and only serve the
	// allowlisted types; anything else is served opaquely as a download.
	// Inline <img>/<audio>/<video> loads are unaffected: media elements load by
	// resource regardless of Content-Disposition, and real uploads keep their
	// safe type.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Type", ct)
	_ = size // ServeContent derives length from the seeker; size is informational

	// Bound the stream by a refreshed idle write deadline so an unauthenticated
	// slow/stalled reader cannot pin this goroutine, connection, and open blob
	// reader forever (a slow-read DoS). Each successful write pushes the deadline
	// out, so a large but progressing download is never cut off; only a reader
	// that stalls longer than the window is dropped.
	w = s.streamWithIdleDeadline(w)

	// Zero modtime omits Last-Modified but still supports range requests.
	http.ServeContent(w, r, key, time.Time{}, rc)
}

// streamWithIdleDeadline arms an idle write deadline on w's underlying
// connection and returns a wrapper that refreshes it on every write. When the
// connection does not support write deadlines (e.g. an httptest recorder —
// http.ErrNotSupported), it returns w unwrapped so the download still streams
// without a deadline.
//
// The deadline is deliberately never cleared afterward: once a stalled write
// trips it, net/http's post-handler flush of any buffered response must also
// fail fast so the (already write-errored) connection is torn down instead of
// re-blocking on the same stalled reader forever. On a normal download the
// deadline is left at (last write + idle) in the future, which is harmless — a
// subsequent media stream on a kept-alive connection re-arms it, other responses
// complete well within the window, and IdleTimeout bounds connection reuse.
func (s *Server) streamWithIdleDeadline(w http.ResponseWriter) http.ResponseWriter {
	ctrl := http.NewResponseController(w)
	if err := ctrl.SetWriteDeadline(s.now().Add(s.mediaWriteIdle)); err != nil {
		return w
	}
	return &idleDeadlineWriter{ResponseWriter: w, ctrl: ctrl, idle: s.mediaWriteIdle, now: s.now}
}

// idleDeadlineWriter refreshes the underlying connection's write deadline by a
// fixed idle window before every write, turning a single fixed WriteTimeout
// (which would truncate long legitimate downloads and is intentionally unset on
// the http.Server) into a per-write stall bound. It relies on
// http.ResponseController reaching the real connection via the Unwrap chain (see
// underlying / statusWriter.Unwrap). A SetWriteDeadline error is ignored per
// write: the deadline was already armed once by streamWithIdleDeadline, and a
// failure to refresh must not abort a legitimate write.
type idleDeadlineWriter struct {
	http.ResponseWriter
	ctrl *http.ResponseController
	idle time.Duration
	now  func() time.Time
}

func (w *idleDeadlineWriter) Write(p []byte) (int, error) {
	_ = w.ctrl.SetWriteDeadline(w.now().Add(w.idle))
	return w.ResponseWriter.Write(p)
}

// Unwrap keeps the wrapped ResponseWriter reachable so http.ResponseController
// and status logging traverse past this wrapper.
func (w *idleDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// effectiveContentType resolves the sanitized Content-Type for a blob without
// streaming it: episode media uses its stored MIME, other blobs are sniffed
// from their leading bytes. The result is always run through safeContentType.
func (s *Server) effectiveContentType(ctx context.Context, key, mime string) (string, error) {
	ct := mime
	if ct == "" {
		rc, _, err := s.blobs.Get(ctx, key)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		if ct, err = sniffContentType(rc); err != nil {
			return "", err
		}
	}
	return safeContentType(ct), nil
}

// sniffContentType detects a blob's content type from its leading bytes and
// rewinds the reader so the caller can still serve it from the start.
func sniffContentType(rs io.ReadSeeker) (string, error) {
	buf := make([]byte, 512)
	n, err := io.ReadFull(rs, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return http.DetectContentType(buf[:n]), nil
}

// safeContentType returns ct when it is safe to serve under the app origin —
// audio/* and video/* for playback, plus a small allowlist of raster image
// types for channel cover art — and application/octet-stream otherwise.
// Client-influenced types such as text/html and image/svg+xml are coerced so a
// hostile upload cannot execute as stored XSS.
func safeContentType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case strings.HasPrefix(ct, "audio/"), strings.HasPrefix(ct, "video/"):
		return ct
	}
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct
	}
	return "application/octet-stream"
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.lookupChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	episodes, err := s.store.ListEpisodes(r.Context(), store.EpisodeFilter{ChannelID: ch.ID, PublishedOnly: true})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	xml, err := feed.Build(s.baseURL, ch, episodes)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(xml)
}

// defaultUploadMIME is the fallback content type when neither the declared
// Content-Type nor the filename extension yields a usable media type.
const defaultUploadMIME = "audio/mpeg"

// detectUploadMIME picks a content type for an uploaded file: the browser's
// declared type when specific, otherwise a guess from the file extension,
// falling back to a generic audio type. contentType is the upload part's
// declared Content-Type and filename its declared filename.
func detectUploadMIME(contentType, filename string) string {
	if contentType != "" && contentType != "application/octet-stream" {
		if mt, _, err := mime.ParseMediaType(contentType); err == nil {
			return mt
		}
	}
	if ext := filepath.Ext(filename); ext != "" {
		if byExt := mime.TypeByExtension(ext); byExt != "" {
			if mt, _, err := mime.ParseMediaType(byExt); err == nil {
				return mt
			}
		}
	}
	return defaultUploadMIME
}
