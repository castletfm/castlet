package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
)

// --- users ------------------------------------------------------------------

const userCols = `id, email, display_name, password_hash, oidc_issuer, oidc_subject, created_at`

func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, password_hash, oidc_issuer, oidc_subject, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.DisplayName, u.PasswordHash, u.OIDCIssuer, u.OIDCSubject, toUnix(u.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create user: %w", mapErr(err))
	}
	return nil
}

func (s *Store) UpdateUser(ctx context.Context, u *model.User) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET email = ?, display_name = ?, password_hash = ?, oidc_issuer = ?, oidc_subject = ?
		 WHERE id = ?`,
		u.Email, u.DisplayName, u.PasswordHash, u.OIDCIssuer, u.OIDCSubject, u.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update user: %w", mapErr(err))
	}
	return requireAffected(res)
}

func (s *Store) UserByID(ctx context.Context, id string) (*model.User, error) {
	return s.userWhere(ctx, "id = ?", id)
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*model.User, error) {
	return s.userWhere(ctx, "email = ?", email)
}

func (s *Store) UserByOIDCSubject(ctx context.Context, issuer, subject string) (*model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE oidc_issuer = ? AND oidc_subject = ? AND oidc_subject <> ''`,
		issuer, subject)
	return scanUser(row)
}

func (s *Store) userWhere(ctx context.Context, cond string, arg any) (*model.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE `+cond, arg))
}

func scanUser(sc interface{ Scan(...any) error }) (*model.User, error) {
	var (
		u       model.User
		created int64
	)
	if err := sc.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash,
		&u.OIDCIssuer, &u.OIDCSubject, &created); err != nil {
		return nil, mapErr(err)
	}
	u.CreatedAt = fromUnix(created)
	return &u, nil
}

// --- channels ---------------------------------------------------------------

func (s *Store) CreateChannel(ctx context.Context, c *model.Channel) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (id, user_id, title, description, language, image_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.UserID, c.Title, c.Description, c.Language, c.ImageKey,
		toUnix(c.CreatedAt), toUnix(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create channel: %w", mapErr(err))
	}
	return nil
}

func (s *Store) UpdateChannel(ctx context.Context, c *model.Channel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET title = ?, description = ?, language = ?, image_key = ?, updated_at = ?
		 WHERE id = ?`,
		c.Title, c.Description, c.Language, c.ImageKey, toUnix(c.UpdatedAt), c.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update channel: %w", mapErr(err))
	}
	return requireAffected(res)
}

func (s *Store) ChannelByID(ctx context.Context, id string) (*model.Channel, error) {
	return s.channelWhere(ctx, "id = ?", id)
}

const channelCols = `id, user_id, title, description, language, image_key, created_at, updated_at`

func scanChannel(sc interface{ Scan(...any) error }) (*model.Channel, error) {
	var (
		c                model.Channel
		created, updated int64
	)
	if err := sc.Scan(&c.ID, &c.UserID, &c.Title, &c.Description, &c.Language,
		&c.ImageKey, &created, &updated); err != nil {
		return nil, err
	}
	c.CreatedAt = fromUnix(created)
	c.UpdatedAt = fromUnix(updated)
	return &c, nil
}

func (s *Store) channelWhere(ctx context.Context, cond string, arg any) (*model.Channel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE `+cond, arg)
	c, err := scanChannel(row)
	if err != nil {
		return nil, mapErr(err)
	}
	return c, nil
}

func (s *Store) ListChannels(ctx context.Context) ([]*model.Channel, error) {
	return s.channelList(ctx, `SELECT `+channelCols+` FROM channels ORDER BY title COLLATE NOCASE`)
}

func (s *Store) ListChannelsByUser(ctx context.Context, userID string) ([]*model.Channel, error) {
	return s.channelList(ctx,
		`SELECT `+channelCols+` FROM channels WHERE user_id = ? ORDER BY title COLLATE NOCASE`, userID)
}

func (s *Store) ChannelImageKeyExists(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM channels WHERE image_key = ? AND image_key <> ''`,
		key).Scan(&n)
	if err != nil {
		return false, mapErr(err)
	}
	return n > 0, nil
}

func (s *Store) channelList(ctx context.Context, query string, args ...any) ([]*model.Channel, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*model.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, c)
	}
	return out, mapErr(rows.Err())
}

// --- episodes ---------------------------------------------------------------

const episodeCols = `id, channel_id, title, description, media_key, media_mime, media_kind,
	media_bytes, duration_secs, status, transcript_status, published_at, created_at, updated_at, language, position`

func (s *Store) CreateEpisode(ctx context.Context, e *model.Episode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO episodes (id, channel_id, title, description, media_key, media_mime, media_kind,
			media_bytes, duration_secs, status, transcript_status, published_at, created_at, updated_at, language, position)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ChannelID, e.Title, e.Description, e.MediaKey, e.MediaMIME, string(e.MediaKind),
		e.MediaBytes, e.DurationSecs, string(e.Status), string(e.TranscriptStatus),
		toUnixPtr(e.PublishedAt), toUnix(e.CreatedAt), toUnix(e.UpdatedAt), e.Language, e.Position)
	if err != nil {
		return fmt.Errorf("sqlite: create episode: %w", mapErr(err))
	}
	return nil
}

func (s *Store) UpdateEpisode(ctx context.Context, e *model.Episode) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET title = ?, description = ?, media_key = ?, media_mime = ?, media_kind = ?,
			media_bytes = ?, duration_secs = ?, status = ?, transcript_status = ?, published_at = ?, updated_at = ?,
			language = ?, position = ?
		 WHERE id = ?`,
		e.Title, e.Description, e.MediaKey, e.MediaMIME, string(e.MediaKind), e.MediaBytes, e.DurationSecs,
		string(e.Status), string(e.TranscriptStatus), toUnixPtr(e.PublishedAt), toUnix(e.UpdatedAt), e.Language, e.Position, e.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update episode: %w", mapErr(err))
	}
	return requireAffected(res)
}

// SetEpisodeTranscriptStatus updates only the transcript_status and updated_at
// columns, leaving the rest of the row untouched. The transcription worker uses
// this instead of UpdateEpisode so an admin edit made mid-transcription is not
// reverted by the worker's stale in-memory copy of the episode.
func (s *Store) SetEpisodeTranscriptStatus(ctx context.Context, id string, status model.TranscriptStatus, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET transcript_status = ?, updated_at = ? WHERE id = ?`,
		string(status), toUnix(updatedAt), id)
	if err != nil {
		return fmt.Errorf("sqlite: set episode transcript status: %w", mapErr(err))
	}
	return requireAffected(res)
}

// channelEpisodeIDs returns the set of episode ids currently belonging to the
// channel, read through tx so it sees the transaction's own view.
func channelEpisodeIDs(ctx context.Context, tx *sql.Tx, channelID string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM episodes WHERE channel_id = ?`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ReorderEpisodes renumbers a channel's episodes atomically. It first loads the
// channel's current episode ids and requires orderedIDs to be an exact
// permutation of that set: a list with duplicates, a missing current episode, or
// a foreign id (one not in the channel) is rejected with store.ErrInvalidReorder
// before any UPDATE runs, so a stale or malformed list can never commit
// duplicate or gapped positions. Each id's position is then set to its index in
// orderedIDs inside a single transaction, so a failure part-way through rolls
// back and cannot leave positions partially renumbered. The `position <> ?`
// guard skips rows already at their target position, so updated_at is only
// bumped for episodes that actually move.
func (s *Store) ReorderEpisodes(ctx context.Context, channelID string, orderedIDs []string, updatedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// Load the channel's current episode ids so orderedIDs can be validated as an
	// exact permutation before anything is renumbered.
	current, err := channelEpisodeIDs(ctx, tx, channelID)
	if err != nil {
		return fmt.Errorf("sqlite: reorder episodes: %w", mapErr(err))
	}

	if len(orderedIDs) != len(current) {
		return fmt.Errorf("sqlite: reorder episodes: expected %d ids, got %d: %w",
			len(current), len(orderedIDs), store.ErrInvalidReorder)
	}
	seen := make(map[string]bool, len(orderedIDs))
	for _, id := range orderedIDs {
		if !current[id] {
			return fmt.Errorf("sqlite: reorder episodes: id %q is not in channel: %w",
				id, store.ErrInvalidReorder)
		}
		if seen[id] {
			return fmt.Errorf("sqlite: reorder episodes: duplicate id %q: %w",
				id, store.ErrInvalidReorder)
		}
		seen[id] = true
	}
	// Equal length + all present in current + no duplicates ⇒ exact permutation,
	// so every current episode is covered and none is missing.

	for idx, id := range orderedIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE episodes SET position = ?, updated_at = ?
			 WHERE id = ? AND channel_id = ? AND position <> ?`,
			idx, toUnix(updatedAt), id, channelID, idx); err != nil {
			return fmt.Errorf("sqlite: reorder episodes: %w", mapErr(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: reorder episodes: %w", mapErr(err))
	}
	return nil
}

func (s *Store) DeleteEpisode(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM episodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete episode: %w", mapErr(err))
	}
	return requireAffected(res)
}

func (s *Store) EpisodeByID(ctx context.Context, id string) (*model.Episode, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+episodeCols+` FROM episodes WHERE id = ?`, id)
	e, err := scanEpisode(row)
	if err != nil {
		return nil, mapErr(err)
	}
	return e, nil
}

func (s *Store) EpisodeByMediaKey(ctx context.Context, key string) (*model.Episode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+episodeCols+` FROM episodes WHERE media_key = ?`, key)
	e, err := scanEpisode(row)
	if err != nil {
		return nil, mapErr(err)
	}
	return e, nil
}

func (s *Store) PublishedEpisodeByMediaKey(ctx context.Context, key string) (*model.Episode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+episodeCols+` FROM episodes WHERE media_key = ? AND status = ? LIMIT 1`,
		key, string(model.EpisodePublished))
	e, err := scanEpisode(row)
	if err != nil {
		return nil, mapErr(err)
	}
	return e, nil
}

func (s *Store) ListEpisodes(ctx context.Context, f store.EpisodeFilter) ([]*model.Episode, error) {
	query := `SELECT ` + episodeCols + ` FROM episodes`
	var (
		conds []string
		args  []any
	)
	if f.ChannelID != "" {
		conds = append(conds, "channel_id = ?")
		args = append(args, f.ChannelID)
	}
	if f.PublishedOnly {
		conds = append(conds, "status = ?")
		args = append(args, string(model.EpisodePublished))
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	// Manual order first (ascending); ties fall back to newest-published-first,
	// so a channel that has never been reordered keeps the default ordering.
	query += " ORDER BY position ASC, COALESCE(published_at, created_at) DESC, created_at DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*model.Episode
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, e)
	}
	return out, mapErr(rows.Err())
}

func scanEpisode(sc interface{ Scan(...any) error }) (*model.Episode, error) {
	var (
		e                      model.Episode
		status, tstatus, mkind string
		published              sql.NullInt64
		created, updated       int64
	)
	if err := sc.Scan(&e.ID, &e.ChannelID, &e.Title, &e.Description, &e.MediaKey,
		&e.MediaMIME, &mkind, &e.MediaBytes, &e.DurationSecs, &status, &tstatus, &published,
		&created, &updated, &e.Language, &e.Position); err != nil {
		return nil, err
	}
	e.MediaKind = model.MediaKind(mkind)
	e.Status = model.EpisodeStatus(status)
	e.TranscriptStatus = model.TranscriptStatus(tstatus)
	e.PublishedAt = fromUnixPtr(published)
	e.CreatedAt = fromUnix(created)
	e.UpdatedAt = fromUnix(updated)
	return &e, nil
}

// --- transcripts ------------------------------------------------------------

func (s *Store) SaveTranscript(ctx context.Context, t *model.Transcript) error {
	segs, err := json.Marshal(t.Segments)
	if err != nil {
		return fmt.Errorf("sqlite: marshal segments: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO transcripts (episode_id, language, segments, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(episode_id) DO UPDATE SET language = excluded.language,
			segments = excluded.segments, created_at = excluded.created_at`,
		t.EpisodeID, t.Language, string(segs), toUnix(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: save transcript: %w", mapErr(err))
	}
	return nil
}

func (s *Store) TranscriptByEpisode(ctx context.Context, episodeID string) (*model.Transcript, error) {
	var (
		t       model.Transcript
		segs    string
		created int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT episode_id, language, segments, created_at FROM transcripts WHERE episode_id = ?`,
		episodeID).Scan(&t.EpisodeID, &t.Language, &segs, &created)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := json.Unmarshal([]byte(segs), &t.Segments); err != nil {
		return nil, fmt.Errorf("sqlite: unmarshal segments: %w", err)
	}
	t.CreatedAt = fromUnix(created)
	return &t, nil
}

// SettleEpisodeTranscript atomically records a transcription job's episode side
// effects under the fence of its claim. See store.Store for the contract.
//
// The fence is the claim token (jobs.attempts), which ClaimJob bumps on every
// (re)claim: a stale attempt whose job was reclaimed no longer matches and
// affects zero rows, so it gets ErrStaleClaim and touches neither the episode nor
// the transcript. On top of the token, the accepted job status follows the real
// state machine so a same-token call cannot settle an already-terminal job: every
// progress/none/done settlement and every transcript save requires the job to
// still be 'processing' (the state a live claim holds before its Ack), while
// jobs.status = 'failed' is accepted ONLY for the transcript-less TranscriptFailed
// mark — the dead-letter / Nack-exhaustion write that legitimately runs right
// after the same claim flipped the job to 'failed'. Doing the claim check and the
// mutations in one transaction (on the single-writer pool) makes the
// check-and-write indivisible.
func (s *Store) SettleEpisodeTranscript(ctx context.Context, jobID string, token int, episodeID string, transcript *model.Transcript, status model.TranscriptStatus, updatedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: settle episode transcript: %w", mapErr(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// The default state machine transition is FROM processing; only the
	// transcript-less failed-episode mark may additionally run against an
	// already-'failed' job (the dead-letter/Nack-exhaustion settlement).
	statusCond := "status = ?"
	statusArgs := []any{string(model.JobProcessing)}
	if status == model.TranscriptFailed && transcript == nil {
		statusCond = "status IN (?, ?)"
		statusArgs = []any{string(model.JobProcessing), string(model.JobFailed)}
	}

	var claimed int
	args := append([]any{jobID, token}, statusArgs...)
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM jobs WHERE id = ? AND attempts = ? AND `+statusCond,
		args...).Scan(&claimed); err != nil {
		return fmt.Errorf("sqlite: verify job claim: %w", mapErr(err))
	}
	if claimed == 0 {
		return store.ErrStaleClaim
	}

	if transcript != nil {
		if err := saveTranscriptTx(ctx, tx, transcript); err != nil {
			return err
		}
	}
	if err := setEpisodeStatusTx(ctx, tx, episodeID, status, updatedAt); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: settle episode transcript: %w", mapErr(err))
	}
	return nil
}

// SettleEpisodeTranscriptAndCompleteJob atomically settles a SUCCESSFUL
// transcription: in ONE transaction it completes the job and records the episode
// side effects, so a reclaim can never land between the episode write and the job
// completion (the gap that previously let a reclaiming attempt downgrade an
// already-done episode). See store.Store for the contract.
//
// The claim fence and the completion are the SAME statement: the job is
// transitioned to done only while attempts == token AND status = 'processing'
// (the state a live claim holds before its Ack). A RowsAffected of 0 means the
// claim is no longer valid — the job was reclaimed (attempts advanced) or is
// already terminal — so the whole settlement (transcript save, episode status,
// and job completion) is rejected together with ErrStaleClaim and nothing is
// written. Only when the fence holds are the transcript (when non-nil) and the
// targeted episode transcript_status write applied, all committed as one unit.
func (s *Store) SettleEpisodeTranscriptAndCompleteJob(ctx context.Context, jobID string, token int, episodeID string, transcript *model.Transcript, status model.TranscriptStatus, updatedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: settle episode transcript and complete job: %w", mapErr(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// Fence AND complete in one statement: only the live claim can finish the job.
	// Clear last_error so a successful completion wipes any message left by a prior
	// failed attempt (RescheduleJob), matching CompleteJob -> setJobStatus.
	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, last_error = '', updated_at = ? WHERE id = ? AND attempts = ? AND status = ?`,
		string(model.JobDone), toUnix(updatedAt), jobID, token, string(model.JobProcessing))
	if err != nil {
		return fmt.Errorf("sqlite: complete job: %w", mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrStaleClaim
	}

	if transcript != nil {
		if err := saveTranscriptTx(ctx, tx, transcript); err != nil {
			return err
		}
	}
	if err := setEpisodeStatusTx(ctx, tx, episodeID, status, updatedAt); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: settle episode transcript and complete job: %w", mapErr(err))
	}
	return nil
}

// saveTranscriptTx upserts a transcript within an open transaction, so the
// episode settlement methods can save it atomically with their other writes.
func saveTranscriptTx(ctx context.Context, tx *sql.Tx, t *model.Transcript) error {
	segs, err := json.Marshal(t.Segments)
	if err != nil {
		return fmt.Errorf("sqlite: marshal segments: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO transcripts (episode_id, language, segments, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(episode_id) DO UPDATE SET language = excluded.language,
			segments = excluded.segments, created_at = excluded.created_at`,
		t.EpisodeID, t.Language, string(segs), toUnix(t.CreatedAt)); err != nil {
		return fmt.Errorf("sqlite: save transcript: %w", mapErr(err))
	}
	return nil
}

// setEpisodeStatusTx applies a targeted transcript_status write within an open
// transaction, leaving the rest of the episode row untouched so a concurrent
// admin edit is not reverted.
func setEpisodeStatusTx(ctx context.Context, tx *sql.Tx, episodeID string, status model.TranscriptStatus, updatedAt time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE episodes SET transcript_status = ?, updated_at = ? WHERE id = ?`,
		string(status), toUnix(updatedAt), episodeID); err != nil {
		return fmt.Errorf("sqlite: set episode transcript status: %w", mapErr(err))
	}
	return nil
}

// requireAffected converts a zero-rows-affected result into ErrNotFound so
// updates/deletes of missing ids surface consistently.
func requireAffected(res interface{ RowsAffected() (int64, error) }) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
