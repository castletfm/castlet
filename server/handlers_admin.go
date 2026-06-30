package server

import (
	"errors"
	"net/http"

	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/internal/slug"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
)

// --- dashboard --------------------------------------------------------------

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	channels, err := s.store.ListChannelsByUser(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_dashboard", "Admin", struct {
		Channels []*model.Channel
	}{Channels: channels})
}

// --- channels ---------------------------------------------------------------

type channelForm struct {
	Channel *model.Channel
	Action  string
	Error   string
}

func (s *Server) handleChannelNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "admin_channel_form", "New channel",
		channelForm{Channel: &model.Channel{Language: "en"}, Action: "/admin/channels"})
}

func (s *Server) handleChannelCreate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	ch := &model.Channel{
		ID:          idgen.New(),
		UserID:      user.ID,
		Title:       r.FormValue("title"),
		Description: r.FormValue("description"),
		Language:    orDefault(r.FormValue("language"), "en"),
		CreatedAt:   s.now(),
		UpdatedAt:   s.now(),
	}
	ch.Slug = s.channelSlug(r.FormValue("slug"), ch.Title)

	if msg := s.validateChannel(ch); msg != "" {
		s.render(w, r, http.StatusBadRequest, "admin_channel_form", "New channel",
			channelForm{Channel: ch, Action: "/admin/channels", Error: msg})
		return
	}
	if err := s.store.CreateChannel(r.Context(), ch); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.render(w, r, http.StatusConflict, "admin_channel_form", "New channel",
				channelForm{Channel: ch, Action: "/admin/channels", Error: "That slug is already taken."})
			return
		}
		s.serverError(w, r, err)
		return
	}
	s.redirect(w, r, "/admin/")
}

func (s *Server) handleChannelEdit(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "admin_channel_form", "Edit channel",
		channelForm{Channel: ch, Action: "/admin/channels/" + ch.ID})
}

func (s *Server) handleChannelUpdate(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	ch.Title = r.FormValue("title")
	ch.Description = r.FormValue("description")
	ch.Language = orDefault(r.FormValue("language"), "en")
	ch.Slug = s.channelSlug(r.FormValue("slug"), ch.Title)
	ch.UpdatedAt = s.now()

	action := "/admin/channels/" + ch.ID
	if msg := s.validateChannel(ch); msg != "" {
		s.render(w, r, http.StatusBadRequest, "admin_channel_form", "Edit channel",
			channelForm{Channel: ch, Action: action, Error: msg})
		return
	}
	if err := s.store.UpdateChannel(r.Context(), ch); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.render(w, r, http.StatusConflict, "admin_channel_form", "Edit channel",
				channelForm{Channel: ch, Action: action, Error: "That slug is already taken."})
			return
		}
		s.serverError(w, r, err)
		return
	}
	s.redirect(w, r, "/admin/")
}

func (s *Server) validateChannel(ch *model.Channel) string {
	if ch.Title == "" {
		return "Title is required."
	}
	if ch.Slug == "" {
		return "Could not derive a slug; please provide one."
	}
	if isReservedSlug(ch.Slug) {
		return "That slug is reserved; choose another."
	}
	return ""
}

// channelSlug uses the provided slug, falling back to one derived from the
// title, then to a generated id so a channel is always reachable.
func (s *Server) channelSlug(provided, title string) string {
	if sl := slug.Make(provided); sl != "" {
		return sl
	}
	if sl := slug.Make(title); sl != "" {
		return sl
	}
	return idgen.New()
}

// --- episodes ---------------------------------------------------------------

func (s *Server) handleEpisodeList(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	episodes, err := s.store.ListEpisodes(r.Context(), store.EpisodeFilter{ChannelID: ch.ID})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_episodes", ch.Title, struct {
		Channel  *model.Channel
		Episodes []*model.Episode
	}{Channel: ch, Episodes: episodes})
}

type episodeForm struct {
	Channel     *model.Channel
	Title       string
	Slug        string
	Description string
	Error       string
}

func (s *Server) handleEpisodeNew(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "admin_episode_form", "New episode", episodeForm{Channel: ch})
}

func (s *Server) handleEpisodeCreate(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}

	form := episodeForm{Channel: ch, Title: r.FormValue("title"), Slug: r.FormValue("slug"), Description: r.FormValue("description")}
	reRender := func(status int, msg string) {
		form.Error = msg
		s.render(w, r, status, "admin_episode_form", "New episode", form)
	}

	if form.Title == "" {
		reRender(http.StatusBadRequest, "Title is required.")
		return
	}

	// Cap the request body, then read the uploaded file.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes)
	file, header, err := r.FormFile("media")
	if err != nil {
		reRender(http.StatusBadRequest, "A media file is required (audio or video).")
		return
	}
	defer file.Close()

	mime := detectUploadMIME(header)
	mediaKey := idgen.New()
	n, err := s.blobs.Put(r.Context(), mediaKey, file)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	ep := &model.Episode{
		ID:               idgen.New(),
		ChannelID:        ch.ID,
		Slug:             s.episodeSlug(form.Slug, form.Title),
		Title:            form.Title,
		Description:      form.Description,
		MediaKey:         mediaKey,
		MediaMIME:        mime,
		MediaKind:        model.DetectMediaKind(mime),
		MediaBytes:       n,
		Status:           model.EpisodeDraft,
		TranscriptStatus: model.TranscriptPending,
		CreatedAt:        s.now(),
		UpdatedAt:        s.now(),
	}
	if err := s.store.CreateEpisode(r.Context(), ep); err != nil {
		// Roll back the orphaned blob on a metadata failure.
		_ = s.blobs.Delete(r.Context(), mediaKey)
		if errors.Is(err, store.ErrConflict) {
			reRender(http.StatusConflict, "An episode with that slug already exists in this channel.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	// Queue transcription; failure to enqueue is logged but does not fail the
	// upload (the episode still exists and can be re-queued).
	if err := s.queue.Enqueue(r.Context(), model.JobTranscribe, model.TranscribePayload{EpisodeID: ep.ID}); err != nil {
		s.logger.Error("enqueue transcription", "episode", ep.ID, "error", err)
	}
	s.redirect(w, r, "/admin/channels/"+ch.ID+"/episodes")
}

func (s *Server) handleEpisodePublish(w http.ResponseWriter, r *http.Request) {
	s.setEpisodePublished(w, r, true)
}

func (s *Server) handleEpisodeUnpublish(w http.ResponseWriter, r *http.Request) {
	s.setEpisodePublished(w, r, false)
}

func (s *Server) setEpisodePublished(w http.ResponseWriter, r *http.Request, publish bool) {
	ep, ch, ok := s.ownedEpisode(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if publish {
		ep.Status = model.EpisodePublished
		if ep.PublishedAt == nil {
			now := s.now()
			ep.PublishedAt = &now
		}
	} else {
		ep.Status = model.EpisodeDraft
	}
	ep.UpdatedAt = s.now()
	if err := s.store.UpdateEpisode(r.Context(), ep); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirect(w, r, "/admin/channels/"+ch.ID+"/episodes")
}

func (s *Server) handleEpisodeDelete(w http.ResponseWriter, r *http.Request) {
	ep, ch, ok := s.ownedEpisode(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.store.DeleteEpisode(r.Context(), ep.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if ep.MediaKey != "" {
		if err := s.blobs.Delete(r.Context(), ep.MediaKey); err != nil {
			s.logger.Error("delete media blob", "key", ep.MediaKey, "error", err)
		}
	}
	s.redirect(w, r, "/admin/channels/"+ch.ID+"/episodes")
}

func (s *Server) episodeSlug(provided, title string) string {
	if sl := slug.Make(provided); sl != "" {
		return sl
	}
	if sl := slug.Make(title); sl != "" {
		return sl
	}
	return idgen.New()
}

// --- ownership helpers ------------------------------------------------------

// ownedChannel loads a channel and verifies the current user owns it. A missing
// or non-owned channel yields a 404 (not 403) so existence is not revealed.
func (s *Server) ownedChannel(w http.ResponseWriter, r *http.Request, id string) (*model.Channel, bool) {
	user := userFrom(r.Context())
	ch, err := s.store.ChannelByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ch.UserID != user.ID) {
		s.renderError(w, r, http.StatusNotFound, "Channel not found.")
		return nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, false
	}
	return ch, true
}

func (s *Server) ownedEpisode(w http.ResponseWriter, r *http.Request, id string) (*model.Episode, *model.Channel, bool) {
	ep, err := s.store.EpisodeByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Episode not found.")
		return nil, nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil, false
	}
	ch, ok := s.ownedChannel(w, r, ep.ChannelID)
	if !ok {
		return nil, nil, false
	}
	return ep, ch, true
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
