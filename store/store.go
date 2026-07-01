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

	// Job persistence backs queue/dbqueue. A queue backed by an external
	// service (Redis, SQS) implements queue.JobQueue directly and need not
	// touch these methods.
	EnqueueJob(ctx context.Context, j *model.Job) error
	// JobByID returns a single job, or ErrNotFound.
	JobByID(ctx context.Context, id string) (*model.Job, error)
	// ClaimJob atomically selects the oldest runnable job whose Kind is in
	// kinds and whose RunAfter <= now, marks it processing with the given
	// lease, increments Attempts, and returns it. It returns ErrNotFound when
	// no job is runnable.
	ClaimJob(ctx context.Context, kinds []model.JobKind, now time.Time, lease time.Duration) (*model.Job, error)
	// CompleteJob marks a job done.
	CompleteJob(ctx context.Context, id string) error
	// RescheduleJob returns a job to pending with a new RunAfter and records
	// the cause, for a transient failure.
	RescheduleJob(ctx context.Context, id string, runAfter time.Time, cause string) error
	// FailJob marks a job permanently failed and records the cause.
	FailJob(ctx context.Context, id string, cause string) error
}
