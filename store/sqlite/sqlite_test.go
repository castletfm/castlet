package sqlite_test

import (
	"context"
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

func TestReorderEpisodes(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	created := time.Now()
	ids := []string{"e1", "e2", "e3", "e4"}
	for i, id := range ids {
		require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: id, ChannelID: "c1", Title: id,
			Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptNone,
			Position: i, CreatedAt: created, UpdatedAt: created}))
	}

	order := func(t *testing.T) []string {
		t.Helper()
		eps, err := s.ListEpisodes(ctx, store.EpisodeFilter{ChannelID: "c1"})
		require.NoError(t, err)
		out := make([]string, len(eps))
		for i, e := range eps {
			out[i] = e.ID
			require.Equal(t, i, e.Position, "positions must be dense")
		}
		return out
	}
	require.Equal(t, []string{"e1", "e2", "e3", "e4"}, order(t))

	// Move e2 down (swap e2/e3) -> only the two moved rows are renumbered.
	moved := created.Add(time.Hour)
	require.NoError(t, s.ReorderEpisodes(ctx, "c1", []string{"e1", "e3", "e2", "e4"}, moved))
	require.Equal(t, []string{"e1", "e3", "e2", "e4"}, order(t))

	// Untouched rows keep their old updated_at; moved rows are bumped.
	e1, err := s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, created.Unix(), e1.UpdatedAt.Unix(), "unmoved episode must not be re-stamped")
	e2, err := s.EpisodeByID(ctx, "e2")
	require.NoError(t, err)
	require.Equal(t, moved.Unix(), e2.UpdatedAt.Unix(), "moved episode must be re-stamped")

	// orderedIDs must be an exact permutation of the channel's current ids. A
	// stale/malformed list is rejected with ErrInvalidReorder and changes nothing.
	before := order(t)
	// (a) duplicate id.
	require.ErrorIs(t, s.ReorderEpisodes(ctx, "c1", []string{"e1", "e1", "e3", "e2"}, moved),
		store.ErrInvalidReorder)
	require.Equal(t, before, order(t), "duplicate id must not persist any change")
	// (b) omitted/missing id (e4 dropped, list too short).
	require.ErrorIs(t, s.ReorderEpisodes(ctx, "c1", []string{"e1", "e3", "e2"}, moved),
		store.ErrInvalidReorder)
	require.Equal(t, before, order(t), "missing id must not persist any change")
	// (c) foreign id (not in this channel, replacing e4).
	require.ErrorIs(t, s.ReorderEpisodes(ctx, "c1", []string{"e1", "e3", "e2", "stray"}, moved),
		store.ErrInvalidReorder)
	require.Equal(t, before, order(t), "foreign id must not persist any change")

	// (d) a valid permutation succeeds and renumbers densely.
	require.NoError(t, s.ReorderEpisodes(ctx, "c1", []string{"e4", "e2", "e3", "e1"}, moved))
	require.Equal(t, []string{"e4", "e2", "e3", "e1"}, order(t))

	// Atomicity: a failing reorder (cancelled context) must be all-or-nothing,
	// leaving no partial renumber behind.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err = s.ReorderEpisodes(cancelled, "c1", []string{"e1", "e3", "e2", "e4"}, moved.Add(time.Hour))
	require.Error(t, err)
	require.Equal(t, []string{"e4", "e2", "e3", "e1"}, order(t), "failed reorder must not persist any change")
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

	require.NoError(t, s.CompleteJob(ctx, "j1"))
	done, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, done.Status)
}
