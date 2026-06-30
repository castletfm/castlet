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

	job, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, model.JobTranscribe, job.Kind)
	require.JSONEq(t, `{"episode_id":"e1"}`, job.Payload)

	// nothing left to dequeue (job is leased)
	next, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Nil(t, next)

	require.NoError(t, q.Ack(ctx, job.ID))
}

func TestNackRetriesThenDies(t *testing.T) {
	q, _ := newQueue(t, dbqueue.WithMaxAttempts(2),
		dbqueue.WithBackoff(func(int) time.Duration { return 0 }))
	ctx := t.Context()
	require.NoError(t, q.Enqueue(ctx, model.JobTranscribe, struct{}{}))

	// attempt 1
	job, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Equal(t, 1, job.Attempts)
	dead, err := q.Nack(ctx, job.ID, errors.New("boom"))
	require.NoError(t, err)
	require.False(t, dead)

	// attempt 2 (backoff 0 -> immediately runnable again)
	job, err = q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Equal(t, 2, job.Attempts)
	dead, err = q.Nack(ctx, job.ID, errors.New("boom again"))
	require.NoError(t, err)
	require.True(t, dead, "should be dead after maxAttempts")

	// now permanently failed: not runnable
	next, err := q.Dequeue(ctx, model.JobTranscribe)
	require.NoError(t, err)
	require.Nil(t, next)
}
