// Package worker runs Castlet's asynchronous transcription. It follows the
// house Run/Controller lifecycle: Run binds nothing but spawns a polling
// goroutine that pulls transcribe jobs from the queue and drives each episode
// through its transcript lifecycle. Cancelling the context passed to Run is the
// only way to stop it.
//
// By design there is no recover() around job dispatch: a panicking transcriber
// crashes the process so the operator's restart/alerting policy applies.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue"
	"github.com/castletfm/castlet/store"
	"github.com/castletfm/castlet/transcribe"
	"github.com/lestrrat-go/option/v3"
)

// ErrWorkerClosed is recorded on the Controller when the worker stops because
// its context was cancelled, distinguishing a clean shutdown from a crash.
var ErrWorkerClosed = errors.New("worker: closed")

// Defaults for the per-job timeout policy. They are deliberately generous so
// the out-of-the-box behavior does not kill legitimately slow whisper.cpp runs.
const (
	defaultTimeoutFactor = 1.5             // multiply audio length by this
	defaultTimeoutMin    = 5 * time.Minute // floor: cover model spin-up on short clips
	defaultTimeoutMax    = 2 * time.Hour   // cap; also the fallback when duration is unknown
)

// defaultSettleTimeout bounds each shutdown-surviving bookkeeping step. The
// settlement writes and the queue state transition each get their OWN budget so
// that even a settlement that consumes its full timeout cannot leave the
// subsequent Nack/Ack running on an already-expired context.
const defaultSettleTimeout = 10 * time.Second

// JobTimeoutPolicy derives the per-job transcription timeout from the episode's
// audio length. A single fixed timeout permanently fails long episodes, so the
// bound scales with the input: timeout = Factor * durationSecs, clamped to
// [Min, Max]. Bounding the run matters because the queue lease only governs
// *reclaim*; it does not kill a running command, so a hung transcriber would
// otherwise block the serial worker loop forever.
type JobTimeoutPolicy struct {
	Factor float64       // multiplier applied to the audio length
	Min    time.Duration // floor, to cover model spin-up on short clips
	Max    time.Duration // cap, and the fallback when duration is unknown
}

// withDefaults fills any zero/negative field with its default so callers (and
// the app wiring) can set only the fields they care about.
func (p JobTimeoutPolicy) withDefaults() JobTimeoutPolicy {
	if p.Factor <= 0 {
		p.Factor = defaultTimeoutFactor
	}
	if p.Min <= 0 {
		p.Min = defaultTimeoutMin
	}
	if p.Max <= 0 {
		p.Max = defaultTimeoutMax
	}
	return p
}

// timeout returns the per-job timeout for an episode of durationSecs seconds.
//
// NOTE: this relies on Episode.DurationSecs being populated at upload time.
// That is not always the case today (it is often 0), so when the duration is
// unknown (<= 0) we fall back to Max — a generous cap — rather than a short
// bound, so unknown-length jobs are not killed prematurely.
func (p JobTimeoutPolicy) timeout(durationSecs int) time.Duration {
	if durationSecs <= 0 {
		return p.Max
	}
	d := time.Duration(p.Factor * float64(durationSecs) * float64(time.Second))
	// Apply both clamps in order so Max is always the hard ceiling. When
	// Min > Max the floor is raised first, then knocked back down to Max, so
	// the configured cap still wins.
	d = max(d, p.Min)
	d = min(d, p.Max)
	return d
}

// Worker transcribes episodes off the job queue. The receiver holds only
// configuration and is safe to Run multiple times.
type Worker struct {
	store         store.Store
	blobs         blob.BlobStore
	queue         queue.JobQueue
	transcriber   transcribe.Transcriber
	pollInterval  time.Duration
	jobTimeout    JobTimeoutPolicy
	settleTimeout time.Duration
	logger        *slog.Logger
}

// Option configures New.
type Option = option.Interface

type (
	identPollInterval  struct{}
	identJobTimeout    struct{}
	identSettleTimeout struct{}
	identLogger        struct{}
)

// WithPollInterval sets how often the worker polls when the queue is empty
// (default 5s). When a job is found the worker drains the queue without
// waiting for the next tick.
func WithPollInterval(d time.Duration) Option { return option.New(identPollInterval{}, d) }

// WithJobTimeout sets the per-job timeout policy (see JobTimeoutPolicy). Any
// zero field falls back to its default (factor 1.5, min 5m, max 2h).
func WithJobTimeout(p JobTimeoutPolicy) Option { return option.New(identJobTimeout{}, p) }

// WithSettleTimeout sets the per-step budget for the shutdown-surviving terminal
// bookkeeping (settlement writes and the Ack/Nack queue transition), default
// 10s. A non-positive value restores the default.
func WithSettleTimeout(d time.Duration) Option { return option.New(identSettleTimeout{}, d) }

// WithLogger sets the structured logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return option.New(identLogger{}, l) }

// New constructs a Worker from its dependencies.
func New(st store.Store, blobs blob.BlobStore, q queue.JobQueue, tr transcribe.Transcriber, options ...Option) *Worker {
	w := &Worker{
		store:         st,
		blobs:         blobs,
		queue:         q,
		transcriber:   tr,
		pollInterval:  5 * time.Second,
		settleTimeout: defaultSettleTimeout,
		logger:        slog.Default(),
	}
	for _, o := range options {
		switch o.Ident().(type) {
		case identPollInterval:
			w.pollInterval = option.MustGet[time.Duration](o)
		case identJobTimeout:
			w.jobTimeout = option.MustGet[JobTimeoutPolicy](o)
		case identSettleTimeout:
			if d := option.MustGet[time.Duration](o); d > 0 {
				w.settleTimeout = d
			}
		case identLogger:
			w.logger = option.MustGet[*slog.Logger](o)
		}
	}
	w.jobTimeout = w.jobTimeout.withDefaults()
	return w
}

// Controller is the handle to a running Worker.
type Controller struct {
	done chan struct{}
	err  atomic.Pointer[error]
}

// Done is closed when the worker goroutine has fully exited.
func (c *Controller) Done() <-chan struct{} { return c.done }

// Err returns the terminal error, or nil for a clean context-driven exit.
func (c *Controller) Err() error {
	if p := c.err.Load(); p != nil && !errors.Is(*p, ErrWorkerClosed) {
		return *p
	}
	return nil
}

// Wait blocks until the worker exits and returns Err.
func (c *Controller) Wait() error { <-c.done; return c.Err() }

// Run starts the worker loop and returns immediately.
func (w *Worker) Run(ctx context.Context) (*Controller, error) {
	ctrl := &Controller{done: make(chan struct{})}
	go func() {
		defer close(ctrl.done)
		err := w.loop(ctx)
		ctrl.err.Store(&err)
	}()
	return ctrl, nil
}

func (w *Worker) loop(ctx context.Context) error {
	t := time.NewTicker(w.pollInterval)
	defer t.Stop()
	for {
		// Drain everything currently runnable before sleeping. Jobs are handled
		// strictly one-at-a-time (no concurrency): local shell-exec transcription
		// is intentionally serialized to avoid overloading the host.
		for {
			if ctx.Err() != nil {
				return ErrWorkerClosed
			}
			processed, err := w.processOne(ctx)
			if err != nil {
				w.logger.Error("transcription job failed", "error", err)
			}
			if !processed {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ErrWorkerClosed
		case <-t.C:
		}
	}
}

// settlement is the durable terminal outcome a job produced. It is applied by
// settle() under the shutdown-surviving context after the (cancellable) work
// has finished, so the episode's final state is recorded even on SIGTERM.
type settlement struct {
	ep         *model.Episode         // episode to update
	transcript *model.Transcript      // saved before the status write when non-nil
	status     model.TranscriptStatus // terminal transcript status to persist
}

// processOne claims and handles a single job. It reports whether a job was
// processed (so the caller can keep draining) and any handling error.
func (w *Worker) processOne(ctx context.Context) (bool, error) {
	job, deadLettered, err := w.queue.Dequeue(ctx, model.JobTranscribe)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	if deadLettered {
		// The queue reclaimed a job past its attempt limit and permanently
		// failed it instead of handing it back to run. Settle the episode
		// transcript to failed too, matching the Nack-exhaustion path, so it is
		// not left stuck "processing" forever. Do not run it. The mark runs under
		// its own fresh shutdown-surviving budget, fenced by the fresh claim
		// token, like every other settlement.
		markCtx, cancel := context.WithTimeout(context.Background(), w.settleTimeout)
		defer cancel()
		w.markTranscript(markCtx, job, model.TranscriptFailed)
		return true, nil
	}
	s, herr := w.handle(ctx, job)

	// The terminal bookkeeping (transcript save, status write, and Ack/Nack)
	// must survive shutdown. handle() runs the transcription under ctx (so the
	// work itself stops on SIGTERM), but settling with that same, now-cancelled
	// ctx would fail with context.Canceled: the job would be acked "done" while
	// the episode stayed stuck mid-transcription. Each terminal step below runs
	// under its OWN fresh background-derived timeout, so the queue state
	// transition (Nack/Ack) is always durable even if the settlement writes
	// consumed their whole budget.
	if herr != nil {
		return true, w.fail(job, herr)
	}
	// Persist the terminal outcome durably BEFORE acking. If the settlement
	// write is lost, do NOT ack the job as success: Nack it so it is retried
	// rather than left terminal-done with the episode stuck processing.
	if s != nil {
		ctx2, cancel := context.WithTimeout(context.Background(), w.settleTimeout)
		err := w.settle(ctx2, job, s)
		cancel()
		if err != nil {
			return true, w.fail(job, err)
		}
	}
	return true, w.ack(job)
}

// ack marks a job done under a fresh shutdown-surviving context, independent of
// whatever the settlement step consumed, so a successful job is never left in
// "processing" because the ack ran on an already-expired context.
func (w *Worker) ack(job *model.Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), w.settleTimeout)
	defer cancel()
	return w.queue.Ack(ctx, job)
}

// fail Nacks a job and, if the queue reports it permanently dead, records the
// terminal failed status. It derives its OWN fresh bounded context (not the
// settlement context, which may already be exhausted) so the queue state
// transition always runs live. cause is returned so the loop can log it.
func (w *Worker) fail(job *model.Job, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), w.settleTimeout)
	defer cancel()
	dead, nerr := w.queue.Nack(ctx, job, cause)
	if nerr != nil {
		return fmt.Errorf("nack: %w (cause: %v)", nerr, cause)
	}
	if dead {
		// The dead-job status settlement gets its OWN fresh budget: Nack above
		// may have consumed most/all of ctx, and reusing it here could skip the
		// permanent TranscriptFailed write on an expired context, stranding the
		// episode in "processing" even though the job is dead.
		markCtx, cancel := context.WithTimeout(context.Background(), w.settleTimeout)
		defer cancel()
		w.markTranscript(markCtx, job, model.TranscriptFailed)
	}
	return cause
}

// settle applies a job's terminal outcome under the shutdown-surviving context,
// fenced by the job's claim: the transcript (if any) and the episode status are
// written in one store transaction, and only while this attempt still holds the
// claim. A transcription can outlive its queue lease (the per-job timeout caps at
// 2h, the default lease is 10m), so by the time there is a result the job may have
// been reclaimed by another worker; the episode/transcript writes are keyed by
// episode, not job, so without the fence they would clobber the reclaiming
// attempt's state. ErrStaleClaim means exactly that — another worker owns the
// episode now — so it is not an error: discard the stale result and let the
// fenced Ack no-op. Any other write error is returned so the caller does not ack
// a job whose result was not durably recorded.
func (w *Worker) settle(ctx context.Context, job *model.Job, s *settlement) error {
	err := w.store.SettleEpisodeTranscript(ctx, job.ID, job.Attempts, s.ep.ID, s.transcript, s.status, time.Now())
	if errors.Is(err, store.ErrStaleClaim) {
		w.logger.Warn("transcription claim lost to reclaim; discarding stale settlement",
			"job", job.ID, "episode", s.ep.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: settle episode transcript: %w", err)
	}
	if s.transcript != nil {
		w.logger.Info("transcribed episode", "episode", s.ep.ID, "segments", len(s.transcript.Segments))
	}
	return nil
}

// handle runs a job's work under ctx and returns the terminal settlement to be
// persisted (nil when there is nothing to record, e.g. a deleted episode). The
// error is non-nil only when the work itself failed.
func (w *Worker) handle(ctx context.Context, job *model.Job) (*settlement, error) {
	switch job.Kind {
	case model.JobTranscribe:
		return w.transcribe(ctx, job)
	default:
		return nil, fmt.Errorf("worker: unknown job kind %q", job.Kind)
	}
}

func (w *Worker) transcribe(ctx context.Context, job *model.Job) (*settlement, error) {
	var p model.TranscribePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return nil, fmt.Errorf("worker: bad payload: %w", err)
	}

	ep, err := w.store.EpisodeByID(ctx, p.EpisodeID)
	if errors.Is(err, store.ErrNotFound) {
		// Episode was deleted before transcription ran; nothing to settle.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Non-terminal progress marker: best-effort under the cancellable work ctx,
	// fenced by the claim like every other episode write. A lost claim (another
	// worker reclaimed the job) is expected here and skipped silently.
	if err := w.store.SettleEpisodeTranscript(ctx, job.ID, job.Attempts, ep.ID, nil,
		model.TranscriptProcessing, time.Now()); err != nil && !errors.Is(err, store.ErrStaleClaim) {
		w.logger.Error("update transcript status", "episode", ep.ID,
			"status", model.TranscriptProcessing, "error", err)
	}

	rc, _, err := w.blobs.Get(ctx, ep.MediaKey)
	if err != nil {
		return nil, fmt.Errorf("worker: open media: %w", err)
	}
	defer rc.Close()

	// Bound the transcriber run so a stuck command is actually killed rather
	// than blocking the serial worker loop forever. The bound scales with the
	// episode's audio length; an unknown length falls back to a generous cap
	// (see JobTimeoutPolicy.timeout).
	jobCtx, cancel := context.WithTimeout(ctx, w.jobTimeout.timeout(ep.DurationSecs))
	defer cancel()

	res, err := w.transcriber.Transcribe(jobCtx, transcribe.Input{
		Audio:    rc,
		MIME:     ep.MediaMIME,
		Filename: ep.ID,
		Language: ep.Language,
	})
	if errors.Is(err, transcribe.ErrUnsupported) {
		// No transcriber configured: settle to "none", not a failure.
		return &settlement{ep: ep, status: model.TranscriptNone}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: transcribe: %w", err)
	}

	// The result is settled atomically under the claim fence in settle(): a
	// transcription can outlive its lease and be reclaimed by another worker, and
	// SettleEpisodeTranscript drops this attempt's transcript/status writes
	// (ErrStaleClaim) when that has happened, so they cannot clobber the reclaiming
	// attempt.
	tr := &model.Transcript{EpisodeID: ep.ID, Language: res.Language, CreatedAt: time.Now()}
	for _, s := range res.Segments {
		tr.Segments = append(tr.Segments, model.Segment{StartSecs: s.StartSecs, EndSecs: s.EndSecs, Text: s.Text})
	}
	return &settlement{ep: ep, transcript: tr, status: model.TranscriptDone}, nil
}

// markTranscript records a terminal transcript status for a job's episode (used
// when a job dies), fenced by the job's claim via SettleEpisodeTranscript so a
// stale attempt cannot write it. Best-effort: ErrStaleClaim means another worker
// owns the job now and is skipped silently; only unexpected errors are logged.
func (w *Worker) markTranscript(ctx context.Context, job *model.Job, status model.TranscriptStatus) {
	var p model.TranscribePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return
	}
	err := w.store.SettleEpisodeTranscript(ctx, job.ID, job.Attempts, p.EpisodeID, nil, status, time.Now())
	if err != nil && !errors.Is(err, store.ErrStaleClaim) {
		w.logger.Error("mark transcript status", "episode", p.EpisodeID, "status", status, "error", err)
	}
}
