package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(t.Context()))
	t.Cleanup(func() { s.Close() })
	return s
}

func seedUser(t *testing.T, s *sqlite.Store) *model.User {
	t.Helper()
	u := &model.User{ID: "u1", Email: "a@example.com", DisplayName: "A", PasswordHash: "x", CreatedAt: time.Now()}
	require.NoError(t, s.CreateUser(t.Context(), u))
	return u
}

func TestUsers(t *testing.T) {
	s := newStore(t)
	u := seedUser(t, s)

	got, err := s.UserByEmail(t.Context(), "a@example.com")
	require.NoError(t, err)
	require.Equal(t, u.ID, got.ID)

	got, err = s.UserByID(t.Context(), "u1")
	require.NoError(t, err)
	require.Equal(t, "A", got.DisplayName)

	_, err = s.UserByID(t.Context(), "nope")
	require.ErrorIs(t, err, store.ErrNotFound)

	// duplicate email -> conflict
	err = s.CreateUser(t.Context(), &model.User{ID: "u2", Email: "a@example.com", DisplayName: "B"})
	require.ErrorIs(t, err, store.ErrConflict)
}

func TestUserOIDC(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	// no subject linked yet
	_, err := s.UserByOIDCSubject(ctx, "https://idp", "sub-1")
	require.ErrorIs(t, err, store.ErrNotFound)

	// password users (empty subject) must not collide on the partial unique index
	require.NoError(t, s.CreateUser(ctx, &model.User{ID: "p1", Email: "p1@x.y", DisplayName: "P1", PasswordHash: "h"}))
	require.NoError(t, s.CreateUser(ctx, &model.User{ID: "p2", Email: "p2@x.y", DisplayName: "P2", PasswordHash: "h"}))

	// link an account to an OIDC identity
	u := &model.User{ID: "o1", Email: "o@x.y", DisplayName: "O", OIDCIssuer: "https://idp", OIDCSubject: "sub-1", CreatedAt: time.Now()}
	require.NoError(t, s.CreateUser(ctx, u))

	got, err := s.UserByOIDCSubject(ctx, "https://idp", "sub-1")
	require.NoError(t, err)
	require.Equal(t, "o1", got.ID)

	// a second account claiming the same subject is rejected
	require.ErrorIs(t, s.CreateUser(ctx, &model.User{ID: "o2", Email: "o2@x.y", DisplayName: "O2",
		OIDCIssuer: "https://idp", OIDCSubject: "sub-1"}), store.ErrConflict)

	// UpdateUser can link a previously password-only account
	p1, err := s.UserByEmail(ctx, "p1@x.y")
	require.NoError(t, err)
	p1.OIDCIssuer = "https://idp"
	p1.OIDCSubject = "sub-2"
	require.NoError(t, s.UpdateUser(ctx, p1))
	linked, err := s.UserByOIDCSubject(ctx, "https://idp", "sub-2")
	require.NoError(t, err)
	require.Equal(t, "p1", linked.ID)
}

func TestChannelsAndEpisodes(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()

	ch := &model.Channel{ID: "c1", UserID: "u1", Title: "Show", Language: "en",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, s.CreateChannel(ctx, ch))

	byID, err := s.ChannelByID(ctx, "c1")
	require.NoError(t, err)
	require.Equal(t, "Show", byID.Title)

	// reusing an id is a conflict
	require.ErrorIs(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "Dup"}), store.ErrConflict)

	byUser, err := s.ListChannelsByUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, byUser, 1)

	pub := time.Now()
	ep := &model.Episode{ID: "e1", ChannelID: "c1", Title: "Ep 1",
		MediaKey: "k1", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio, MediaBytes: 100,
		Status:           model.EpisodePublished,
		TranscriptStatus: model.TranscriptPending, PublishedAt: &pub, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, s.CreateEpisode(ctx, ep))

	draft := &model.Episode{ID: "e2", ChannelID: "c1", Title: "Draft",
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptNone, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, s.CreateEpisode(ctx, draft))

	all, err := s.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
	require.NoError(t, err)
	require.Len(t, all, 2)

	published, err := s.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1", PublishedOnly: true})
	require.NoError(t, err)
	require.Len(t, published, 1)
	require.Equal(t, "e1", published[0].ID)
	require.Equal(t, model.MediaAudio, published[0].MediaKind)

	byMedia, err := s.EpisodeByMediaKey(ctx, "k1")
	require.NoError(t, err)
	require.Equal(t, "e1", byMedia.ID)

	// a video episode round-trips its kind
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "ev", ChannelID: "c1", Title: "Vid",
		MediaKey: "kv", MediaMIME: "video/mp4", MediaKind: model.MediaVideo, Status: model.EpisodeDraft,
		TranscriptStatus: model.TranscriptNone, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	vid, err := s.EpisodeByID(ctx, "ev")
	require.NoError(t, err)
	require.True(t, vid.IsVideo())

	// reusing an id is a conflict
	require.ErrorIs(t, s.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1",
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptNone}), store.ErrConflict)

	require.NoError(t, s.DeleteEpisode(ctx, "e2"))
	require.ErrorIs(t, s.DeleteEpisode(ctx, "e2"), store.ErrNotFound)
}

func TestTranscriptRoundTrip(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	tr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: time.Now(),
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 1.5, Text: "hi"}, {StartSecs: 1.5, EndSecs: 3, Text: "there"}}}
	require.NoError(t, s.SaveTranscript(ctx, tr))

	got, err := s.TranscriptByEpisode(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, "en", got.Language)
	require.Len(t, got.Segments, 2)
	require.Equal(t, "there", got.Segments[1].Text)

	// upsert replaces
	tr.Segments = []model.Segment{{StartSecs: 0, EndSecs: 1, Text: "only"}}
	require.NoError(t, s.SaveTranscript(ctx, tr))
	got, err = s.TranscriptByEpisode(ctx, "e1")
	require.NoError(t, err)
	require.Len(t, got.Segments, 1)
}

func TestJobClaimLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))

	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, "j1", claimed.ID)
	require.Equal(t, model.JobProcessing, claimed.Status)
	require.Equal(t, 1, claimed.Attempts)

	// second claim finds nothing runnable (leased into the future)
	_, err = s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, s.CompleteJob(ctx, "j1", claimed.Attempts))
	done, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, done.Status)
}

// TestJobSettlementFencedByClaimToken proves the fencing token: after a job's
// lease expires and a second worker reclaims it (advancing Attempts), the
// ORIGINAL attempt's settlement is rejected as a no-op and does not clobber the
// reclaiming attempt's state.
func TestJobSettlementFencedByClaimToken(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))

	// First worker claims the job: token (Attempts) = 1.
	first, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, first.Attempts)

	// Its lease expires and a second worker reclaims the still-"running" job: the
	// token advances to 2.
	later := now.Add(2 * time.Minute)
	second, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, later, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 2, second.Attempts)

	// The original attempt now tries to settle with its stale token: rejected.
	require.ErrorIs(t, s.CompleteJob(ctx, "j1", first.Attempts), store.ErrStaleClaim)
	require.ErrorIs(t, s.FailJob(ctx, "j1", first.Attempts, "stale"), store.ErrStaleClaim)
	require.ErrorIs(t, s.RescheduleJob(ctx, "j1", first.Attempts, later, "stale"), store.ErrStaleClaim)

	// The job still belongs to the reclaiming attempt (processing, token 2).
	got, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobProcessing, got.Status)
	require.Equal(t, 2, got.Attempts)

	// The current claim settles normally.
	require.NoError(t, s.CompleteJob(ctx, "j1", second.Attempts))
	got, err = s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, got.Status)
}

func TestJobClaimReclaimsExpiredLease(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	// A job left processing by a crashed worker: its lease (run_after) already
	// expired, so it must be reclaimable rather than stuck forever.
	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobProcessing, Attempts: 1, RunAfter: now.Add(-time.Minute),
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}))

	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, "j1", claimed.ID)
	require.Equal(t, model.JobProcessing, claimed.Status)
	require.Equal(t, 2, claimed.Attempts) // the reclaim consumes a fresh attempt
	require.True(t, claimed.RunAfter.After(now))

	// The fresh lease pushes run_after into the future, so it is not reclaimed again.
	_, err = s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestJobClaimLeaseNotReclaimedBeforeDeadline(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()

	// Claim at a sub-second offset. The true deadline is claimAt+lease with
	// millisecond precision; a lease truncated to whole seconds would expire
	// ~0.9s early, so the deadline must be rounded up when persisted.
	base := time.Unix(1_000_000, 0).UTC()
	claimAt := base.Add(900 * time.Millisecond)
	lease := 10 * time.Minute

	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobPending, RunAfter: claimAt, CreatedAt: claimAt, UpdatedAt: claimAt}))

	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, claimAt, lease)
	require.NoError(t, err)
	require.Equal(t, "j1", claimed.ID)

	// Past the naively-truncated deadline (base+lease) but before the true
	// deadline (claimAt+lease): the lease must not be reclaimable yet.
	justBefore := base.Add(lease).Add(100 * time.Millisecond)
	require.True(t, justBefore.Before(claimAt.Add(lease)))
	_, err = s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, justBefore, lease)
	require.ErrorIs(t, err, store.ErrNotFound)

	// After the true deadline: reclaimable again.
	after := claimAt.Add(lease).Add(time.Second)
	reclaimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, after, lease)
	require.NoError(t, err)
	require.Equal(t, "j1", reclaimed.ID)
}
