package server

import (
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"time"

	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/feed"
	"github.com/castletfm/castlet/store"
)

// handleMedia streams a stored media object with range support so audio/video
// players can seek. The content type comes from the owning episode; if no
// episode references the key (e.g. channel art), http.ServeContent sniffs it.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
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

	if ep, eerr := s.store.EpisodeByMediaKey(r.Context(), key); eerr == nil && ep.MediaMIME != "" {
		w.Header().Set("Content-Type", ep.MediaMIME)
	}
	_ = size // ServeContent derives length from the seeker; size is informational
	// Zero modtime omits Last-Modified but still supports range requests.
	http.ServeContent(w, r, key, time.Time{}, rc)
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request, channelSlug string) {
	ch, ok := s.lookupChannel(w, r, channelSlug)
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
