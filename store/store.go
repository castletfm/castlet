// Package store defines the metadata persistence boundary for Castlet:
// users, channels, episodes, transcripts, and the job table that backs the
// default queue. The default implementation is store/sqlite; an enterprise
// deployment can provide a Postgres implementation of the same interface
// without changing any caller.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/castletfm/castlet/model"
)

// Sentinel errors returned by every Store implementation.
var (
	// ErrNotFound is returned when a lookup matches no row.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a write violates a uniqueness constraint
	// (e.g. a duplicate email or id).
	ErrConflict = errors.New("store: conflict")
	// ErrStaleClaim is returned by the job settlement methods (CompleteJob,
	// RescheduleJob, FailJob, and the combined SettleEpisodeTranscriptAndCompleteJob)
	// when the settlement is no longer valid. Settlement requires BOTH a matching
	// claim token AND the job still being in 'processing'.
	// It therefore covers two cases:
	//   - The supplied claim token no longer matches the job's current claim: the
	//     job was reclaimed by another worker after its lease expired. The stale
	//     worker's settlement is a no-op and must not overwrite the reclaiming
	//     attempt's state.
	//   - The token still matches but the job is already in a terminal state
	//     (already settled): a transition out of a terminal state is rejected, so a
	//     double settlement of the same claim cannot re-fire.
	ErrStaleClaim = errors.New("store: stale job claim")
	// ErrInvalidReorder is returned by ReorderEpisodes when orderedIDs is not an
	// exact permutation of the channel's current episode ids (it contains
	// duplicates, omits a current episode, or names a foreign id). Callers
	// should surface it as a client error (400), not a server error.
	ErrInvalidReorder = errors.New("store: invalid reorder")
)

// EpisodeFilter narrows ListEpisodes. The zero value lists every episode,
// newest first.
type EpisodeFilter struct {
	ChannelID     string // restrict to one channel when non-empty
	PublishedOnly bool   // exclude drafts when true
	Limit         int    // cap results when > 0
}

// Store is the metadata persistence interface. Implementations must be safe
// for concurrent use by multiple goroutines.
type Store interface {
	// Migrate creates or upgrades the schema. Safe to call repeatedly.
	Migrate(ctx context.Context) error
	// Ping verifies the store is reachable, backing the server's readiness
	// probe. It should stay cheap (a connection check or SELECT 1).
	Ping(ctx context.Context) error
	// Close releases underlying resources (connection pool, file handles).
	Close() error

	CreateUser(ctx context.Context, u *model.User) error
	UpdateUser(ctx context.Context, u *model.User) error
	UserByID(ctx context.Context, id string) (*model.User, error)
	UserByEmail(ctx context.Context, email string) (*model.User, error)
	// UserByOIDCSubject finds the user linked to an identity provider subject,
	// or ErrNotFound.
	UserByOIDCSubject(ctx context.Context, issuer, subject string) (*model.User, error)

	CreateChannel(ctx context.Context, c *model.Channel) error
	UpdateChannel(ctx context.Context, c *model.Channel) error
	ChannelByID(ctx context.Context, id string) (*model.Channel, error)
	ListChannels(ctx context.Context) ([]*model.Channel, error)
	ListChannelsByUser(ctx context.Context, userID string) ([]*model.Channel, error)
	// ChannelImageKeyExists reports whether any channel references the blob
	// stored under key as its cover art. Channels are always public, so such a
	// blob may be served even when no published episode references the key.
	ChannelImageKeyExists(ctx context.Context, key string) (bool, error)

	CreateEpisode(ctx context.Context, e *model.Episode) error
	UpdateEpisode(ctx context.Context, e *model.Episode) error
	// SetEpisodeTranscriptStatus updates only the transcript_status (and
	// updated_at) of an episode. It is a targeted write so a concurrent admin
	// edit to the rest of the row is not clobbered by the transcription worker.
	SetEpisodeTranscriptStatus(ctx context.Context, id string, status model.TranscriptStatus, updatedAt time.Time) error
	DeleteEpisode(ctx context.Context, id string) error
	EpisodeByID(ctx context.Context, id string) (*model.Episode, error)
	// EpisodeByMediaKey finds an episode whose media is stored under key. Media
	// is content-addressed, so a key may be shared by several episodes; this
	// returns an arbitrary one and is used only to test whether any episode
	// references the key. Returns ErrNotFound when none does.
	EpisodeByMediaKey(ctx context.Context, key string) (*model.Episode, error)
	// PublishedEpisodeByMediaKey finds a published episode whose media is stored
	// under key, so the media endpoint can both gate on publication and serve the
	// blob with that episode's content type. Media is content-addressed, so a key
	// may be shared by a draft and a published episode; a draft's MIME must not be
	// used when a different published episode is what makes the key public.
	// Returns ErrNotFound when no published episode references the key.
	PublishedEpisodeByMediaKey(ctx context.Context, key string) (*model.Episode, error)
	ListEpisodes(ctx context.Context, f EpisodeFilter) ([]*model.Episode, error)
	// ReorderEpisodes renumbers the given channel's episodes so each id's
	// position equals its index in orderedIDs. orderedIDs must be an exact
	// permutation of the channel's current episode ids; a list with duplicates,
	// a missing current episode, or a foreign id is rejected with
	// ErrInvalidReorder and no rows are changed. All updates run in one
	// transaction, so a mid-way failure cannot leave positions partially
	// renumbered (all-or-nothing). Only the position and updated_at columns are
	// touched; ids already at their target position are left untouched.
	ReorderEpisodes(ctx context.Context, channelID string, orderedIDs []string, updatedAt time.Time) error

	// SaveTranscript replaces any existing transcript for the episode.
	SaveTranscript(ctx context.Context, t *model.Transcript) error
	TranscriptByEpisode(ctx context.Context, episodeID string) (*model.Transcript, error)
	// SettleEpisodeTranscript atomically records a transcription job's episode
	// side effects, fenced by the job's claim so a stale worker cannot clobber a
	// reclaiming attempt. It settles the episode/transcript ONLY if the job
	// identified by jobID is still the active claim: the row must exist with
	// attempts == token, the fencing token that ClaimJob bumps on every reclaim.
	// On top of that token match, the accepted job status follows the real state
	// machine so a same-token call cannot settle an already-terminal job:
	//   - Any transcript save (transcript != nil) and every done/none/processing
	//     transcript_status settlement require the job to still be in
	//     'processing' (the state a live claim holds before its Ack).
	//   - A 'failed' job permits ONLY the transcript-less TranscriptFailed mark
	//     (status == TranscriptFailed with a nil transcript) — the dead-letter /
	//     Nack-exhaustion write that legitimately runs right after the same claim
	//     flipped the job to 'failed'.
	// When the claim holds, in the same transaction it saves transcript when
	// non-nil and applies a targeted write of the episode's transcript_status
	// (leaving the rest of the row untouched, so a concurrent admin edit is not
	// reverted). Otherwise — the job is no longer this attempt's claim (its lease
	// expired and another worker reclaimed it, bumping attempts) or its status
	// does not satisfy the rule above — it returns ErrStaleClaim and changes
	// nothing, so episode/transcript writes are as fenced as the queue's own
	// Ack/Nack.
	SettleEpisodeTranscript(ctx context.Context, jobID string, token int, episodeID string, transcript *model.Transcript, status model.TranscriptStatus, updatedAt time.Time) error
	// SettleEpisodeTranscriptAndCompleteJob is the atomic SUCCESS-path settlement:
	// in ONE transaction it saves the transcript (when non-nil), applies the
	// targeted episode transcript_status write, AND marks the job done — all fenced
	// by the claim (attempts == token AND status == 'processing'). Coupling the job
	// completion to the episode write in a single transaction is what prevents a
	// reclaim from landing between them: with a separate settle then Ack, an expired
	// lease could be reclaimed after the episode was set done, whereupon the
	// reclaiming worker would downgrade the episode back to processing/failed. Here
	// the two are indivisible: either the whole outcome is applied while this attempt
	// still holds the claim, or nothing is (ErrStaleClaim) because the job was
	// reclaimed or is already terminal. A worker-compatible JobQueue must therefore
	// mirror its claims into this store's job row (the dbqueue+sqlite contract, see
	// the Job persistence note below); the worker's success path calls this method
	// instead of a separate SettleEpisodeTranscript + queue Ack.
	SettleEpisodeTranscriptAndCompleteJob(ctx context.Context, jobID string, token int, episodeID string, transcript *model.Transcript, status model.TranscriptStatus, updatedAt time.Time) error

	// Job persistence backs queue/dbqueue. The default worker settles a job's
	// terminal outcome and the job's completion in one fenced transaction via
	// SettleEpisodeTranscriptAndCompleteJob, which couples the queue's claim token
	// to this job row. A JobQueue used with that worker must therefore mirror its
	// claims into these job methods (as queue/dbqueue does). A fully external queue
	// (Redis, SQS) that does not use these methods can only be paired with a worker
	// whose settlement does not couple to the store job row.
	EnqueueJob(ctx context.Context, j *model.Job) error
	// JobByID returns a single job, or ErrNotFound.
	JobByID(ctx context.Context, id string) (*model.Job, error)
	// ClaimJob atomically selects the oldest runnable job whose Kind is in
	// kinds and whose RunAfter <= now, marks it processing with the given
	// lease, increments Attempts, and returns it. A runnable job is either
	// pending or a processing job whose lease expired (reclaimed from a crashed
	// worker). It returns ErrNotFound when no job is runnable.
	//
	// The returned Job.Attempts doubles as the claim's fencing token: it is
	// bumped on every claim, so a reclaim advances it. The settlement methods
	// below take that token and apply only while it still matches, so a stale
	// attempt whose job was reclaimed cannot clobber the reclaiming attempt.
	ClaimJob(ctx context.Context, kinds []model.JobKind, now time.Time, lease time.Duration) (*model.Job, error)
	// CompleteJob marks a job done, but only while token still matches the job's
	// current claim (its Attempts) AND the job is still in 'processing'. It returns
	// ErrStaleClaim when the job was reclaimed by another worker or is already in a
	// terminal state (already settled), leaving the job untouched.
	CompleteJob(ctx context.Context, id string, token int) error
	// RescheduleJob returns a job to pending with a new RunAfter and records the
	// cause, for a transient failure, but only while token still matches the job's
	// current claim AND the job is still in 'processing'. It returns ErrStaleClaim
	// when the job was reclaimed or is already in a terminal state (already
	// settled).
	RescheduleJob(ctx context.Context, id string, token int, runAfter time.Time, cause string) error
	// FailJob marks a job permanently failed and records the cause, but only while
	// token still matches the job's current claim AND the job is still in
	// 'processing'. It returns ErrStaleClaim when the job was reclaimed or is
	// already in a terminal state (already settled).
	FailJob(ctx context.Context, id string, token int, cause string) error
}
