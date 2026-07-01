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
	"github.com/castletfm/castlet/internal/metrics"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue"
	"github.com/castletfm/castlet/store"
	"github.com/castletfm/castlet/transcribe"
	"github.com/lestrrat-go/option/v3"
)

// Metric names the worker records into its registry.
const (
	metricJobsTotal   = "transcription_jobs_total"
	metricLastSuccess = "worker_last_success_timestamp_seconds"
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
	metrics       *metrics.Registry
}

// Option configures New.
type Option = option.Interface

type (
	identPollInterval  struct{}
	identJobTimeout    struct{}
	identSettleTimeout struct{}
	identLogger        struct{}
	identMetrics       struct{}
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

// WithMetrics sets the metrics registry the worker records job outcomes and its
// last-success timestamp into. Share one registry with the server so /metrics
// reports both. When unset, the worker records into a private registry.
func WithMetrics(r *metrics.Registry) Option { return option.New(identMetrics{}, r) }

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
		case identMetrics:
			w.metrics = option.MustGet[*metrics.Registry](o)
		}
	}
	w.jobTimeout = w.jobTimeout.withDefaults()
	if w.metrics == nil {
		w.metrics = metrics.New()
	}
	w.metrics.Register(metricJobsTotal, metrics.Counter, "Transcription job attempts by outcome (success or failure).")
	w.metrics.Register(metricLastSuccess, metrics.Gauge, "Unix timestamp of the worker's last successful transcription.")
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
// settleAndComplete under the shutdown-surviving context after the (cancellable)
// work has finished, so the episode's final state is recorded even on SIGTERM.
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
		w.metrics.Inc(metricJobsTotal, "outcome", "failure")
		return true, nil
	}
	s, herr := w.handle(ctx, job)

	// The terminal bookkeeping (the combined settle+complete on success, or the
	// Nack/fail path) must survive shutdown. handle() runs the transcription under
	// ctx (so the work itself stops on SIGTERM), but settling with that same,
	// now-cancelled ctx would fail with context.Canceled: the outcome would be lost
	// while the episode stayed stuck mid-transcription. Each terminal step below
	// runs under its OWN fresh background-derived timeout, so it is always durable
	// even if an earlier step consumed its whole budget.
	if herr != nil {
		return true, w.fail(job, herr)
	}
	// A nil settlement means there is nothing to record (e.g. the episode was
	// deleted mid-job); just complete the job so it is not retried forever.
	if s == nil {
		return true, w.ack(job)
	}
	// Persist the terminal outcome AND complete the job in ONE fenced store
	// transaction. Coupling the two means a reclaim can never land between the
	// episode write and the completion to downgrade an already-done episode. A
	// lost claim makes the whole unit a no-op (ErrStaleClaim, handled in
	// settleAndComplete); any other error means the outcome was not durably
	// recorded, so Nack the job for retry rather than leave it done with the
	// episode stuck processing.
	ctx2, cancel := context.WithTimeout(context.Background(), w.settleTimeout)
	serr := w.settleAndComplete(ctx2, job, s)
	cancel()
	if serr != nil {
		return true, w.fail(job, serr)
	}
	w.metrics.Inc(metricJobsTotal, "outcome", "success")
	w.metrics.Set(metricLastSuccess, float64(time.Now().Unix()))
	return true, nil
}

// ack is the no-side-effects completion path: it marks a job done under a fresh
// shutdown-surviving context when there is nothing to settle (a nil settlement,
// e.g. the episode was deleted mid-job). The successful transcription path does
// NOT come here — it completes the job inside settleAndComplete's fenced
// transaction. Deriving its own context keeps the completion durable even on
// SIGTERM, so such a job is never left stuck in "processing".
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
	w.metrics.Inc(metricJobsTotal, "outcome", "failure")
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

// settleAndComplete applies a job's SUCCESSFUL terminal outcome in ONE fenced
// store transaction under the shutdown-surviving context: the transcript (if
// any), the episode status, and the job completion are written together, and
// only while this attempt still holds the claim. Coupling the completion to the
// episode write is what closes the window where a reclaim could land between a
// separate settle and Ack and downgrade an already-done episode. A transcription
// can outlive its queue lease (the per-job timeout caps at 2h, the default lease
// is 10m), so by the time there is a result the job may have been reclaimed by
// another worker; ErrStaleClaim means exactly that — another worker owns the
// episode now — so it is not an error: discard the stale result, changing
// nothing. Any other write error is returned so the caller Nacks a job whose
// result was not durably recorded.
func (w *Worker) settleAndComplete(ctx context.Context, job *model.Job, s *settlement) error {
	err := w.store.SettleEpisodeTranscriptAndCompleteJob(ctx, job.ID, job.Attempts, s.ep.ID, s.transcript, s.status, time.Now())
	if errors.Is(err, store.ErrStaleClaim) {
		w.logger.Warn("transcription claim lost to reclaim; discarding stale settlement",
			"job", job.ID, "episode", s.ep.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: settle episode transcript and complete job: %w", err)
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

	// The result is settled atomically under the claim fence in settleAndComplete:
	// a transcription can outlive its lease and be reclaimed by another worker, and
	// Store.SettleEpisodeTranscriptAndCompleteJob drops this attempt's
	// transcript/status writes (ErrStaleClaim) when that has happened, so they
	// cannot clobber the reclaiming attempt.
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
