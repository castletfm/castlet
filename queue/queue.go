// Package queue defines the asynchronous job dispatch boundary. The default
// implementation (queue/dbqueue) is backed by the metadata Store so a
// standalone install needs no extra service; an enterprise deployment can
// implement JobQueue over Redis, SQS, or similar.
//
// NOTE: the default worker settles a successful transcription and completes its
// job in ONE fenced store transaction (store.SettleEpisodeTranscriptAndCompleteJob),
// which couples the claim's fencing token to the store's job row. A JobQueue paired
// with that worker must therefore mirror its claims into the store (as dbqueue does
// via store.ClaimJob); a fully external queue that never touches the store job
// methods cannot provide that atomic coupling and must be paired with a worker
// whose settlement does not depend on it.
package queue

import (
	"context"

	"github.com/castletfm/castlet/model"
)

// JobQueue enqueues and dispatches asynchronous work. Implementations own the
// retry policy (attempt limits and backoff); callers express only success
// (Ack) or failure (Nack). Implementations must be safe for concurrent use.
type JobQueue interface {
	// Enqueue persists a new job. payload is marshaled to JSON.
	Enqueue(ctx context.Context, kind model.JobKind, payload any) error
	// EnqueueTranscription enqueues a JobTranscribe for episodeID AND marks that
	// episode's transcript_status = pending as ONE atomic unit: a caller either
	// gets both (a queued job and an episode that shows pending) or neither (on
	// failure no job is queued and the episode keeps its prior, non-pending
	// status). If the episode is already pending or processing, it returns
	// store.ErrConflict: no additional job is queued and the existing job and
	// status are left untouched (distinct from a genuine error, which rolls back
	// any accepted change). This is the only way callers should start transcription, so a
	// queued job and its episode's pending state never diverge — there is never a
	// pending episode with no job to run it, nor a queued job whose episode status
	// was left stale. The store-backed default (dbqueue over sqlite) provides this
	// by inserting the job and marking the episode in a single store transaction;
	// like the worker's fenced settlement (see the package note), a JobQueue built
	// on an external broker must supply the equivalent atomicity.
	//
	// The pending mark is also the guard against overlapping requests: if the
	// episode is already pending or processing no job is queued and store.ErrConflict
	// is returned, so two concurrent starts for one episode cannot both queue a job.
	EnqueueTranscription(ctx context.Context, episodeID string) error
	// Dequeue claims the next runnable job whose Kind is in kinds (all kinds
	// when none are given), marking it in-flight. It returns (nil, false, nil)
	// when no job is currently runnable.
	//
	// A job reclaimed from a crashed worker may have already exhausted its
	// attempts; rather than run it again the queue dead-letters it (marks it
	// permanently failed) and returns it with deadLettered=true. The caller must
	// NOT run such a job, only settle its side effects to failed — mirroring the
	// attempt-exhaustion path where Nack reports dead=true.
	Dequeue(ctx context.Context, kinds ...model.JobKind) (job *model.Job, deadLettered bool, err error)
	// Ack marks an in-flight job as successfully completed. The job carries the
	// claim's fencing token (see Dequeue): a job's lease can expire and be
	// reclaimed by another worker while the original attempt is still running, so
	// Ack settles only while this attempt still holds the claim. A stale Ack whose
	// job was reclaimed is a no-op (nil error), leaving the reclaiming attempt in
	// charge.
	Ack(ctx context.Context, job *model.Job) error
	// Nack reports that processing failed. The queue reschedules the job with
	// backoff, or marks it permanently dead once attempts are exhausted.
	// dead reports whether the job is now permanently failed. Like Ack, Nack is
	// fenced by the job's claim token: a stale Nack whose job was reclaimed is a
	// no-op returning dead=false, so it neither reschedules nor fails the
	// reclaiming attempt's job.
	Nack(ctx context.Context, job *model.Job, cause error) (dead bool, err error)
}
