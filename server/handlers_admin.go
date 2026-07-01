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
	"time"

	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
)

const (
	// minUploadRate is the slowest upload throughput the server is willing to
	// wait for, in bytes per second. 128 KiB/s (~1 Mbps) is conservative for a
	// slow-but-legitimate link; a client sending slower than this is treated as
	// a stalled/drip upload and eventually cut off.
	minUploadRate = 128 << 10 // 128 KiB/s

	// uploadTimeoutFloor is the minimum read window granted regardless of size,
	// covering small uploads and transcriber/model spin-up latency.
	uploadTimeoutFloor = 5 * time.Minute
)

// uploadReadTimeout derives the read deadline for a media upload from the
// configured max upload size and minUploadRate: a client uploading at the
// minimum accepted rate must be able to deliver the largest allowed body within
// the window, i.e. max(floor, maxUploadBytes/minUploadRate). It is much larger
// than the global readTimeout so a big upload over a slow link can proceed,
// while a fully idle/drip upload is still eventually bounded. At the 512 MiB
// default cap this yields ~1 hour.
func (s *Server) uploadReadTimeout() time.Duration {
	// Round up: a cap that is not an exact multiple of minUploadRate still needs
	// enough seconds to deliver the trailing bytes at the minimum rate, so floor
	// division would clip the window just below the true time required.
	secs := (s.maxUploadBytes + minUploadRate - 1) / minUploadRate
	d := time.Duration(secs) * time.Second
	if d < uploadTimeoutFloor {
		return uploadTimeoutFloor
	}
	return d
}

// abortUpload bounds a rejection that happens after the long upload deadline was
// granted (i.e. the multipart parse failed with the body only partially read).
// It (a) expires the read deadline so net/http will not sit on a stalled hostile
// body under the long upload window, and (b) marks the connection to close so no
// unread body is drained for keep-alive reuse. Both are best-effort: the caller
// still writes the 4xx afterwards. SetReadDeadline errors (e.g. a conn with no
// deadline support) are ignored — this is a hardening step, not the response.
func (s *Server) abortUpload(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).SetReadDeadline(s.now())
}

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

// handleEpisodeCreate accepts a multipart media upload.
//
// INVARIANT: the long, size-derived upload read deadline (uploadReadTimeout) is
// granted ONLY for a validated multipart upload that is actively being read.
// Every other request shape is rejected here while still bounded by the global
// ReadTimeout — the deadline is extended only after all cheap, no-body checks
// pass — and no rejection path ever lets net/http read or drain a hostile,
// unread body under the long deadline.
//
// The request shapes and how each is bounded:
//  1. non-multipart Content-Type      -> 415, before the deadline is extended.
//  2. missing/empty multipart boundary -> 400, before the deadline is extended.
//  3. known over-cap Content-Length    -> 413, before the deadline is extended.
//  4. auth / ownership failure          -> handled by ownedChannel, before it.
//  5. valid multipart being parsed      -> long deadline applies; MaxBytesReader
//     caps total bytes read.
//  6. chunked/unknown-length body over the cap, or otherwise unparseable ->
//     rejected promptly by abortUpload (expire deadline + Connection: close) so
//     the remaining hostile body is not drained under the long deadline.
func (s *Server) handleEpisodeCreate(w http.ResponseWriter, r *http.Request) {
	// (4) Auth / ownership — cheap, reads no body.
	ch, ok := s.ownedChannel(w, r, r.PathValue("id"))
	if !ok {
		return
	}

	// (1)/(2) This endpoint only handles multipart file uploads. Require a
	// multipart/form-data Content-Type before touching the body: for any other
	// type ParseMultipartForm falls back to ParseForm, which reads the whole
	// request up to the (large) upload cap into memory. Guarding here avoids
	// that allocation for non-multipart requests. The boundary param is required
	// too: without it ParseMultipartForm fails only after the body is read, so a
	// boundary-less multipart/form-data would otherwise be granted the long
	// upload deadline before failing.
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		s.renderError(w, r, http.StatusUnsupportedMediaType, "The upload must be sent as multipart/form-data.")
		return
	}
	if params["boundary"] == "" {
		s.renderError(w, r, http.StatusBadRequest, "The multipart/form-data upload is missing its boundary.")
		return
	}

	// (3) Reject a known over-cap upload up front. When the client declares a
	// Content-Length larger than the cap the request is doomed, so return 413
	// before wrapping the body or extending the deadline — a bad request must
	// stay bounded by the global ReadTimeout instead of the long upload window.
	if r.ContentLength > 0 && r.ContentLength > s.maxUploadBytes {
		s.renderError(w, r, http.StatusRequestEntityTooLarge, "The uploaded file is too large.")
		return
	}

	// (5) The request is a validated multipart upload. Cap the request body
	// before anything reads it. r.FormValue/r.FormFile trigger multipart
	// parsing, which would otherwise spool the entire upload to memory (then
	// disk) with no limit, so the cap must wrap r.Body first — after a FormValue
	// call it is too late. Hand MaxBytesReader the UNWRAPPED ResponseWriter: it
	// does not follow Unwrap, and only when given net/http's real *response can
	// it fire the oversized-body hook that flags the connection to close instead
	// of draining the remaining body.
	r.Body = http.MaxBytesReader(underlying(w), r.Body, s.maxUploadBytes)

	// Extend the read deadline immediately before parsing the body: the global
	// ReadTimeout bounds body-drip on normal routes but is too short for a large
	// upload over a slow link, so grant a generous window here. ErrNotSupported
	// (no deadline support on the underlying conn) is harmless — the request
	// just keeps the global deadline.
	if err := http.NewResponseController(w).SetReadDeadline(s.now().Add(s.uploadReadTimeout())); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.serverError(w, r, err)
		return
	}

	// (6) Parse the (capped) multipart body explicitly so an over-cap upload
	// surfaces as a clean 413 instead of being silently swallowed by FormValue.
	// On ANY parse failure the body is only partially read, so abortUpload first
	// revokes the long deadline and marks the connection to close — otherwise a
	// hostile chunked/unknown-length body that stalls after crossing the cap (or
	// any malformed body) could be held/drained under the long upload window
	// before or after the 4xx is written.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.abortUpload(w)
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

	// Reserve the media key BEFORE writing the blob. The blob is stored (Put)
	// before its episode row exists, so without a reservation a concurrent episode
	// delete's orphan check — which only sees committed rows — cannot observe this
	// in-flight upload and could delete the blob out from under the about-to-be-
	// created episode (the CST-013 sibling window). The reservation is held across
	// Put+CreateEpisode and released (by token) only after the row (which then
	// carries the reference) is committed, so there is no instant in which neither
	// the reservation nor the episode row is visible to a concurrent delete.
	// reserveMediaKey retries briefly if a physical delete of the same key is in
	// progress (a delete lease is held) — the delete is quick, and Put below then
	// restores the immutable bytes.
	token, err := s.reserveMediaKey(r.Context(), mediaKey)
	if err != nil {
		if errors.Is(err, store.ErrBlobDeleting) {
			s.renderError(w, r, http.StatusServiceUnavailable,
				"The media is briefly being cleaned up. Please retry the upload.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	reserved := true
	// Release is done with a context detached from the request so a client
	// disconnect after a successful create cannot skip it (a skipped release only
	// lingers until the reservation TTL anyway).
	defer func() {
		if reserved {
			s.releaseBlob(context.WithoutCancel(r.Context()), token)
		}
	}()

	n, err := s.blobs.Put(r.Context(), mediaKey, file)
	if err != nil {
		// Put failed, so no blob was written; the deferred release drops the
		// reservation and there is nothing to clean up.
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
		TranscriptStatus: model.TranscriptNone,
		CreatedAt:        s.now(),
		UpdatedAt:        s.now(),
	}
	// Create the episode first: it owns the content-addressed media key. Its
	// transcript status starts at 'none' (not yet queued), and only the atomic
	// enqueue below flips it to 'pending' together with the job insert.
	if err := s.store.CreateEpisode(r.Context(), ep); err != nil {
		// Release our own reservation FIRST (detached from the request context so a
		// concurrent client cancel cannot skip it), so the orphan check below does
		// not count it, then best-effort delete the just-uploaded blob if nothing
		// else references it.
		cleanupCtx := context.WithoutCancel(r.Context())
		s.releaseBlob(cleanupCtx, token)
		reserved = false
		s.deleteOrphanBlob(cleanupCtx, mediaKey) // only if no other episode/reservation shares it
		s.serverError(w, r, err)
		return
	}

	// Queue transcription atomically with marking the episode pending: either both
	// happen or neither, so the episode is never left pending with no job to run it
	// (the CST-011 stuck case) and no job is ever queued with a stale episode
	// status. On failure the episode simply keeps its 'none' status with no job —
	// safe: the UI still offers a re-transcribe (only pending/processing block it),
	// so the user can retry. Surface the error. The episode was just created with a
	// fresh id in status 'none', so the atomic transition proceeds; ErrConflict is
	// not expected here, but map it to a conflict rather than a 500 if it ever races.
	if err := s.queue.EnqueueTranscription(r.Context(), ep.ID); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.renderError(w, r, http.StatusConflict, "Transcription is already in progress for this episode.")
			return
		}
		s.serverError(w, r, err)
		return
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

	// Renumber densely in one transaction so a mid-way failure cannot leave the
	// channel's positions partially renumbered.
	ids := make([]string, len(eps))
	for idx, e := range eps {
		ids[idx] = e.ID
	}
	if err := s.store.ReorderEpisodes(r.Context(), ch.ID, ids, s.now()); err != nil {
		if errors.Is(err, store.ErrInvalidReorder) {
			// A concurrent add/delete left our snapshot stale; ask the user to retry.
			s.renderError(w, r, http.StatusBadRequest,
				"The episode order is out of date. Please reload and try again.")
			return
		}
		s.serverError(w, r, err)
		return
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
	// Fast-path rejection of overlapping requests from a stale view: transcription
	// is already queued or running. This is only a UX shortcut — the authoritative
	// guard is the atomic transition in EnqueueTranscription below, which rejects a
	// concurrent request that raced past this check with store.ErrConflict.
	if ep.TranscriptStatus == model.TranscriptPending || ep.TranscriptStatus == model.TranscriptProcessing {
		s.renderError(w, r, http.StatusConflict, "Transcription is already in progress for this episode.")
		return
	}
	// Enqueue the job and mark the episode pending atomically: either both commit
	// or neither. A failure leaves the episode with its current (non-pending)
	// status and no job, so the UI still offers a re-transcribe; success can never
	// leave the episode pending with no job, nor a queued job with a stale status.
	// A concurrent request that already started transcription makes this one lose
	// the atomic transition: surface that as a conflict, not a server error.
	if err := s.queue.EnqueueTranscription(r.Context(), ep.ID); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.renderError(w, r, http.StatusConflict, "Transcription is already in progress for this episode.")
			return
		}
		s.serverError(w, r, err)
		return
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
	// Delete the row and learn, atomically, whether its blob is now orphaned. When
	// it is, DeleteEpisode has already taken a per-key delete lease in the same
	// transaction, so a concurrent same-content upload cannot reserve (and depend
	// on) the key while we physically delete it (CST-013). The physical delete +
	// lease release run on a request-detached context so a client disconnect cannot
	// leave the lease held (which would block that key's uploads until its TTL).
	mediaKey, orphaned, err := s.store.DeleteEpisode(r.Context(), ep.ID, s.now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if orphaned {
		s.deleteLeasedBlob(context.WithoutCancel(r.Context()), mediaKey)
	}
	s.redirect(w, r, "/admin/channels/"+ch.ID+"/episodes")
}

// deleteBlob removes a media blob, logging (but not surfacing) a failure. The
// caller is responsible for having established that the blob is no longer
// referenced — for an episode delete this comes from DeleteEpisode's atomic,
// same-transaction orphan check.
func (s *Server) deleteBlob(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if err := s.blobs.Delete(ctx, key); err != nil {
		s.logger.Error("delete media blob", "key", key, "error", err)
	}
}

// deleteLeasedBlob performs the physical delete of a blob whose orphan-hood was
// decided under a delete lease (taken in-transaction by DeleteEpisode or
// BlobOrphaned) and then releases that lease. The lease is held across the
// physical delete so a concurrent ReserveBlob for the same key is rejected
// (ErrBlobDeleting) and retried rather than racing the delete; releasing only
// afterwards is what makes the whole delete atomic against reservations.
func (s *Server) deleteLeasedBlob(ctx context.Context, key string) {
	if key == "" {
		return
	}
	s.deleteBlob(ctx, key)
	if err := s.store.ReleaseDeleteLease(ctx, key); err != nil {
		s.logger.Error("release blob delete lease", "key", key, "error", err)
	}
}

// reserveMediaKey reserves the media key for an in-flight upload, returning the
// release token. While a physical delete of the same key is in progress the store
// returns ErrBlobDeleting; the physical delete is quick, so this retries with a
// short bounded backoff until the delete's lease clears. If the budget is
// exhausted it returns ErrBlobDeleting for the caller to surface.
func (s *Server) reserveMediaKey(ctx context.Context, key string) (int64, error) {
	const (
		budget = 500 * time.Millisecond
		step   = 20 * time.Millisecond
	)
	deadline := time.Now().Add(budget)
	for {
		token, err := s.store.ReserveBlob(ctx, key, s.now())
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, store.ErrBlobDeleting) || !time.Now().Before(deadline) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(step):
		}
	}
}

// releaseBlob drops the in-flight upload reservation identified by token, logging
// (but not surfacing) a failure. A leaked reservation only lingers until its TTL.
func (s *Server) releaseBlob(ctx context.Context, token int64) {
	if err := s.store.ReleaseBlob(ctx, token); err != nil {
		s.logger.Error("release blob reservation", "token", token, "error", err)
	}
}

// deleteOrphanBlob removes a just-uploaded media blob on a failed episode
// create, but only if nothing else references it. Unlike the episode DELETE path
// there is no row to remove here (the insert failed), so this is a best-effort
// cleanup of an orphaned upload, run only on a rare create error AFTER this
// upload's own reservation has been released. BlobOrphaned checks referencing
// episodes, channel cover art (content-addressed media is shared, so an identical
// image upload can collide — deleting would 404 the cover art), and any active
// reservation held by a concurrent same-content upload; when it reports orphaned
// it has taken the delete lease, so the physical delete runs under the same lease
// protocol as the episode-delete path.
func (s *Server) deleteOrphanBlob(ctx context.Context, key string) {
	if key == "" {
		return
	}
	orphaned, err := s.store.BlobOrphaned(ctx, key, s.now())
	if err != nil {
		s.logger.Error("check blob references", "key", key, "error", err)
		return
	}
	if orphaned {
		s.deleteLeasedBlob(ctx, key)
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
