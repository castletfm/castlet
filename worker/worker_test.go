package worker_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/store"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/castletfm/castlet/transcribe"
	"github.com/castletfm/castlet/transcribe/null"
	"github.com/castletfm/castlet/worker"
	"github.com/stretchr/testify/require"
)

// fakeTranscriber returns a fixed two-segment transcript.
type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	return &transcribe.Result{Language: "en", Segments: []transcribe.Segment{
		{StartSecs: 0, EndSecs: 1, Text: "hello"},
		{StartSecs: 1, EndSecs: 2, Text: "world"},
	}}, nil
}

// deadlineTranscriber records whether the context it is handed carries a
// deadline, so a test can assert the worker applies a per-job timeout.
type deadlineTranscriber struct {
	hasDeadline chan bool
}

func (d deadlineTranscriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	_, ok := ctx.Deadline()
	d.hasDeadline <- ok
	return &transcribe.Result{Language: "en"}, nil
}

// editingTranscriber simulates an admin editing the episode's title while the
// (slow) transcription is in flight: it performs a full-row UpdateEpisode from a
// freshly loaded copy, exactly as an admin edit handler would, then returns a
// transcript. The worker holds a stale copy from job start, so if it wrote the
// whole row back the edit would be lost.
type editingTranscriber struct {
	st *sqlite.Store
	id string
}

func (e editingTranscriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	cur, err := e.st.EpisodeByID(ctx, e.id)
	if err != nil {
		return nil, err
	}
	cur.Title = "Edited Mid-Job"
	cur.UpdatedAt = time.Now()
	if err := e.st.UpdateEpisode(ctx, cur); err != nil {
		return nil, err
	}
	return &transcribe.Result{Language: "en", Segments: []transcribe.Segment{
		{StartSecs: 0, EndSecs: 1, Text: "hello"},
	}}, nil
}

func setup(t *testing.T) (*sqlite.Store, *localfs.Store, *dbqueue.Queue) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "w.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(t.Context()))
	t.Cleanup(func() { st.Close() })
	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	return st, blobs, dbqueue.New(st)
}

func seedEpisode(t *testing.T, st *sqlite.Store, blobs *localfs.Store, q *dbqueue.Queue) string {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "u@x.y", DisplayName: "U", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err := blobs.Put(ctx, "mk1", strings.NewReader("fake audio bytes"))
	require.NoError(t, err)
	require.NoError(t, st.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		MediaKey: "mk1", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptPending,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, q.Enqueue(ctx, model.JobTranscribe, model.TranscribePayload{EpisodeID: "e1"}))
	return "e1"
}

// seedClaimedJob inserts a job row that stands in for a fake queue's in-flight
// job, so the worker's claim-fenced episode settlement (SettleEpisodeTranscript)
// recognizes this attempt as the active claim. Its Attempts — the fencing token —
// matches the job the fake queue hands the worker, and it is left "processing" as
// a live claim would be. Returns the job to serve from the fake queue.
func seedClaimedJob(t *testing.T, st *sqlite.Store, id, episodeID string) *model.Job {
	t.Helper()
	now := time.Now()
	j := &model.Job{ID: id, Kind: model.JobTranscribe,
		Payload: `{"episode_id":"` + episodeID + `"}`,
		Status:  model.JobProcessing, Attempts: 1,
		RunAfter: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, st.EnqueueJob(t.Context(), j))
	return j
}

// waitStatus polls until the episode reaches want or the deadline passes.
func waitStatus(t *testing.T, st *sqlite.Store, id string, want model.TranscriptStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ep, err := st.EpisodeByID(t.Context(), id)
		require.NoError(t, err)
		if ep.TranscriptStatus == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("episode %s did not reach status %q in time", id, want)
}

func TestWorkerTranscribes(t *testing.T) {
	st, blobs, q := setup(t)
	id := seedEpisode(t, st, blobs, q)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctrl, err := worker.New(st, blobs, q, fakeTranscriber{}, worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	waitStatus(t, st, id, model.TranscriptDone)

	tr, err := st.TranscriptByEpisode(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, tr.Segments, 2)
	require.Equal(t, "world", tr.Segments[1].Text)

	cancel()
	require.NoError(t, ctrl.Wait())
}

// TestWorkerJobHasTimeout asserts the worker hands the transcriber a context
// with a deadline, so a hung command is eventually killed instead of blocking
// the serial worker loop forever.
func TestWorkerJobHasTimeout(t *testing.T) {
	st, blobs, q := setup(t)
	seedEpisode(t, st, blobs, q)

	tr := deadlineTranscriber{hasDeadline: make(chan bool, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := worker.New(st, blobs, q, tr, worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	select {
	case ok := <-tr.hasDeadline:
		require.True(t, ok, "worker must pass the transcriber a context with a deadline")
	case <-time.After(3 * time.Second):
		t.Fatal("transcriber was not invoked in time")
	}
}

// TestWorkerDoesNotClobberConcurrentEdit proves that an admin edit to the
// episode made while transcription is running is preserved: the worker's
// transcript-status write must be a targeted UPDATE, not a full-row overwrite
// from its stale copy.
func TestWorkerDoesNotClobberConcurrentEdit(t *testing.T) {
	st, blobs, q := setup(t)
	id := seedEpisode(t, st, blobs, q)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tr := editingTranscriber{st: st, id: id}
	_, err := worker.New(st, blobs, q, tr, worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	waitStatus(t, st, id, model.TranscriptDone)

	ep, err := st.EpisodeByID(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "Edited Mid-Job", ep.Title,
		"admin edit made during transcription must not be reverted by the worker")
	require.Equal(t, model.TranscriptDone, ep.TranscriptStatus)
}

// TestWorkerReclaimDeadLetterFailsEpisode proves that a job reclaimed after it
// has already exhausted its attempts (a worker that kept crashing before it
// could Nack) settles BOTH the queue job and the episode transcript to failed.
// The queue dead-letters the job at dequeue time; the worker must still mark the
// episode failed rather than leaving it stuck "processing" forever, matching the
// normal Nack-exhaustion path.
func TestWorkerReclaimDeadLetterFailsEpisode(t *testing.T) {
	st, blobs, _ := setup(t)
	ctx := t.Context()

	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "u@x.y", DisplayName: "U", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err := blobs.Put(ctx, "mk1", strings.NewReader("fake audio bytes"))
	require.NoError(t, err)
	require.NoError(t, st.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		MediaKey: "mk1", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptProcessing,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	// A job left "processing" by a crashed worker that already used up its
	// attempts, with an expired lease so it is reclaimable. The reclaim bumps
	// attempts past maxAttempts, so the queue dead-letters it.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, st.EnqueueJob(ctx, &model.Job{ID: "poison", Kind: model.JobTranscribe,
		Payload: `{"episode_id":"e1"}`, Status: model.JobProcessing, Attempts: 2,
		RunAfter: past, CreatedAt: past, UpdatedAt: past}))

	q := dbqueue.New(st, dbqueue.WithMaxAttempts(2))
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// fakeTranscriber would drive the episode to Done if the job were (wrongly)
	// run, so a Failed status also proves the job was not re-run.
	_, err = worker.New(st, blobs, q, fakeTranscriber{}, worker.WithPollInterval(10*time.Millisecond)).Run(wctx)
	require.NoError(t, err)

	waitStatus(t, st, "e1", model.TranscriptFailed)

	got, err := st.JobByID(t.Context(), "poison")
	require.NoError(t, err)
	require.Equal(t, model.JobFailed, got.Status, "dead-lettered job must be permanently failed")

	_, err = st.TranscriptByEpisode(t.Context(), "e1")
	require.Error(t, err, "no transcript should be saved for a dead-lettered job")
}

// cancelingTranscriber simulates a shutdown (SIGTERM) arriving mid-job: it
// cancels the worker's context and then fails, so the terminal Nack runs with
// an already-cancelled request context.
type cancelingTranscriber struct {
	cancel context.CancelFunc
}

func (c cancelingTranscriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	c.cancel()
	return nil, errors.New("boom")
}

// nackRecordingQueue serves a single job and records the context error observed
// at the moment Nack is called, so a test can assert the terminal bookkeeping
// write is not made with a cancelled context.
type nackRecordingQueue struct {
	job        *model.Job
	dequeued   bool
	nackCtxErr error
	nacked     chan struct{}
}

func (q *nackRecordingQueue) Enqueue(context.Context, model.JobKind, any) error { return nil }

func (q *nackRecordingQueue) Dequeue(context.Context, ...model.JobKind) (*model.Job, bool, error) {
	if q.dequeued {
		return nil, false, nil
	}
	q.dequeued = true
	return q.job, false, nil
}

func (q *nackRecordingQueue) Ack(context.Context, *model.Job) error { return nil }

func (q *nackRecordingQueue) Nack(ctx context.Context, _ *model.Job, _ error) (bool, error) {
	q.nackCtxErr = ctx.Err()
	close(q.nacked)
	return false, nil
}

// TestWorkerNackSurvivesShutdown asserts that when the worker's context is
// cancelled while a job is being processed, the terminal Nack is still made with
// a live context, so the job's final state change is persisted rather than lost
// to context.Canceled (which would strand the job in "processing").
func TestWorkerNackSurvivesShutdown(t *testing.T) {
	st, blobs, _ := setup(t)
	seedEpisode(t, st, blobs, dbqueue.New(st))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	q := &nackRecordingQueue{
		job:    &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: `{"episode_id":"e1"}`},
		nacked: make(chan struct{}),
	}
	_, err := worker.New(st, blobs, q, cancelingTranscriber{cancel: cancel},
		worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	select {
	case <-q.nacked:
	case <-time.After(3 * time.Second):
		t.Fatal("Nack was not called")
	}
	require.NoError(t, q.nackCtxErr, "terminal Nack must run with a context that survives shutdown")
}

// cancelBeforeSettleTranscriber simulates shutdown (SIGTERM) arriving between
// the transcription work and the terminal status persistence: it cancels the
// worker's context and then returns a successful result, so the final status
// write and Ack run with an already-cancelled request context.
type cancelBeforeSettleTranscriber struct {
	cancel context.CancelFunc
}

func (c cancelBeforeSettleTranscriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	c.cancel()
	return &transcribe.Result{Language: "en", Segments: []transcribe.Segment{
		{StartSecs: 0, EndSecs: 1, Text: "hello"},
	}}, nil
}

// ackRecordingQueue serves a single job and records whether it was terminated
// via Ack or Nack, so a test can assert the success path acks the job only
// after its terminal state persists.
type ackRecordingQueue struct {
	job      *model.Job
	dequeued bool
	acked    chan struct{}
	nacked   chan struct{}
}

func (q *ackRecordingQueue) Enqueue(context.Context, model.JobKind, any) error { return nil }

func (q *ackRecordingQueue) Dequeue(context.Context, ...model.JobKind) (*model.Job, bool, error) {
	if q.dequeued {
		return nil, false, nil
	}
	q.dequeued = true
	return q.job, false, nil
}

func (q *ackRecordingQueue) Ack(context.Context, *model.Job) error { close(q.acked); return nil }

func (q *ackRecordingQueue) Nack(context.Context, *model.Job, error) (bool, error) {
	close(q.nacked)
	return false, nil
}

// TestWorkerAckSurvivesShutdownSuccess asserts that when the worker's context is
// cancelled after transcription succeeds but before the terminal status is
// persisted, the episode is still durably settled to "done" AND the job is
// acked (never left terminal-done with the episode stuck processing).
func TestWorkerAckSurvivesShutdownSuccess(t *testing.T) {
	st, blobs, _ := setup(t)
	seedEpisode(t, st, blobs, dbqueue.New(st))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	q := &ackRecordingQueue{
		job:    seedClaimedJob(t, st, "j1", "e1"),
		acked:  make(chan struct{}),
		nacked: make(chan struct{}),
	}
	_, err := worker.New(st, blobs, q, cancelBeforeSettleTranscriber{cancel: cancel},
		worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	select {
	case <-q.acked:
	case <-q.nacked:
		t.Fatal("job was nacked; a successful transcript must persist and ack under a shutdown-surviving context")
	case <-time.After(3 * time.Second):
		t.Fatal("job was neither acked nor nacked")
	}

	ep, err := st.EpisodeByID(t.Context(), "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptDone, ep.TranscriptStatus,
		"episode status must be durably settled to done even though the worker ctx was cancelled")

	tr, err := st.TranscriptByEpisode(t.Context(), "e1")
	require.NoError(t, err)
	require.Len(t, tr.Segments, 1)
}

// budgetExhaustingStore blocks the terminal fenced settlement (the one that
// carries a transcript) until the settlement context it is handed expires,
// simulating a settlement step that consumes its entire budget. The non-terminal
// progress marker (no transcript) and every other call delegate to the embedded
// store.
type budgetExhaustingStore struct {
	store.Store
}

func (s budgetExhaustingStore) SettleEpisodeTranscript(ctx context.Context, jobID string, token int, episodeID string, tr *model.Transcript, status model.TranscriptStatus, at time.Time) error {
	if tr == nil {
		return s.Store.SettleEpisodeTranscript(ctx, jobID, token, episodeID, tr, status, at)
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestWorkerNackSurvivesSettlementBudgetExhaustion asserts that when the
// settlement writes consume their whole timeout, the terminal Nack still runs
// under a fresh, live context (not the exhausted settlement context), so the
// claimed job is durably Nack'd for retry rather than stranded in "processing".
func TestWorkerNackSurvivesSettlementBudgetExhaustion(t *testing.T) {
	st, blobs, _ := setup(t)
	seedEpisode(t, st, blobs, dbqueue.New(st))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	q := &nackRecordingQueue{
		job:    &model.Job{ID: "j1", Kind: model.JobTranscribe, Payload: `{"episode_id":"e1"}`},
		nacked: make(chan struct{}),
	}
	// A tiny settle budget plus a store that blocks SaveTranscript until that
	// budget is spent means settlement always times out; the queue transition
	// must not inherit the exhausted context.
	bstore := budgetExhaustingStore{Store: st}
	_, err := worker.New(bstore, blobs, q, fakeTranscriber{},
		worker.WithPollInterval(10*time.Millisecond),
		worker.WithSettleTimeout(20*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	select {
	case <-q.nacked:
	case <-time.After(3 * time.Second):
		t.Fatal("Nack was not called after settlement exhausted its budget")
	}
	require.NoError(t, q.nackCtxErr,
		"terminal Nack must run under a fresh live context, not the exhausted settlement context")
}

// failingTranscriber always fails, so the worker takes the fail/Nack path.
type failingTranscriber struct{}

func (failingTranscriber) Transcribe(context.Context, transcribe.Input) (*transcribe.Result, error) {
	return nil, errors.New("boom")
}

// deadNackQueue serves a single job and, on Nack, consumes the whole context it
// is handed before reporting the job permanently dead, simulating a Nack that
// spends its entire bounded budget. This lets a test assert the subsequent
// dead-job status settlement runs under its OWN fresh context rather than the
// exhausted Nack context.
type deadNackQueue struct {
	job      *model.Job
	dequeued bool
	nacked   chan struct{}
}

func (q *deadNackQueue) Enqueue(context.Context, model.JobKind, any) error { return nil }

func (q *deadNackQueue) Dequeue(context.Context, ...model.JobKind) (*model.Job, bool, error) {
	if q.dequeued {
		return nil, false, nil
	}
	q.dequeued = true
	return q.job, false, nil
}

func (q *deadNackQueue) Ack(context.Context, *model.Job) error { return nil }

func (q *deadNackQueue) Nack(ctx context.Context, _ *model.Job, _ error) (bool, error) {
	<-ctx.Done() // spend the entire Nack budget, leaving ctx expired
	close(q.nacked)
	return true, nil
}

// deadStatusRecordingStore records the context error observed when the dead-job
// TranscriptFailed status is written, so a test can assert that settlement runs
// under a live context rather than the exhausted Nack context.
type deadStatusRecordingStore struct {
	store.Store
	statusCtxErr error
	recorded     chan struct{}
}

func (s *deadStatusRecordingStore) SettleEpisodeTranscript(ctx context.Context, jobID string, token int, episodeID string, tr *model.Transcript, status model.TranscriptStatus, at time.Time) error {
	err := s.Store.SettleEpisodeTranscript(ctx, jobID, token, episodeID, tr, status, at)
	if status == model.TranscriptFailed {
		s.statusCtxErr = ctx.Err()
		close(s.recorded)
	}
	return err
}

// TestWorkerDeadJobStatusSurvivesNackBudgetExhaustion asserts that when Nack
// reports a job permanently dead after consuming its whole context budget, the
// terminal TranscriptFailed status write still runs under its OWN fresh live
// context, so the episode is durably settled to "failed" rather than stranded in
// "processing" on an already-expired context.
func TestWorkerDeadJobStatusSurvivesNackBudgetExhaustion(t *testing.T) {
	base, blobs, _ := setup(t)
	seedEpisode(t, base, blobs, dbqueue.New(base))

	st := &deadStatusRecordingStore{Store: base, recorded: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	q := &deadNackQueue{
		job:    seedClaimedJob(t, base, "j1", "e1"),
		nacked: make(chan struct{}),
	}
	// A tiny settle budget means Nack draining its context to expiry is quick;
	// the dead-job status write must not inherit that exhausted context.
	_, err := worker.New(st, blobs, q, failingTranscriber{},
		worker.WithPollInterval(10*time.Millisecond),
		worker.WithSettleTimeout(20*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	select {
	case <-q.nacked:
	case <-time.After(3 * time.Second):
		t.Fatal("Nack was not called")
	}

	select {
	case <-st.recorded:
	case <-time.After(3 * time.Second):
		t.Fatal("dead-job TranscriptFailed status was never written; it likely ran under the exhausted Nack context")
	}
	require.NoError(t, st.statusCtxErr,
		"dead-job status settlement must run under a fresh live context, not the exhausted Nack context")

	ep, err := base.EpisodeByID(t.Context(), "e1")
	require.NoError(t, err)
	require.Equal(t, model.TranscriptFailed, ep.TranscriptStatus,
		"a dead job must settle the episode to failed, not leave it stuck processing")
}

func TestWorkerNullSettlesToNone(t *testing.T) {
	st, blobs, q := setup(t)
	id := seedEpisode(t, st, blobs, q)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := worker.New(st, blobs, q, null.New(), worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	// null transcriber reports unsupported -> transcript status becomes "none"
	waitStatus(t, st, id, model.TranscriptNone)

	_, err = st.TranscriptByEpisode(t.Context(), id)
	require.Error(t, err, "no transcript should be saved")
}
