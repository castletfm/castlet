package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
)

// rowExecer is the subset of *sql.DB / *sql.Tx used to insert a job, so the same
// INSERT can run standalone (EnqueueJob) or inside a larger transaction
// (EnqueueTranscriptionJob).
type rowExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertJob writes a job row using the given executor (the pool for a standalone
// enqueue, or an open transaction when the insert must commit atomically with
// other writes).
func insertJob(ctx context.Context, ex rowExecer, j *model.Job) error {
	_, err := ex.ExecContext(ctx,
		`INSERT INTO jobs (id, kind, payload, status, attempts, last_error, run_after, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, string(j.Kind), j.Payload, string(j.Status), j.Attempts, j.LastError,
		toUnix(j.RunAfter), toUnix(j.CreatedAt), toUnix(j.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: enqueue job: %w", mapErr(err))
	}
	return nil
}

func (s *Store) EnqueueJob(ctx context.Context, j *model.Job) error {
	return insertJob(ctx, s.db, j)
}

// EnqueueTranscriptionJob inserts the job and marks the episode pending in one
// transaction so the two are indivisible: on the single-writer pool either both
// land or, on any failure, the whole thing rolls back and nothing is written.
//
// The episode->pending transition is the guard, not a separate pre-check: the
// UPDATE only matches an episode that is NOT already pending or processing, and
// the job is inserted only when that UPDATE touched its row. This makes the
// "already queued/running" decision atomic with the insert, so two concurrent
// enqueues for the same episode cannot both queue a job — the first flips the
// status and inserts, the second's conditional UPDATE matches zero rows and it
// returns ErrConflict without inserting a duplicate. A missing episode yields
// ErrNotFound. See store.Store for the contract.
func (s *Store) EnqueueTranscriptionJob(ctx context.Context, j *model.Job, episodeID string, updatedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: enqueue transcription job: %w", mapErr(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// Guard the transition inside the transaction: only advance an episode that is
	// not already pending/processing. On the single-writer pool a concurrent
	// enqueue that already flipped the status leaves zero rows here.
	res, err := tx.ExecContext(ctx,
		`UPDATE episodes SET transcript_status = ?, updated_at = ?
		 WHERE id = ? AND transcript_status NOT IN (?, ?)`,
		string(model.TranscriptPending), toUnix(updatedAt), episodeID,
		string(model.TranscriptPending), string(model.TranscriptProcessing))
	if err != nil {
		return fmt.Errorf("sqlite: mark episode pending: %w", mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// The UPDATE matched no row for one of two reasons; distinguish them so a
		// missing episode still surfaces as ErrNotFound while an already
		// pending/processing episode is a conflict. No job is inserted either way.
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM episodes WHERE id = ?)`, episodeID).Scan(&exists); err != nil {
			return fmt.Errorf("sqlite: enqueue transcription job: %w", mapErr(err))
		}
		if !exists {
			return store.ErrNotFound
		}
		return store.ErrConflict // already pending or processing
	}
	// The episode was advanced by this transaction: queue the job to match, so the
	// insert and the mark commit together.
	if err := insertJob(ctx, tx, j); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: enqueue transcription job: %w", mapErr(err))
	}
	return nil
}

func (s *Store) JobByID(ctx context.Context, id string) (*model.Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT id, kind, payload, status, attempts, last_error, run_after, created_at, updated_at
		 FROM jobs WHERE id = ?`, id))
	if err != nil {
		return nil, mapErr(err)
	}
	return j, nil
}

// ClaimJob atomically selects and locks the oldest runnable job. A job is
// runnable when its RunAfter is due and it is either pending or processing with
// an expired lease (its worker crashed before finishing); the latter is how a
// stuck job is reclaimed. Because the pool is a single writer, the
// SELECT-then-UPDATE inside one transaction is race-free across worker
// goroutines and processes sharing the file.
func (s *Store) ClaimJob(ctx context.Context, kinds []model.JobKind, now time.Time, lease time.Duration) (*model.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// A freshly leased processing job has run_after in the future, so the
	// run_after <= now gate only reclaims processing jobs whose lease expired.
	query := `SELECT id, kind, payload, status, attempts, last_error, run_after, created_at, updated_at
		FROM jobs WHERE status IN (?, ?) AND run_after <= ?`
	args := []any{string(model.JobPending), string(model.JobProcessing), toUnix(now)}
	if len(kinds) > 0 {
		ph := make([]string, len(kinds))
		for i, k := range kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		query += " AND kind IN (" + strings.Join(ph, ",") + ")"
	}
	query += " ORDER BY run_after ASC, created_at ASC LIMIT 1"

	j, err := scanJob(tx.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, mapErr(err) // ErrNotFound when nothing is runnable
	}

	// Snapshot the exact row state the claim decision was made on, so the UPDATE
	// can be conditioned on it and reject a row that changed underneath us.
	prevStatus := string(j.Status)
	prevAttempts := j.Attempts
	prevRunAfter := toUnix(j.RunAfter)

	// Lease the job: mark processing, bump attempts, push run_after out by the
	// lease so a crashed worker's job becomes reclaimable after it expires.
	j.Status = model.JobProcessing
	j.Attempts++
	j.UpdatedAt = now
	// Round the lease deadline up to whole seconds so the persisted run_after
	// never falls before the true sub-second deadline; RunAfter is normalized to
	// the stored precision so callers compare against the durable value.
	j.RunAfter = fromUnix(toUnixCeil(now.Add(lease)))
	// Condition the UPDATE on the SELECTED row's status/attempts/run_after so a
	// concurrent claim that already mutated the row cannot be double-claimed. On
	// the single-writer pool this always matches; the guard defends the invariant
	// regardless. RowsAffected != 1 means the row was claimed out from under us, so
	// report nothing runnable rather than returning a row we did not actually lease.
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, attempts = ?, run_after = ?, updated_at = ?
		 WHERE id = ? AND status = ? AND attempts = ? AND run_after = ?`,
		string(j.Status), j.Attempts, toUnix(j.RunAfter), toUnix(j.UpdatedAt),
		j.ID, prevStatus, prevAttempts, prevRunAfter)
	if err != nil {
		return nil, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, store.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, mapErr(err)
	}
	return j, nil
}

func (s *Store) CompleteJob(ctx context.Context, id string, token int) error {
	return s.setJobStatus(ctx, id, token, model.JobDone, "", nil)
}

func (s *Store) RescheduleJob(ctx context.Context, id string, token int, runAfter time.Time, cause string) error {
	return s.setJobStatus(ctx, id, token, model.JobPending, cause, &runAfter)
}

func (s *Store) FailJob(ctx context.Context, id string, token int, cause string) error {
	return s.setJobStatus(ctx, id, token, model.JobFailed, cause, nil)
}

// setJobStatus applies a status transition, fenced by the claim token AND the
// job being currently processing. The WHERE clause matches attempts = token so
// only the current claim can settle the job: a stale attempt whose lease expired
// and was reclaimed (which bumped attempts) affects zero rows and gets
// ErrStaleClaim, so it cannot clobber the reclaiming attempt's status. The
// additional status = 'processing' guard makes terminal states final: a job only
// transitions FROM processing TO a terminal/next state, so once it is done or
// failed the same token can no longer move it (e.g. a Complete followed by a
// stray Reschedule affects zero rows and gets ErrStaleClaim).
func (s *Store) setJobStatus(ctx context.Context, id string, token int, status model.JobStatus, cause string, runAfter *time.Time) error {
	now := time.Now()
	var res sql.Result
	var err error
	if runAfter != nil {
		res, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET status = ?, last_error = ?, run_after = ?, updated_at = ? WHERE id = ? AND attempts = ? AND status = ?`,
			string(status), cause, toUnix(*runAfter), toUnix(now), id, token, string(model.JobProcessing))
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET status = ?, last_error = ?, updated_at = ? WHERE id = ? AND attempts = ? AND status = ?`,
			string(status), cause, toUnix(now), id, token, string(model.JobProcessing))
	}
	if err != nil {
		return fmt.Errorf("sqlite: update job status: %w", mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrStaleClaim
	}
	return nil
}

func scanJob(sc interface{ Scan(...any) error }) (*model.Job, error) {
	var (
		j                      model.Job
		kind, status           string
		runAfter, created, upd int64
	)
	if err := sc.Scan(&j.ID, &kind, &j.Payload, &status, &j.Attempts, &j.LastError,
		&runAfter, &created, &upd); err != nil {
		return nil, err
	}
	j.Kind = model.JobKind(kind)
	j.Status = model.JobStatus(status)
	j.RunAfter = fromUnix(runAfter)
	j.CreatedAt = fromUnix(created)
	j.UpdatedAt = fromUnix(upd)
	return &j, nil
}
