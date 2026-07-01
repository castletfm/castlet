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

func TestBumpSessionEpoch(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	u := seedUser(t, s)
	require.Zero(t, u.SessionEpoch, "new users start at epoch 0")

	require.NoError(t, s.BumpSessionEpoch(ctx, u.ID))
	got, err := s.UserByID(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.SessionEpoch)

	require.NoError(t, s.BumpSessionEpoch(ctx, u.ID))
	got, err = s.UserByID(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, 2, got.SessionEpoch)

	require.ErrorIs(t, s.BumpSessionEpoch(ctx, "nope"), store.ErrNotFound)
}

// CreateUser must never persist a caller-supplied session_epoch: the column
// defaults to 0 and BumpSessionEpoch is its sole writer. Writing it here would
// let a stale in-memory epoch seed a revoked-looking (or pre-bumped) value.
func TestCreateUserIgnoresSessionEpoch(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	require.NoError(t, s.CreateUser(ctx, &model.User{
		ID: "u1", Email: "a@example.com", DisplayName: "A", PasswordHash: "x",
		SessionEpoch: 99, CreatedAt: time.Now(),
	}))

	got, err := s.UserByID(ctx, "u1")
	require.NoError(t, err)
	require.Zero(t, got.SessionEpoch, "CreateUser must ignore a caller-supplied epoch and default to 0")
}

// A generic UpdateUser must never write session_epoch: a stale user struct
// (holding an older epoch) must not clobber an epoch a prior BumpSessionEpoch
// already advanced, which would re-validate cookies a "log out everywhere"
// revoked.
func TestUpdateUserDoesNotClobberSessionEpoch(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	u := seedUser(t, s) // epoch 0
	require.NoError(t, s.BumpSessionEpoch(ctx, u.ID))

	// u still carries the stale epoch 0; write it back via a normal update.
	u.DisplayName = "Renamed"
	require.NoError(t, s.UpdateUser(ctx, u))

	got, err := s.UserByID(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, "Renamed", got.DisplayName, "UpdateUser still persists other fields")
	require.Equal(t, 1, got.SessionEpoch, "UpdateUser must not reset a bumped epoch")
}

// Migrate must be safe to run repeatedly (it runs on every startup), including
// its ALTER TABLE backfills, and must preserve existing data.
func TestMigrateIdempotent(t *testing.T) {
	s := newStore(t) // already migrated once by newStore
	ctx := t.Context()
	u := seedUser(t, s)
	require.NoError(t, s.BumpSessionEpoch(ctx, u.ID))

	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.Migrate(ctx))

	got, err := s.UserByID(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.SessionEpoch, "re-running Migrate must not reset data")
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

	_, _, err = s.DeleteEpisode(ctx, "e2", time.Now())
	require.NoError(t, err)
	_, _, err = s.DeleteEpisode(ctx, "e2", time.Now())
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestDeleteEpisodeOrphan proves DeleteEpisode's atomic orphan report (CST-013):
// a shared, content-addressed media key is reported orphaned only once the LAST
// referencing episode is gone, and a channel's cover art keeps a key alive.
func TestDeleteEpisodeOrphan(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	mkEp := func(id, key string) *model.Episode {
		return &model.Episode{ID: id, ChannelID: "c1", Title: id, MediaKey: key,
			MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio, Status: model.EpisodeDraft,
			TranscriptStatus: model.TranscriptNone, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	}

	// Two episodes share one content-addressed key (identical uploaded bytes).
	require.NoError(t, s.CreateEpisode(ctx, mkEp("e1", "shared")))
	require.NoError(t, s.CreateEpisode(ctx, mkEp("e2", "shared")))

	now := time.Now()

	// Deleting the first must NOT orphan the blob: e2 still references it.
	key, orphaned, err := s.DeleteEpisode(ctx, "e1", now)
	require.NoError(t, err)
	require.Equal(t, "shared", key)
	require.False(t, orphaned, "blob is still referenced by e2")

	// Deleting the last referencing episode orphans the blob.
	key, orphaned, err = s.DeleteEpisode(ctx, "e2", now)
	require.NoError(t, err)
	require.Equal(t, "shared", key)
	require.True(t, orphaned, "no episode references the blob anymore")

	// A channel cover art protects a shared key: an episode whose media key equals
	// a channel's image key is not orphaned when deleted.
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c2", UserID: "u1", Title: "Cover",
		ImageKey: "img", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, s.CreateEpisode(ctx, mkEp("e3", "img")))
	key, orphaned, err = s.DeleteEpisode(ctx, "e3", now)
	require.NoError(t, err)
	require.Equal(t, "img", key)
	require.False(t, orphaned, "channel cover art still references the key")

	// An episode with no media key is never orphaned (there is no blob to delete).
	require.NoError(t, s.CreateEpisode(ctx, mkEp("e4", "")))
	key, orphaned, err = s.DeleteEpisode(ctx, "e4", now)
	require.NoError(t, err)
	require.Empty(t, key)
	require.False(t, orphaned)
}

// TestBlobReservationOrphan proves the reservation table closes the sibling race
// (an in-flight upload writes the blob before its episode row exists): an active
// reservation keeps DeleteEpisode/BlobOrphaned from reporting a key orphaned even
// when no episode references it, releasing (by token) frees it, and a stale (older
// than the TTL) reservation no longer protects the key so a crashed upload cannot
// pin a blob forever. A BlobOrphaned==true result takes a delete lease that the
// caller releases after the notional physical delete (as the handler does).
func TestBlobReservationOrphan(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	now := time.Now()
	ep := &model.Episode{ID: "e1", ChannelID: "c1", Title: "Ep", MediaKey: "k",
		MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio, Status: model.EpisodeDraft,
		TranscriptStatus: model.TranscriptNone, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateEpisode(ctx, ep))

	// Simulate a concurrent upload of the SAME content-addressed key that has
	// written its blob (Put) but not yet committed its episode row: it holds a
	// reservation. Deleting the only existing episode must NOT orphan the key.
	token, err := s.ReserveBlob(ctx, "k", now)
	require.NoError(t, err)
	key, orphaned, err := s.DeleteEpisode(ctx, "e1", now)
	require.NoError(t, err)
	require.Equal(t, "k", key)
	require.False(t, orphaned, "an active reservation must keep the blob alive")

	// BlobOrphaned (the rollback path) sees the reservation too.
	got, err := s.BlobOrphaned(ctx, "k", now)
	require.NoError(t, err)
	require.False(t, got, "an active reservation must keep the blob alive")

	// Once the upload releases its reservation and no episode references the key,
	// the blob is orphaned (and a delete lease is taken; release it afterwards).
	require.NoError(t, s.ReleaseBlob(ctx, token))
	got, err = s.BlobOrphaned(ctx, "k", now)
	require.NoError(t, err)
	require.True(t, got, "no episode and no reservation -> orphaned")
	require.NoError(t, s.ReleaseDeleteLease(ctx, "k"))

	// A reservation older than the TTL is stale (a crashed upload) and must not
	// protect the key: evaluating "now" well past the reservation's timestamp
	// treats it as expired.
	staleToken, err := s.ReserveBlob(ctx, "k", now)
	require.NoError(t, err)
	future := now.Add(48 * time.Hour) // beyond blobReservationTTL
	got, err = s.BlobOrphaned(ctx, "k", future)
	require.NoError(t, err)
	require.True(t, got, "a stale reservation must not pin the blob")
	require.NoError(t, s.ReleaseDeleteLease(ctx, "k"))
	require.NoError(t, s.ReleaseBlob(ctx, staleToken))

	// Concurrent identical uploads act as a refcount: two reservations, releasing
	// one still leaves the key protected.
	t1, err := s.ReserveBlob(ctx, "k", now)
	require.NoError(t, err)
	t2, err := s.ReserveBlob(ctx, "k", now)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseBlob(ctx, t1)) // drop one
	got, err = s.BlobOrphaned(ctx, "k", now)
	require.NoError(t, err)
	require.False(t, got, "a remaining reservation still protects the key")
	require.NoError(t, s.ReleaseBlob(ctx, t2))
}

// TestBlobDeleteLease proves the delete lease closes the residual window in which
// a reservation created after the delete transaction commits but before the
// physical blobs.Delete would be invisible: while the lease is held ReserveBlob is
// rejected (ErrBlobDeleting) so a same-content upload retries instead of racing
// the delete; a stale lease (past its TTL) no longer blocks, and after the lease
// is released reservation succeeds again.
func TestBlobDeleteLease(t *testing.T) {
	s := newStore(t)
	seedUser(t, s)
	ctx := t.Context()
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	now := time.Now()
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		MediaKey: "k", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio, Status: model.EpisodeDraft,
		TranscriptStatus: model.TranscriptNone, CreatedAt: now, UpdatedAt: now}))

	// Deleting the only referencing episode orphans "k" and takes a delete lease
	// atomically with the decision.
	_, orphaned, err := s.DeleteEpisode(ctx, "e1", now)
	require.NoError(t, err)
	require.True(t, orphaned)

	// A concurrent upload reserving the same key while the lease is held is rejected.
	_, err = s.ReserveBlob(ctx, "k", now)
	require.ErrorIs(t, err, store.ErrBlobDeleting)

	// A stale lease (older than blobDeleteLeaseTTL) no longer blocks reservations.
	future := now.Add(10 * time.Minute) // beyond blobDeleteLeaseTTL
	tok, err := s.ReserveBlob(ctx, "k", future)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseBlob(ctx, tok))

	// After the physical delete completes and the lease is released, reservation
	// succeeds normally again.
	require.NoError(t, s.ReleaseDeleteLease(ctx, "k"))
	tok, err = s.ReserveBlob(ctx, "k", now)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseBlob(ctx, tok))
}

// TestReleaseBlobByToken proves ReleaseBlob drops exactly the caller's reservation
// (identified by its token), never an arbitrary row — so releasing upload A does
// not drop a concurrent upload B's reservation for the same key.
func TestReleaseBlobByToken(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	tokenA, err := s.ReserveBlob(ctx, "k", now)
	require.NoError(t, err)
	tokenB, err := s.ReserveBlob(ctx, "k", now)
	require.NoError(t, err)
	require.NotEqual(t, tokenA, tokenB)

	// Releasing A must leave B's reservation intact, so the key is still protected.
	require.NoError(t, s.ReleaseBlob(ctx, tokenA))
	got, err := s.BlobOrphaned(ctx, "k", now)
	require.NoError(t, err)
	require.False(t, got, "B's reservation must still protect the key")

	// After B releases too, nothing references the key.
	require.NoError(t, s.ReleaseBlob(ctx, tokenB))
	got, err = s.BlobOrphaned(ctx, "k", now)
	require.NoError(t, err)
	require.True(t, got)
	require.NoError(t, s.ReleaseDeleteLease(ctx, "k"))
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

// TestSettleEpisodeTranscriptFencedByClaimToken proves that the episode and
// transcript side effects are fenced by the job's claim exactly like the queue
// job status: once the lease expires and another worker reclaims the job
// (advancing Attempts), the ORIGINAL attempt can no longer write the episode
// status or save a transcript — its fenced call is a no-op returning
// ErrStaleClaim — while the reclaiming attempt's write succeeds.
func TestSettleEpisodeTranscriptFencedByClaimToken(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	seedUser(t, s)
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptPending, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe,
		Payload: `{"episode_id":"e1"}`, Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))

	// First worker claims the job: token (Attempts) = 1.
	first, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, first.Attempts)

	// The current claim can settle a non-terminal progress marker.
	require.NoError(t, s.SettleEpisodeTranscript(ctx, "j1", first.Attempts, "e1", nil, model.TranscriptProcessing, now))
	ep, err := s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptProcessing, ep.TranscriptStatus)

	// Its lease expires and a second worker reclaims the still-"running" job,
	// advancing the token to 2.
	later := now.Add(2 * time.Minute)
	second, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, later, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 2, second.Attempts)

	// The original attempt (stale token 1) now tries to settle a done result:
	// rejected as a no-op. Neither the transcript nor the status is written.
	staleTr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: later,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 1, Text: "stale"}}}
	require.ErrorIs(t, s.SettleEpisodeTranscript(ctx, "j1", first.Attempts, "e1", staleTr, model.TranscriptDone, later),
		store.ErrStaleClaim)
	_, err = s.TranscriptByEpisode(ctx, "e1")
	require.ErrorIs(t, err, store.ErrNotFound, "stale attempt must not save a transcript")
	ep, err = s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptProcessing, ep.TranscriptStatus,
		"stale attempt must not overwrite the episode status")

	// The reclaiming attempt (current token 2) settles normally: transcript saved
	// and status advanced to done.
	freshTr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: later,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 2, Text: "fresh"}}}
	require.NoError(t, s.SettleEpisodeTranscript(ctx, "j1", second.Attempts, "e1", freshTr, model.TranscriptDone, later))
	got, err := s.TranscriptByEpisode(ctx, "e1")
	require.NoError(t, err)
	require.Len(t, got.Segments, 1)
	require.Equal(t, "fresh", got.Segments[0].Text)
	ep, err = s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptDone, ep.TranscriptStatus)
}

// TestTerminalJobStatusIsFinal proves the state-machine guard: once a job is
// settled to a terminal status, the SAME claim token can no longer move it. A
// Complete followed by a stray Reschedule (same token, attempts still matching)
// must be a no-op returning ErrStaleClaim, leaving the job done.
func TestTerminalJobStatusIsFinal(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))

	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)

	// Settle to done, then try to move the already-terminal job back with the
	// SAME (still-matching) token: rejected, the job stays done.
	require.NoError(t, s.CompleteJob(ctx, "j1", claimed.Attempts))
	later := now.Add(time.Minute)
	require.ErrorIs(t, s.RescheduleJob(ctx, "j1", claimed.Attempts, later, "stray"), store.ErrStaleClaim)
	require.ErrorIs(t, s.FailJob(ctx, "j1", claimed.Attempts, "stray"), store.ErrStaleClaim)
	require.ErrorIs(t, s.CompleteJob(ctx, "j1", claimed.Attempts), store.ErrStaleClaim)

	got, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, got.Status, "terminal state must be final")
}

// TestSettleEpisodeTranscriptRejectsPostFailure proves the settlement state
// machine: after a job is dead-lettered (FailJob) and its episode marked failed,
// a same-token done-settlement must NOT resurrect the episode. The job is already
// 'failed', so a done write (or any transcript save) is rejected as ErrStaleClaim.
func TestSettleEpisodeTranscriptRejectsPostFailure(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	seedUser(t, s)
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptPending, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe,
		Payload: `{"episode_id":"e1"}`, Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))

	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)

	// Dead-letter the job, then record the failed episode with the same token: the
	// transcript-less TranscriptFailed mark legitimately runs against the failed job.
	require.NoError(t, s.FailJob(ctx, "j1", claimed.Attempts, "dead"))
	require.NoError(t, s.SettleEpisodeTranscript(ctx, "j1", claimed.Attempts, "e1", nil, model.TranscriptFailed, now))
	ep, err := s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptFailed, ep.TranscriptStatus)

	// A same-token done-settlement after failure must NOT save a transcript nor set
	// the episode done: the job is 'failed', not 'processing'.
	tr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: now,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 1, Text: "late"}}}
	require.ErrorIs(t, s.SettleEpisodeTranscript(ctx, "j1", claimed.Attempts, "e1", tr, model.TranscriptDone, now),
		store.ErrStaleClaim)
	_, err = s.TranscriptByEpisode(ctx, "e1")
	require.ErrorIs(t, err, store.ErrNotFound, "no transcript may be saved after dead-lettering")
	ep, err = s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptFailed, ep.TranscriptStatus, "episode must not be resurrected to done")
}

// seedEpisodeJob creates a channel/episode and a pending transcribe job for it,
// so the combined-settlement tests have a claimable job whose episode side
// effects can be settled.
func seedEpisodeJob(t *testing.T, s *sqlite.Store, now time.Time) {
	t.Helper()
	ctx := t.Context()
	seedUser(t, s)
	require.NoError(t, s.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptProcessing, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe,
		Payload: `{"episode_id":"e1"}`, Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))
}

// TestSettleEpisodeTranscriptAndCompleteJobAtomic proves the SUCCESS-path
// settlement writes the transcript, the episode status, AND the job completion
// as one fenced unit for the live claim.
func TestSettleEpisodeTranscriptAndCompleteJobAtomic(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()
	seedEpisodeJob(t, s, now)

	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, claimed.Attempts)

	tr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: now,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 1, Text: "hi"}}}
	require.NoError(t, s.SettleEpisodeTranscriptAndCompleteJob(ctx, "j1", claimed.Attempts, "e1", tr, model.TranscriptDone, now))

	job, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, job.Status, "job must be completed in the same transaction")

	ep, err := s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptDone, ep.TranscriptStatus)

	got, err := s.TranscriptByEpisode(ctx, "e1")
	require.NoError(t, err)
	require.Len(t, got.Segments, 1)
	require.Equal(t, "hi", got.Segments[0].Text)
}

// TestSettleEpisodeTranscriptAndCompleteJobFencedByReclaim is the core anti-
// downgrade proof: a reclaim that lands before the original attempt's combined
// settlement makes the whole settlement (episode status, transcript, AND job
// completion) a single no-op returning ErrStaleClaim — the reclaiming attempt is
// never downgraded and the job is never double-completed. This is exactly the
// window that a separate settle-then-Ack left open.
func TestSettleEpisodeTranscriptAndCompleteJobFencedByReclaim(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()
	seedEpisodeJob(t, s, now)

	// First worker claims (token 1) and finishes its (slow) transcription.
	first, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, first.Attempts)

	// Its lease expires and a second worker reclaims the job, advancing the token.
	later := now.Add(2 * time.Minute)
	second, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, later, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 2, second.Attempts)

	// The original attempt (stale token 1) now tries to settle its done result:
	// the whole combined unit is rejected. Nothing is written.
	staleTr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: later,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 1, Text: "stale"}}}
	require.ErrorIs(t,
		s.SettleEpisodeTranscriptAndCompleteJob(ctx, "j1", first.Attempts, "e1", staleTr, model.TranscriptDone, later),
		store.ErrStaleClaim)

	job, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobProcessing, job.Status, "stale settlement must not complete the reclaimed job")
	require.Equal(t, 2, job.Attempts)
	ep, err := s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptProcessing, ep.TranscriptStatus, "stale settlement must not downgrade the episode")
	_, err = s.TranscriptByEpisode(ctx, "e1")
	require.ErrorIs(t, err, store.ErrNotFound, "stale settlement must not save a transcript")

	// The reclaiming attempt (token 2) settles atomically and owns the outcome.
	freshTr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: later,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 2, Text: "fresh"}}}
	require.NoError(t, s.SettleEpisodeTranscriptAndCompleteJob(ctx, "j1", second.Attempts, "e1", freshTr, model.TranscriptDone, later))
	job, err = s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, job.Status)
	ep, err = s.EpisodeByID(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptDone, ep.TranscriptStatus)
}

// TestSettleEpisodeTranscriptAndCompleteJobRejectsTerminal proves the combined
// settlement cannot re-fire: once the job is done, a second call with the SAME
// (still-matching) token is a no-op returning ErrStaleClaim, so a duplicate
// settlement can neither double-complete the job nor rewrite the episode.
func TestSettleEpisodeTranscriptAndCompleteJobRejectsTerminal(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()
	seedEpisodeJob(t, s, now)

	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)

	tr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: now,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 1, Text: "one"}}}
	require.NoError(t, s.SettleEpisodeTranscriptAndCompleteJob(ctx, "j1", claimed.Attempts, "e1", tr, model.TranscriptDone, now))

	// Second call with the same token: the job is already 'done', not 'processing'.
	dup := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: now,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 9, Text: "two"}}}
	require.ErrorIs(t,
		s.SettleEpisodeTranscriptAndCompleteJob(ctx, "j1", claimed.Attempts, "e1", dup, model.TranscriptDone, now),
		store.ErrStaleClaim)

	got, err := s.TranscriptByEpisode(ctx, "e1")
	require.NoError(t, err)
	require.Equal(t, "one", got.Segments[0].Text, "a duplicate settlement must not overwrite the transcript")
}

// TestSettleEpisodeTranscriptAndCompleteJobClearsPriorError proves the combined
// success path wipes a message left by a prior failed attempt: a job that fails
// once (RescheduleJob records last_error), is retried, then succeeds via the
// combined settlement must end up 'done' with last_error empty — matching
// CompleteJob -> setJobStatus, which always clears last_error on completion.
func TestSettleEpisodeTranscriptAndCompleteJobClearsPriorError(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()
	seedEpisodeJob(t, s, now)

	// First attempt claims, then fails transiently and reschedules with a cause.
	first, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
	require.NoError(t, err)
	later := now.Add(2 * time.Minute)
	require.NoError(t, s.RescheduleJob(ctx, "j1", first.Attempts, later, "boom"))

	failed, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, "boom", failed.LastError, "reschedule must record the failure cause")

	// Retry: re-claim (advancing the token) and settle successfully.
	second, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, later, time.Minute)
	require.NoError(t, err)
	tr := &model.Transcript{EpisodeID: "e1", Language: "en", CreatedAt: later,
		Segments: []model.Segment{{StartSecs: 0, EndSecs: 1, Text: "ok"}}}
	require.NoError(t, s.SettleEpisodeTranscriptAndCompleteJob(ctx, "j1", second.Attempts, "e1", tr, model.TranscriptDone, later))

	job, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobDone, job.Status)
	require.Empty(t, job.LastError, "successful completion must clear the prior failure message")
}

// TestClaimJobNoDoubleClaim proves the ClaimJob guard: with many workers racing
// for a single pending job, exactly one claims it (the others see ErrNotFound)
// and its attempts advance by exactly one. The conditional UPDATE + RowsAffected
// check ensures a job is never handed to two workers as claimed.
func TestClaimJobNoDoubleClaim(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))

	const racers = 8
	type result struct {
		job *model.Job
		err error
	}
	results := make(chan result, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			j, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, time.Minute)
			results <- result{job: j, err: err}
		}()
	}
	close(start)

	claims := 0
	for range racers {
		r := <-results
		if r.err == nil {
			claims++
			require.Equal(t, "j1", r.job.ID)
			require.Equal(t, 1, r.job.Attempts, "the single claim must consume exactly one attempt")
			continue
		}
		require.ErrorIs(t, r.err, store.ErrNotFound, "a losing racer must see nothing runnable, not a double-claim")
	}
	require.Equal(t, 1, claims, "exactly one worker may claim the job")

	got, err := s.JobByID(ctx, "j1")
	require.NoError(t, err)
	require.Equal(t, model.JobProcessing, got.Status)
	require.Equal(t, 1, got.Attempts, "attempts must not be double-incremented by a double-claim")
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

// TestCountPendingJobsCountsRunnableBacklog proves the queue-depth gauge counts
// EXACTLY the set ClaimJob could return right now, one cell of the runnable
// truth table at a time:
//   - a ready pending job (run_after <= now) counts;
//   - a pending retry deferred to a FUTURE run_after (as Nack/RescheduleJob
//     sets) does NOT count — it is not yet runnable;
//   - a processing job whose lease expired (run_after <= now) counts;
//   - a processing job with a live lease (run_after > now) does NOT count.
//
// This guards against the gauge over-reporting future-scheduled retries as
// backlog, and under-reporting a crashed/expired job that is immediately
// reclaimable.
func TestCountPendingJobsCountsRunnableBacklog(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()

	// A plain pending job (due now) is part of the backlog.
	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "pending", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobPending, RunAfter: now, CreatedAt: now, UpdatedAt: now}))

	// A pending job scheduled far in the future (a deferred retry) is NOT yet
	// runnable and must never be counted at any instant tested below.
	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "future", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobPending, RunAfter: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}))

	n, err := s.CountPendingJobs(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a future-scheduled pending retry is not runnable backlog")

	// Claim the ready one: now processing with a live lease, so it is NOT counted;
	// the future pending job is still not due either.
	lease := time.Minute
	claimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, now, lease)
	require.NoError(t, err)
	require.Equal(t, "pending", claimed.ID)

	n, err = s.CountPendingJobs(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 0, n, "a processing job with a live lease is not runnable")

	// Once the lease expires, the crashed job is reclaimable and counts again. The
	// future pending job (now+1h) is still not due at now+lease+1s, so the count
	// stays at 1.
	afterExpiry := now.Add(lease).Add(time.Second)
	n, err = s.CountPendingJobs(ctx, afterExpiry)
	require.NoError(t, err)
	require.Equal(t, 1, n, "an expired processing job is reclaimable backlog")

	// The count matches ClaimJob's reclaim decision at the same instant.
	reclaimed, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, afterExpiry, lease)
	require.NoError(t, err)
	require.Equal(t, "pending", reclaimed.ID)
}
