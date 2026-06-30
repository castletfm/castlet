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

// Worker transcribes episodes off the job queue. The receiver holds only
// configuration and is safe to Run multiple times.
type Worker struct {
	store        store.Store
	blobs        blob.BlobStore
	queue        queue.JobQueue
	transcriber  transcribe.Transcriber
	pollInterval time.Duration
	logger       *slog.Logger
}

// Option configures New.
type Option = option.Interface

type (
	identPollInterval struct{}
	identLogger       struct{}
)

// WithPollInterval sets how often the worker polls when the queue is empty
// (default 5s). When a job is found the worker drains the queue without
// waiting for the next tick.
func WithPollInterval(d time.Duration) Option { return option.New(identPollInterval{}, d) }

// WithLogger sets the structured logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return option.New(identLogger{}, l) }

// New constructs a Worker from its dependencies.
func New(st store.Store, blobs blob.BlobStore, q queue.JobQueue, tr transcribe.Transcriber, options ...Option) *Worker {
	w := &Worker{
		store:        st,
		blobs:        blobs,
		queue:        q,
		transcriber:  tr,
		pollInterval: 5 * time.Second,
		logger:       slog.Default(),
	}
	for _, o := range options {
		switch o.Ident().(type) {
		case identPollInterval:
			w.pollInterval = option.MustGet[time.Duration](o)
		case identLogger:
			w.logger = option.MustGet[*slog.Logger](o)
		}
	}
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
		// Drain everything currently runnable before sleeping.
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

// processOne claims and handles a single job. It reports whether a job was
// processed (so the caller can keep draining) and any handling error.
func (w *Worker) processOne(ctx context.Context) (bool, error) {
	job, err := w.queue.Dequeue(ctx, model.JobTranscribe)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	if herr := w.handle(ctx, job); herr != nil {
		dead, nerr := w.queue.Nack(ctx, job.ID, herr)
		if nerr != nil {
			return true, fmt.Errorf("nack: %w (cause: %v)", nerr, herr)
		}
		if dead {
			w.markTranscript(ctx, job, model.TranscriptFailed)
		}
		return true, herr
	}
	return true, w.queue.Ack(ctx, job.ID)
}

func (w *Worker) handle(ctx context.Context, job *model.Job) error {
	switch job.Kind {
	case model.JobTranscribe:
		return w.transcribe(ctx, job)
	default:
		return fmt.Errorf("worker: unknown job kind %q", job.Kind)
	}
}

func (w *Worker) transcribe(ctx context.Context, job *model.Job) error {
	var p model.TranscribePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return fmt.Errorf("worker: bad payload: %w", err)
	}

	ep, err := w.store.EpisodeByID(ctx, p.EpisodeID)
	if errors.Is(err, store.ErrNotFound) {
		// Episode was deleted before transcription ran; nothing to do.
		return nil
	}
	if err != nil {
		return err
	}

	w.setStatus(ctx, ep, model.TranscriptProcessing)

	rc, _, err := w.blobs.Get(ctx, ep.MediaKey)
	if err != nil {
		return fmt.Errorf("worker: open media: %w", err)
	}
	defer rc.Close()

	res, err := w.transcriber.Transcribe(ctx, transcribe.Input{
		Audio:    rc,
		MIME:     ep.MediaMIME,
		Filename: ep.Slug,
	})
	if errors.Is(err, transcribe.ErrUnsupported) {
		// No transcriber configured: settle to "none", not a failure.
		w.setStatus(ctx, ep, model.TranscriptNone)
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: transcribe: %w", err)
	}

	tr := &model.Transcript{EpisodeID: ep.ID, Language: res.Language, CreatedAt: time.Now()}
	for _, s := range res.Segments {
		tr.Segments = append(tr.Segments, model.Segment{StartSecs: s.StartSecs, EndSecs: s.EndSecs, Text: s.Text})
	}
	if err := w.store.SaveTranscript(ctx, tr); err != nil {
		return fmt.Errorf("worker: save transcript: %w", err)
	}
	w.setStatus(ctx, ep, model.TranscriptDone)
	w.logger.Info("transcribed episode", "episode", ep.ID, "segments", len(tr.Segments))
	return nil
}

func (w *Worker) setStatus(ctx context.Context, ep *model.Episode, status model.TranscriptStatus) {
	ep.TranscriptStatus = status
	ep.UpdatedAt = time.Now()
	if err := w.store.UpdateEpisode(ctx, ep); err != nil {
		w.logger.Error("update transcript status", "episode", ep.ID, "status", status, "error", err)
	}
}

// markTranscript loads the job's episode and records a terminal transcript
// status (used when a job dies). Best-effort.
func (w *Worker) markTranscript(ctx context.Context, job *model.Job, status model.TranscriptStatus) {
	var p model.TranscribePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return
	}
	ep, err := w.store.EpisodeByID(ctx, p.EpisodeID)
	if err != nil {
		return
	}
	w.setStatus(ctx, ep, status)
}
