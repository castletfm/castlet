package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/castletfm/castlet/internal/idgen"
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

	if msg := s.validateChannel(ch); msg != "" {
		s.render(w, r, http.StatusBadRequest, "admin_channel_form", "New channel",
			channelForm{Channel: ch, Action: "/admin/channels", Error: msg})
		return
	}
	if err := s.store.CreateChannel(r.Context(), ch); err != nil {
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
	ch.UpdatedAt = s.now()

	action := "/admin/channels/" + ch.ID
	if msg := s.validateChannel(ch); msg != "" {
		s.render(w, r, http.StatusBadRequest, "admin_channel_form", "Edit channel",
			channelForm{Channel: ch, Action: action, Error: msg})
		return
	}
	if err := s.store.UpdateChannel(r.Context(), ch); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirect(w, r, "/admin/")
}

func (s *Server) validateChannel(ch *model.Channel) string {
	if ch.Title == "" {
		return "Title is required."
	}
	return ""
}

// handleUpload is the global "Upload" entry point shown in the header for any
// logged-in user. Episodes belong to a channel, so it routes by how many the
// user owns: straight to the upload form when there is exactly one, a channel
// picker when there are several, and a create-a-channel prompt when there are
// none.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	channels, err := s.store.ListChannelsByUser(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(channels) == 1 {
		s.redirect(w, r, "/admin/channels/"+channels[0].ID+"/episodes/new")
		return
	}
	s.render(w, r, http.StatusOK, "admin_upload", "Upload", struct {
		Channels []*model.Channel
	}{Channels: channels})
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
	Description string
	Language    string // spoken language for transcription; "" = auto-detect
	Error       string
}

func (s *Server) handleEpisodeNew(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// Default the episode's spoken language to the channel's primary language so
	// the common single-language case needs no extra clicks.
	s.render(w, r, http.StatusOK, "admin_episode_form", "New episode",
		episodeForm{Channel: ch, Language: ch.Language})
}

func (s *Server) handleEpisodeCreate(w http.ResponseWriter, r *http.Request) {
	// Cap the request body before anything reads it. r.FormValue/r.FormFile
	// trigger multipart parsing, which would otherwise spool the entire upload
	// to memory (then disk) with no limit, so the cap must wrap r.Body first —
	// after a FormValue call it is too late.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes)

	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}

	// This endpoint only handles multipart file uploads. Require a
	// multipart/form-data Content-Type before touching the body: for any other
	// type ParseMultipartForm falls back to ParseForm, which reads the whole
	// request up to the (large) upload cap into memory. Guarding here avoids
	// that allocation for non-multipart requests.
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "multipart/form-data" {
		s.renderError(w, r, http.StatusUnsupportedMediaType, "The upload must be sent as multipart/form-data.")
		return
	}

	// Parse the (capped) multipart body explicitly so an over-cap upload
	// surfaces as a clean 413 instead of being silently swallowed by FormValue.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.renderError(w, r, http.StatusRequestEntityTooLarge, "The uploaded file is too large.")
			return
		}
		s.renderError(w, r, http.StatusBadRequest, "The upload could not be read.")
		return
	}

	form := episodeForm{Channel: ch, Title: r.FormValue("title"),
		Description: r.FormValue("description"), Language: r.FormValue("language")}
	reRender := func(status int, msg string) {
		form.Error = msg
		s.render(w, r, status, "admin_episode_form", "New episode", form)
	}

	if form.Title == "" {
		reRender(http.StatusBadRequest, "Title is required.")
		return
	}

	// Read the uploaded file from the already-parsed multipart form.
	file, header, err := r.FormFile("media")
	if err != nil {
		reRender(http.StatusBadRequest, "A media file is required (audio or video).")
		return
	}
	defer file.Close()

	mimeType := detectUploadMIME(header)
	// Content-address the media: the blob key is the sha256 of the bytes, so a
	// media URL is permanently bound to exactly those bytes — they cannot change
	// without becoming a different URL. (The uploaded file is seekable, so we can
	// hash it and then rewind to store it.)
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		s.serverError(w, r, err)
		return
	}
	mediaKey := hex.EncodeToString(hasher.Sum(nil))
	n, err := s.blobs.Put(r.Context(), mediaKey, file)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	ep := &model.Episode{
		ID:               idgen.New(),
		ChannelID:        ch.ID,
		Title:            form.Title,
		Description:      form.Description,
		MediaKey:         mediaKey,
		MediaMIME:        mimeType,
		MediaKind:        model.DetectMediaKind(mimeType),
		MediaBytes:       n,
		Language:         form.Language,
		Status:           model.EpisodeDraft,
		TranscriptStatus: model.TranscriptPending,
		CreatedAt:        s.now(),
		UpdatedAt:        s.now(),
	}
	if err := s.store.CreateEpisode(r.Context(), ep); err != nil {
		s.deleteOrphanBlob(r.Context(), mediaKey) // only if no other episode shares it
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

// episodeEditForm backs the admin episode edit page. Media is immutable, so it
// is not part of the form; only metadata is editable.
type episodeEditForm struct {
	Episode     *model.Episode
	Channel     *model.Channel
	Title       string
	Description string
	Language    string
	Error       string
}

func (s *Server) handleEpisodeEdit(w http.ResponseWriter, r *http.Request) {
	ep, ch, ok := s.ownedEpisode(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "admin_episode_edit", "Edit episode", episodeEditForm{
		Episode: ep, Channel: ch, Title: ep.Title, Description: ep.Description, Language: ep.Language})
}

func (s *Server) handleEpisodeUpdate(w http.ResponseWriter, r *http.Request) {
	ep, ch, ok := s.ownedEpisode(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	form := episodeEditForm{Episode: ep, Channel: ch, Title: r.FormValue("title"),
		Description: r.FormValue("description"), Language: r.FormValue("language")}
	if form.Title == "" {
		form.Error = "Title is required."
		s.render(w, r, http.StatusBadRequest, "admin_episode_edit", "Edit episode", form)
		return
	}
	// Metadata only — MediaKey is left untouched, so the bytes never change.
	ep.Title = form.Title
	ep.Description = form.Description
	ep.Language = form.Language
	ep.UpdatedAt = s.now()
	if err := s.store.UpdateEpisode(r.Context(), ep); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirect(w, r, "/admin/channels/"+ch.ID+"/episodes")
}

func (s *Server) handleEpisodePublish(w http.ResponseWriter, r *http.Request) {
	s.setEpisodePublished(w, r, true)
}

func (s *Server) handleEpisodeUnpublish(w http.ResponseWriter, r *http.Request) {
	s.setEpisodePublished(w, r, false)
}

// handleEpisodeMove reorders an episode within its channel one step up or down
// (form field "dir" = up|down). It loads the channel's episodes in display
// order, swaps the target with its neighbour, then renumbers positions densely
// so the manual order is well-defined from then on.
func (s *Server) handleEpisodeMove(w http.ResponseWriter, r *http.Request) {
	ep, ch, ok := s.ownedEpisode(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	dest := "/admin/channels/" + ch.ID + "/episodes"

	eps, err := s.store.ListEpisodes(r.Context(), store.EpisodeFilter{ChannelID: ch.ID})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	i := -1
	for idx, e := range eps {
		if e.ID == ep.ID {
			i = idx
			break
		}
	}
	j := i - 1
	if r.FormValue("dir") == "down" {
		j = i + 1
	}
	if i < 0 || j < 0 || j >= len(eps) {
		s.redirect(w, r, dest) // already at an edge or not found; nothing to do
		return
	}
	eps[i], eps[j] = eps[j], eps[i]

	for idx, e := range eps {
		if e.Position == idx {
			continue
		}
		e.Position = idx
		e.UpdatedAt = s.now()
		if err := s.store.UpdateEpisode(r.Context(), e); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.redirect(w, r, dest)
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

// handleEpisodeTranscribe re-runs transcription for an existing episode using
// its saved language (set on the edit page): it resets the status to pending and
// re-enqueues the job, without re-uploading the media.
func (s *Server) handleEpisodeTranscribe(w http.ResponseWriter, r *http.Request) {
	ep, ch, ok := s.ownedEpisode(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// Reject overlapping requests: transcription is already queued or running.
	if ep.TranscriptStatus == model.TranscriptPending || ep.TranscriptStatus == model.TranscriptProcessing {
		s.renderError(w, r, http.StatusConflict, "Transcription is already in progress for this episode.")
		return
	}
	ep.TranscriptStatus = model.TranscriptPending
	ep.UpdatedAt = s.now()
	if err := s.store.UpdateEpisode(r.Context(), ep); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.queue.Enqueue(r.Context(), model.JobTranscribe, model.TranscribePayload{EpisodeID: ep.ID}); err != nil {
		s.logger.Error("enqueue re-transcription", "episode", ep.ID, "error", err)
	}
	s.redirect(w, r, "/admin/channels/"+ch.ID+"/episodes")
}

// handleEpisodeStatus returns an episode's transcript status as JSON, so the
// admin episode list can poll and update the badge live while transcription
// runs (no full-page reload).
func (s *Server) handleEpisodeStatus(w http.ResponseWriter, r *http.Request) {
	ep, _, ok := s.ownedEpisode(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Transcript string `json:"transcript"`
	}{Transcript: string(ep.TranscriptStatus)}); err != nil {
		s.logger.Error("encode episode status", "episode", ep.ID, "error", err)
	}
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
	s.deleteOrphanBlob(r.Context(), ep.MediaKey)
	s.redirect(w, r, "/admin/channels/"+ch.ID+"/episodes")
}

// deleteOrphanBlob removes a media blob, but only if no other row still
// references it. Media is content-addressed, so identical uploads share a key;
// this keeps a delete from yanking a blob another owner depends on. Both
// episodes and channel cover art can own a key, so both must be checked before a
// blob is removed.
func (s *Server) deleteOrphanBlob(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if _, err := s.store.EpisodeByMediaKey(ctx, key); err == nil {
		return // still referenced by another episode
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("check media references", "key", key, "error", err)
		return
	}
	// No episode owns the key, but a channel's cover art may: content-addressed
	// media is shared, so an identical image and audio upload can collide. Don't
	// delete a blob a channel image still references (would 404 the cover art).
	if cover, err := s.store.ChannelImageKeyExists(ctx, key); err != nil {
		s.logger.Error("check channel image references", "key", key, "error", err)
		return
	} else if cover {
		return // still referenced by a channel image
	}
	if err := s.blobs.Delete(ctx, key); err != nil {
		s.logger.Error("delete media blob", "key", key, "error", err)
	}
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
