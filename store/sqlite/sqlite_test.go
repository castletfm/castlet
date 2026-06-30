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

func TestChannelsAndEpisodes(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()

	ch := &model.Channel{ID: "c1", UserID: "u1", Slug: "show", Title: "Show", Language: "en",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, s.CreateChannel(ctx, ch))

	bySlug, err := s.ChannelBySlug(ctx, "show")
	require.NoError(t, err)
	require.Equal(t, "c1", bySlug.ID)

	require.ErrorIs(t, s.CreateChannel(ctx, &model.Channel{ID: "c2", UserID: "u1", Slug: "show", Title: "Dup"}), store.ErrConflict)

	byUser, err := s.ListChannelsByUser(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, byUser, 1)

	pub := time.Now()
	ep := &model.Episode{ID: "e1", ChannelID: "c1", Slug: "ep1", Title: "Ep 1",
		MediaKey: "k1", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio, MediaBytes: 100,
		Status:           model.EpisodePublished,
		TranscriptStatus: model.TranscriptPending, PublishedAt: &pub, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, s.CreateEpisode(ctx, ep))

	draft := &model.Episode{ID: "e2", ChannelID: "c1", Slug: "ep2", Title: "Draft",
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
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "ev", ChannelID: "c1", Slug: "vid", Title: "Vid",
		MediaKey: "kv", MediaMIME: "video/mp4", MediaKind: model.MediaVideo, Status: model.EpisodeDraft,
		TranscriptStatus: model.TranscriptNone, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	vid, err := s.EpisodeByID(ctx, "ev")
	require.NoError(t, err)
	require.True(t, vid.IsVideo())

	// unique (channel, slug)
	require.ErrorIs(t, s.CreateEpisode(ctx, &model.Episode{ID: "e3", ChannelID: "c1", Slug: "ep1",
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptNone}), store.ErrConflict)

	require.NoError(t, s.DeleteEpisode(ctx, "e2"))
	require.ErrorIs(t, s.DeleteEpisode(ctx, "e2"), store.ErrNotFound)
}

func TestTranscriptRoundTrip(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Slug: "s", Title: "S", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Slug: "e", Title: "E",
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

	require.NoError(t, s.CompleteJob(ctx, "j1"))
	done, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, done.Status)
}
