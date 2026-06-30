package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
)

// handlePublic is the catch-all for the public site. It parses the request
// path and dispatches to the landing, channel, episode, or feed view. Routing
// the dynamic top-level slugs here (rather than via ServeMux wildcards) avoids
// conflicts with the literal /static/ and /admin/ subtrees.
func (s *Server) handlePublic(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	switch len(parts) {
	case 0:
		s.handleLanding(w, r)
	case 1:
		s.handleChannel(w, r, parts[0])
	case 2:
		if parts[1] == "feed.xml" {
			s.handleFeed(w, r, parts[0])
			return
		}
		s.handleEpisode(w, r, parts[0], parts[1])
	default:
		s.renderError(w, r, http.StatusNotFound, "Page not found.")
	}
}

// splitPath returns the non-empty, slash-separated segments of p.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	channels, err := s.store.ListChannels(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "landing", "", channels)
}

// channelPage is the view model for a channel's public page.
type channelPage struct {
	Channel  *model.Channel
	Episodes []*model.Episode
}

func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request, channelSlug string) {
	ch, ok := s.lookupChannel(w, r, channelSlug)
	if !ok {
		return
	}
	episodes, err := s.store.ListEpisodes(r.Context(), store.EpisodeFilter{ChannelID: ch.ID, PublishedOnly: true})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "channel", ch.Title, channelPage{Channel: ch, Episodes: episodes})
}

// episodePage is the view model for a single episode page.
type episodePage struct {
	Channel  *model.Channel
	Episode  *model.Episode
	MediaURL string
	Segments []model.Segment
}

func (s *Server) handleEpisode(w http.ResponseWriter, r *http.Request, channelSlug, episodeSlug string) {
	ch, ok := s.lookupChannel(w, r, channelSlug)
	if !ok {
		return
	}
	ep, err := s.store.EpisodeBySlug(r.Context(), ch.ID, episodeSlug)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ep.Status != model.EpisodePublished) {
		s.renderError(w, r, http.StatusNotFound, "Episode not found.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	page := episodePage{Channel: ch, Episode: ep, MediaURL: "/media/" + ep.MediaKey}
	if ep.TranscriptStatus == model.TranscriptDone {
		if tr, terr := s.store.TranscriptByEpisode(r.Context(), ep.ID); terr == nil {
			page.Segments = tr.Segments
		}
	}
	s.render(w, r, http.StatusOK, "episode", ep.Title, page)
}

// lookupChannel resolves a public channel slug, writing a 404 and returning
// false when it does not exist.
func (s *Server) lookupChannel(w http.ResponseWriter, r *http.Request, slug string) (*model.Channel, bool) {
	ch, err := s.store.ChannelBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Channel not found.")
		return nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, false
	}
	return ch, true
}

// serverError logs an unexpected error and renders a 500.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("handler error", "path", r.URL.Path, "error", err)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong.")
}
