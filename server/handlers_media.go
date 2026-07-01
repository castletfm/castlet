package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
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

	mime := ""
	if ep, eerr := s.store.EpisodeByMediaKey(r.Context(), key); eerr == nil {
		mime = ep.MediaMIME
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
	// Zero modtime omits Last-Modified but still supports range requests.
	http.ServeContent(w, r, key, time.Time{}, rc)
}

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

// detectUploadMIME picks a content type for an uploaded file: the browser's
// declared type when specific, otherwise a guess from the file extension,
// falling back to a generic audio type.
func detectUploadMIME(header *multipart.FileHeader) string {
	if ct := header.Header.Get("Content-Type"); ct != "" && ct != "application/octet-stream" {
		if mt, _, err := mime.ParseMediaType(ct); err == nil {
			return mt
		}
	}
	if ext := filepath.Ext(header.Filename); ext != "" {
		if byExt := mime.TypeByExtension(ext); byExt != "" {
			if mt, _, err := mime.ParseMediaType(byExt); err == nil {
				return mt
			}
		}
	}
	return "audio/mpeg"
}
