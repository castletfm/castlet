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
	// RescheduleJob, FailJob) when the supplied claim token no longer matches the
	// job's current claim: the job was reclaimed by another worker after its lease
	// expired. The stale worker's settlement is a no-op and must not overwrite the
	// reclaiming attempt's state.
	ErrStaleClaim = errors.New("store: stale job claim")
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

	CreateEpisode(ctx context.Context, e *model.Episode) error
	UpdateEpisode(ctx context.Context, e *model.Episode) error
	// SetEpisodeTranscriptStatus updates only the transcript_status (and
	// updated_at) of an episode. It is a targeted write so a concurrent admin
	// edit to the rest of the row is not clobbered by the transcription worker.
	SetEpisodeTranscriptStatus(ctx context.Context, id string, status model.TranscriptStatus, updatedAt time.Time) error
	DeleteEpisode(ctx context.Context, id string) error
	EpisodeByID(ctx context.Context, id string) (*model.Episode, error)
	// EpisodeByMediaKey finds the episode whose media is stored under key, so
	// the media endpoint can serve it with the right content type. Returns
	// ErrNotFound when no episode references the key.
	EpisodeByMediaKey(ctx context.Context, key string) (*model.Episode, error)
	ListEpisodes(ctx context.Context, f EpisodeFilter) ([]*model.Episode, error)

	// SaveTranscript replaces any existing transcript for the episode.
	SaveTranscript(ctx context.Context, t *model.Transcript) error
	TranscriptByEpisode(ctx context.Context, episodeID string) (*model.Transcript, error)

	// Job persistence backs queue/dbqueue. A queue backed by an external
	// service (Redis, SQS) implements queue.JobQueue directly and need not
	// touch these methods.
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
	// current claim (its Attempts). It returns ErrStaleClaim when the job was
	// reclaimed by another worker, leaving the job untouched.
	CompleteJob(ctx context.Context, id string, token int) error
	// RescheduleJob returns a job to pending with a new RunAfter and records the
	// cause, for a transient failure, but only while token still matches the
	// job's current claim. It returns ErrStaleClaim when the job was reclaimed.
	RescheduleJob(ctx context.Context, id string, token int, runAfter time.Time, cause string) error
	// FailJob marks a job permanently failed and records the cause, but only
	// while token still matches the job's current claim. It returns ErrStaleClaim
	// when the job was reclaimed.
	FailJob(ctx context.Context, id string, token int, cause string) error
}
