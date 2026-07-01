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
	// (e.g. a duplicate email or id), and by EnqueueTranscriptionJob when the
	// target episode is already pending/processing (a transcription is already
	// queued or running) — in that case no additional job is queued and the
	// existing job and status are left untouched.
	ErrConflict = errors.New("store: conflict")
	// ErrStaleClaim is returned by the job settlement methods (CompleteJob,
	// RescheduleJob, FailJob, SettleEpisodeTranscript, and the combined
	// SettleEpisodeTranscriptAndCompleteJob) when the settlement is no longer valid.
	// Settlement requires a matching claim token plus the method's allowed source
	// state — normally the job still being in 'processing', with one exception:
	// SettleEpisodeTranscript's transcript-less TranscriptFailed mark is also accepted
	// just after the job has moved to 'failed' (the dead-letter / attempts-exhausted path).
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
	// ErrBlobDeleting is returned by ReserveBlob when an active delete lease exists
	// for the media key — a physical blob delete for that key is in progress. The
	// caller should retry the reservation shortly (the delete is quick); once the
	// lease clears the reservation succeeds. It prevents a same-content upload from
	// reserving a key whose bytes are about to be physically deleted.
	ErrBlobDeleting = errors.New("store: blob delete in progress")
)

// Blob-lifecycle timing bounds. These live together, and in the interface
// package both the store impl and the server depend on, so the two load-bearing
// inequalities can be read and reasoned about in one place:
//
//	BlobDeleteTimeout  <  BlobDeleteLeaseTTL   (a live physical delete cannot outlive its lease)
//	UploadProtectWindow <  BlobReservationTTL  (a live upload commits its episode while its reservation is still fresh)
//
// The TTLs are the *upper* bounds used only to reclaim state abandoned by a crash;
// the timeouts are the *hard* bounds enforced on live operations. Because each
// enforced timeout is strictly (and comfortably) smaller than the TTL that would
// otherwise expire the corresponding lease/reservation, no live operation can ever
// race a reclaim of its own state.
const (
	// BlobReservationTTL bounds how long a LEAKED reservation delays orphan
	// cleanup: a live upload is always represented by its present reservation
	// (see UploadProtectWindow), so a large value never risks deleting a needed
	// blob — it only postpones reclaiming a blob whose upload crashed without
	// releasing. Deliberately generous.
	BlobReservationTTL = 24 * time.Hour
	// UploadProtectWindow is the hard deadline on the reserve->Put->CreateEpisode
	// section. It must be >= the longest plausible time to store the blob and
	// insert the row, yet strictly < BlobReservationTTL, so a live upload's
	// CreateEpisode always commits while its reservation is still counted (never
	// aged out). If exceeded, the context aborts the upload before the reservation
	// could be treated as stale.
	UploadProtectWindow = time.Hour
	// BlobDeleteLeaseTTL bounds how long a delete lease blocks reservations for its
	// key. A lease left behind by a crashed delete handler is ignored after this,
	// so uploads of that key are not blocked forever.
	BlobDeleteLeaseTTL = 5 * time.Minute
	// BlobDeleteTimeout is the hard deadline on a single physical blob delete. It
	// must be strictly (and comfortably) < BlobDeleteLeaseTTL so a live delete can
	// never outlive its lease: if the delete is still running when its lease would
	// expire, the timeout has already aborted it.
	BlobDeleteTimeout = 30 * time.Second
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
	// UpdateUser persists the mutable user fields. It deliberately does NOT
	// write session_epoch — BumpSessionEpoch is the only mutator of that column
	// — so a stale user struct can never overwrite a newer epoch and re-validate
	// cookies a "log out everywhere" already revoked. A caller that needs the
	// current epoch after an update (e.g. before issuing a session) must reload
	// the user rather than trust the struct it passed in.
	UpdateUser(ctx context.Context, u *model.User) error
	// BumpSessionEpoch increments the user's session epoch, invalidating every
	// session issued before the bump (a "log out everywhere", used on logout and
	// after a password change). Returns ErrNotFound when no user has the id.
	BumpSessionEpoch(ctx context.Context, id string) error
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
	// DeleteEpisode removes the episode identified by id and, in the SAME
	// transaction, decides whether its media blob is now orphaned and, when it is,
	// takes a per-key delete lease. orphaned is true only when after the row is gone
	// no remaining episode references the media key, no channel cover art references
	// it, AND no active (non-stale) blob reservation covers it. Media is
	// content-addressed and immutable, so a key may be shared.
	//
	// Two coupled mechanisms make blob deletion race-free. (1) Coupling the delete
	// and the reference re-check in one transaction closes the TOCTOU where a
	// separate "check references, then delete" lets a concurrent same-content upload
	// insert a new referencing episode between check and delete; counting
	// reservations extends this to the window where a concurrent upload has written
	// the blob (blobs.Put) but not yet committed its episode row. (2) The physical
	// blobs.Delete necessarily runs OUTSIDE this transaction, so a reservation
	// created after this tx commits but before the physical delete would be
	// invisible here; to close that, when the blob is orphaned this method acquires
	// a per-key delete lease in the SAME transaction (single-winner: it does not
	// acquire, and reports orphaned=false, if another delete already holds an active
	// lease for the key). A concurrent ReserveBlob serializes against the lease and
	// is rejected with ErrBlobDeleting until the owner finishes the physical delete
	// and calls ReleaseDeleteLease(deleteToken). now anchors both the reservation-
	// and lease-staleness cutoffs. orphaned is true ONLY when this call both found
	// no references AND acquired the lease; in that case deleteToken identifies the
	// lease the caller MUST release after physically deleting the blob. Returns the
	// episode's media key (empty when it had none) and ErrNotFound (with
	// orphaned=false) when no episode has the id.
	DeleteEpisode(ctx context.Context, id string, now time.Time) (mediaKey string, orphaned bool, deleteToken int64, err error)
	// ReserveBlob records an in-flight media upload for key so a concurrent episode
	// delete's orphan check counts it and cannot delete the blob before the upload's
	// episode row is committed (media is written before its row exists). It returns
	// the reservation's id as a release token for ReleaseBlob. Identical
	// content-addressed concurrent uploads each add a reservation, so reservations
	// act as a refcount. If an active delete lease exists for key (a physical delete
	// is in progress) it reserves nothing and returns ErrBlobDeleting; the caller
	// should retry shortly. now stamps the reservation for staleness expiry and
	// anchors the lease-staleness check.
	ReserveBlob(ctx context.Context, key string, now time.Time) (token int64, err error)
	// ReleaseBlob drops the exact reservation identified by the token ReserveBlob
	// returned, so a release never removes a different concurrent upload's
	// reservation. A missing reservation is not an error.
	ReleaseBlob(ctx context.Context, token int64) error
	// ReleaseDeleteLease drops the exact delete lease identified by deleteToken
	// (returned by DeleteEpisode/BlobOrphaned when they acquired it), unblocking
	// reservations once the physical blob delete has completed. Releasing by token
	// means one deleter's release can never clear a different deleter's still-active
	// lease. A missing lease is not an error.
	ReleaseDeleteLease(ctx context.Context, deleteToken int64) error
	// BlobOrphaned reports whether nothing references the media key (no episode, no
	// channel cover art, no active reservation) and, when nothing does, acquires the
	// per-key delete lease in the SAME transaction — the same single-winner protocol
	// as DeleteEpisode's orphan branch. It backs the failed-create rollback cleanup,
	// which has no episode row to delete. orphaned is true ONLY when this call found
	// no references AND acquired the lease (another in-progress delete makes it
	// return false); in that case deleteToken identifies the lease the caller MUST
	// release after physically deleting the blob. now anchors the reservation- and
	// lease-staleness cutoffs.
	BlobOrphaned(ctx context.Context, key string, now time.Time) (orphaned bool, deleteToken int64, err error)
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
	// EnqueueTranscriptionJob atomically inserts a transcription job AND marks the
	// referenced episode's transcript_status = pending in ONE transaction, so the
	// job and the episode's pending state can never diverge. Either both are
	// committed (a job to run plus an episode that shows pending) or neither is
	// (on any error the transaction rolls back, leaving no job and the episode's
	// prior, non-pending status intact). This closes both windows a two-step
	// enqueue-then-mark leaves open: an episode stuck pending with no job to run
	// it, and a queued job whose episode status was never advanced. The pending
	// transition is the concurrency guard: if the episode is already
	// pending/processing the transaction commits nothing and returns ErrConflict
	// (no second job is queued; the existing job and status are left intact), so
	// two racing enqueues cannot both queue a job. j.Payload must
	// already identify episodeID.
	//
	// The episode->pending transition is also the concurrency guard: the job is
	// inserted ONLY when the episode was not already pending or processing, and
	// that check happens inside the same transaction as the insert. So two
	// concurrent enqueues for one episode cannot both queue a job — the loser sees
	// the episode already pending/processing and returns ErrConflict, writing
	// nothing. Returns ErrNotFound (and writes nothing) when no episode has that
	// id.
	EnqueueTranscriptionJob(ctx context.Context, j *model.Job, episodeID string, updatedAt time.Time) error
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
	// CountPendingJobs returns the size of the runnable job backlog: exactly the
	// jobs ClaimJob could return as of now — pending or processing rows whose
	// run_after is due (run_after <= now). A pending retry deferred to a future
	// run_after is not yet runnable and is not counted; a processing job counts
	// only once its lease has expired. This mirrors ClaimJob's runnable predicate
	// so the backing queue-depth metric neither over- nor under-reports the
	// backlog. now is supplied by the caller so it shares the caller's clock; it
	// should stay cheap.
	CountPendingJobs(ctx context.Context, now time.Time) (int, error)
}
