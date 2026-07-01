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

func (s *Store) EnqueueJob(ctx context.Context, j *model.Job) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, kind, payload, status, attempts, last_error, run_after, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, string(j.Kind), j.Payload, string(j.Status), j.Attempts, j.LastError,
		toUnix(j.RunAfter), toUnix(j.CreatedAt), toUnix(j.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: enqueue job: %w", mapErr(err))
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

	// Lease the job: mark processing, bump attempts, push run_after out by the
	// lease so a crashed worker's job becomes reclaimable after it expires.
	j.Status = model.JobProcessing
	j.Attempts++
	j.UpdatedAt = now
	// Round the lease deadline up to whole seconds so the persisted run_after
	// never falls before the true sub-second deadline; RunAfter is normalized to
	// the stored precision so callers compare against the durable value.
	j.RunAfter = fromUnix(toUnixCeil(now.Add(lease)))
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, attempts = ?, run_after = ?, updated_at = ? WHERE id = ?`,
		string(j.Status), j.Attempts, toUnix(j.RunAfter), toUnix(j.UpdatedAt), j.ID); err != nil {
		return nil, mapErr(err)
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

// setJobStatus applies a terminal status transition, fenced by the claim token.
// The WHERE clause matches attempts = token so only the current claim can settle
// the job: a stale attempt whose lease expired and was reclaimed (which bumped
// attempts) affects zero rows and gets ErrStaleClaim, so it cannot clobber the
// reclaiming attempt's status.
func (s *Store) setJobStatus(ctx context.Context, id string, token int, status model.JobStatus, cause string, runAfter *time.Time) error {
	now := time.Now()
	var res sql.Result
	var err error
	if runAfter != nil {
		res, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET status = ?, last_error = ?, run_after = ?, updated_at = ? WHERE id = ? AND attempts = ?`,
			string(status), cause, toUnix(*runAfter), toUnix(now), id, token)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET status = ?, last_error = ?, updated_at = ? WHERE id = ? AND attempts = ?`,
			string(status), cause, toUnix(now), id, token)
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
