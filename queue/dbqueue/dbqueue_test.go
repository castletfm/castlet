package dbqueue_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
)

func newQueue(t *testing.T, opts ...dbqueue.Option) (*dbqueue.Queue, *sqlite.Store) {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "q.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(t.Context()))
	t.Cleanup(func() { s.Close() })
	return dbqueue.New(s, opts...), s
}

func TestEnqueueDequeueAck(t *testing.T) {
	q, _ := newQueue(t)
	ctx := t.Context()

	require.NoError(t, q.Enqueue(ctx, model.JobTranscribe, model.TranscribePayload{EpisodeID: "e1"}))

	job, dead, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.False(t, dead)
	require.NotNil(t, job)
	require.Equal(t, model.JobTranscribe, job.Kind)
	require.JSONEq(t, `{"episode_id":"e1"}`, job.Payload)

	// nothing left to dequeue (job is leased)
	next, _, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Nil(t, next)

	require.NoError(t, q.Ack(ctx, job))
}

func TestNackRetriesThenDies(t *testing.T) {
	q, _ := newQueue(t, dbqueue.WithMaxAttempts(2),
		dbqueue.WithBackoff(func(int) time.Duration { return 0 }))
	ctx := t.Context()
	require.NoError(t, q.Enqueue(ctx, model.JobTranscribe, struct{}{}))

	// attempt 1
	job, _, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Equal(t, 1, job.Attempts)
	dead, err := q.Nack(ctx, job, errors.New("boom"))
	require.NoError(t, err)
	require.False(t, dead)

	// attempt 2 (backoff 0 -> immediately runnable again)
	job, _, err = q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Equal(t, 2, job.Attempts)
	dead, err = q.Nack(ctx, job, errors.New("boom again"))
	require.NoError(t, err)
	require.True(t, dead, "should be dead after maxAttempts")

	// now permanently failed: not runnable
	next, _, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Nil(t, next)
}

// TestStaleSettlementNoOpAfterReclaim proves the queue fences settlement by the
// claim token: a long job whose lease expires can be reclaimed by a second
// worker while the first is still running, and the first worker's Ack/Nack must
// then be no-ops so it cannot clobber the reclaiming attempt.
func TestStaleSettlementNoOpAfterReclaim(t *testing.T) {
	q, s := newQueue(t, dbqueue.WithMaxAttempts(5))
	ctx := t.Context()
	require.NoError(t, q.Enqueue(ctx, model.JobTranscribe, model.TranscribePayload{EpisodeID: "e1"}))

	// First worker claims the job (token = Attempts = 1).
	first, _, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Equal(t, 1, first.Attempts)

	// Its lease expires; a second worker reclaims it via the store with a "now"
	// past the lease, advancing the token to 2.
	second, err := s.ClaimJob(ctx, []model.JobKind{model.JobTranscribe}, time.Now().Add(time.Hour), time.Minute)
	require.NoError(t, err)
	require.Equal(t, 2, second.Attempts)

	// Stale Ack and Nack from the first attempt are no-ops: no error, and the job
	// remains owned (processing, token 2) by the reclaiming attempt.
	require.NoError(t, q.Ack(ctx, first))
	dead, err := q.Nack(ctx, first, errors.New("stale failure"))
	require.NoError(t, err)
	require.False(t, dead, "a stale Nack must not report the reclaimed job dead")

	got, err := s.JobByID(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, model.JobProcessing, got.Status, "stale settlement must not change the reclaimed job")
	require.Equal(t, 2, got.Attempts)

	// The reclaiming attempt settles normally.
	require.NoError(t, q.Ack(ctx, second))
	got, err = s.JobByID(ctx, first.ID)
	require.NoError(t, err)
	require.Equal(t, model.JobDone, got.Status)
}

func TestDequeueDeadLettersPoisonJob(t *testing.T) {
	q, s := newQueue(t, dbqueue.WithMaxAttempts(2))
	ctx := t.Context()

	// A job left processing by a crashed worker that already used up its attempts,
	// with an expired lease so it is reclaimable. Because a crash never Nack's,
	// max-attempts is enforced on the reclaim in Dequeue instead: this must be
	// dead-lettered, not handed back for yet another run.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, s.EnqueueJob(ctx, &model.Job{ID: "poison", Kind: model.JobTranscribe, Payload: "{}",
		Status: model.JobProcessing, Attempts: 2, RunAfter: past, CreatedAt: past, UpdatedAt: past}))

	job, dead, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.True(t, dead, "poison job past max attempts must be dead-lettered, not run")
	require.NotNil(t, job, "the dead-lettered job is surfaced so the caller can settle its side effects")
	require.Equal(t, "poison", job.ID)

	got, err := s.JobByID(ctx, "poison")
	require.NoError(t, err)
	require.Equal(t, model.JobFailed, got.Status)
	require.Equal(t, 3, got.Attempts)
}
