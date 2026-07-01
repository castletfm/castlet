// Package dbqueue is the default queue.JobQueue implementation. It persists
// jobs in the metadata Store, so a standalone install needs no broker. The
// queue owns the retry policy: a failed job is rescheduled with backoff until
// its attempts are exhausted, after which it is marked permanently dead.
package dbqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue"
	"github.com/castletfm/castlet/store"
	"github.com/lestrrat-go/option/v3"
)

// Queue is a Store-backed JobQueue.
type Queue struct {
	store       store.Store
	maxAttempts int
	lease       time.Duration
	backoff     func(attempt int) time.Duration
}

var _ queue.JobQueue = (*Queue)(nil)

// Option configures New.
type Option = option.Interface

type (
	identMaxAttempts struct{}
	identLease       struct{}
	identBackoff     struct{}
)

// WithMaxAttempts sets how many times a job may be attempted before it is
// marked dead (default 5).
func WithMaxAttempts(n int) Option { return option.New(identMaxAttempts{}, n) }

// WithLease sets how long a claimed job stays invisible before another worker
// may reclaim it, bounding the damage from a crashed worker (default 10m).
func WithLease(d time.Duration) Option { return option.New(identLease{}, d) }

// WithBackoff overrides the retry backoff function. attempt is the number of
// attempts already made (>= 1). Default is exponential: 2^attempt minutes
// capped at 1h.
func WithBackoff(fn func(attempt int) time.Duration) Option { return option.New(identBackoff{}, fn) }

// New returns a Queue backed by s.
func New(s store.Store, options ...Option) *Queue {
	q := &Queue{
		store:       s,
		maxAttempts: 5,
		lease:       10 * time.Minute,
		backoff:     defaultBackoff,
	}
	for _, o := range options {
		switch o.Ident().(type) {
		case identMaxAttempts:
			q.maxAttempts = option.MustGet[int](o)
		case identLease:
			q.lease = option.MustGet[time.Duration](o)
		case identBackoff:
			q.backoff = option.MustGet[func(int) time.Duration](o)
		}
	}
	return q
}

func defaultBackoff(attempt int) time.Duration {
	d := time.Minute << attempt // 2^attempt minutes
	if limit := time.Hour; d > limit {
		return limit
	}
	return d
}

func (q *Queue) Enqueue(ctx context.Context, kind model.JobKind, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("dbqueue: marshal payload: %w", err)
	}
	now := time.Now()
	return q.store.EnqueueJob(ctx, &model.Job{
		ID:        idgen.New(),
		Kind:      kind,
		Payload:   string(data),
		Status:    model.JobPending,
		RunAfter:  now,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

func (q *Queue) Dequeue(ctx context.Context, kinds ...model.JobKind) (*model.Job, bool, error) {
	j, err := q.store.ClaimJob(ctx, kinds, time.Now(), q.lease)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("dbqueue: dequeue: %w", err)
	}
	// A job reclaimed from a crashed worker has its attempts bumped by the
	// claim but is never Nack'd, so enforce max-attempts here too: a poison job
	// that keeps crashing must be dead-lettered rather than reclaimed and re-run
	// forever. attempts <= maxAttempts still runs (matching Nack's >= maxAttempts
	// dead-letter accounting). The dead-lettered job is surfaced
	// (deadLettered=true) so the caller can settle its side effects to failed,
	// exactly as it does when Nack reports the job dead.
	if j.Attempts > q.maxAttempts {
		if err := q.store.FailJob(ctx, j.ID, "dbqueue: exceeded max attempts"); err != nil {
			return nil, false, fmt.Errorf("dbqueue: dead-letter poison job: %w", err)
		}
		return j, true, nil
	}
	return j, false, nil
}

func (q *Queue) Ack(ctx context.Context, jobID string) error {
	return q.store.CompleteJob(ctx, jobID)
}

func (q *Queue) Nack(ctx context.Context, jobID string, cause error) (bool, error) {
	j, err := q.store.JobByID(ctx, jobID)
	if err != nil {
		return false, fmt.Errorf("dbqueue: nack lookup: %w", err)
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	// Attempts was already incremented when the job was claimed, so it is the
	// number of tries spent so far.
	if j.Attempts >= q.maxAttempts {
		return true, q.store.FailJob(ctx, jobID, msg)
	}
	runAfter := time.Now().Add(q.backoff(j.Attempts))
	return false, q.store.RescheduleJob(ctx, jobID, runAfter, msg)
}
