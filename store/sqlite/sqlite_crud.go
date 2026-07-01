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
