package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
)

// --- users ------------------------------------------------------------------

func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, password_hash, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.DisplayName, u.PasswordHash, toUnix(u.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create user: %w", mapErr(err))
	}
	return nil
}

func (s *Store) UserByID(ctx context.Context, id string) (*model.User, error) {
	return s.userWhere(ctx, "id = ?", id)
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*model.User, error) {
	return s.userWhere(ctx, "email = ?", email)
}

func (s *Store) userWhere(ctx context.Context, cond string, arg any) (*model.User, error) {
	var (
		u       model.User
		created int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, password_hash, created_at FROM users WHERE `+cond,
		arg).Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &created)
	if err != nil {
		return nil, mapErr(err)
	}
	u.CreatedAt = fromUnix(created)
	return &u, nil
}

// --- channels ---------------------------------------------------------------

func (s *Store) CreateChannel(ctx context.Context, c *model.Channel) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (id, user_id, slug, title, description, language, image_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.UserID, c.Slug, c.Title, c.Description, c.Language, c.ImageKey,
		toUnix(c.CreatedAt), toUnix(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create channel: %w", mapErr(err))
	}
	return nil
}

func (s *Store) UpdateChannel(ctx context.Context, c *model.Channel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET slug = ?, title = ?, description = ?, language = ?, image_key = ?, updated_at = ?
		 WHERE id = ?`,
		c.Slug, c.Title, c.Description, c.Language, c.ImageKey, toUnix(c.UpdatedAt), c.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update channel: %w", mapErr(err))
	}
	return requireAffected(res)
}

func (s *Store) ChannelByID(ctx context.Context, id string) (*model.Channel, error) {
	return s.channelWhere(ctx, "id = ?", id)
}

func (s *Store) ChannelBySlug(ctx context.Context, slug string) (*model.Channel, error) {
	return s.channelWhere(ctx, "slug = ?", slug)
}

const channelCols = `id, user_id, slug, title, description, language, image_key, created_at, updated_at`

func scanChannel(sc interface{ Scan(...any) error }) (*model.Channel, error) {
	var (
		c                model.Channel
		created, updated int64
	)
	if err := sc.Scan(&c.ID, &c.UserID, &c.Slug, &c.Title, &c.Description, &c.Language,
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

const episodeCols = `id, channel_id, slug, title, description, media_key, media_mime, media_kind,
	media_bytes, duration_secs, status, transcript_status, published_at, created_at, updated_at`

func (s *Store) CreateEpisode(ctx context.Context, e *model.Episode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO episodes (id, channel_id, slug, title, description, media_key, media_mime, media_kind,
			media_bytes, duration_secs, status, transcript_status, published_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ChannelID, e.Slug, e.Title, e.Description, e.MediaKey, e.MediaMIME, string(e.MediaKind),
		e.MediaBytes, e.DurationSecs, string(e.Status), string(e.TranscriptStatus),
		toUnixPtr(e.PublishedAt), toUnix(e.CreatedAt), toUnix(e.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create episode: %w", mapErr(err))
	}
	return nil
}

func (s *Store) UpdateEpisode(ctx context.Context, e *model.Episode) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET slug = ?, title = ?, description = ?, media_key = ?, media_mime = ?, media_kind = ?,
			media_bytes = ?, duration_secs = ?, status = ?, transcript_status = ?, published_at = ?, updated_at = ?
		 WHERE id = ?`,
		e.Slug, e.Title, e.Description, e.MediaKey, e.MediaMIME, string(e.MediaKind), e.MediaBytes, e.DurationSecs,
		string(e.Status), string(e.TranscriptStatus), toUnixPtr(e.PublishedAt), toUnix(e.UpdatedAt), e.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update episode: %w", mapErr(err))
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

func (s *Store) EpisodeBySlug(ctx context.Context, channelID, slug string) (*model.Episode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+episodeCols+` FROM episodes WHERE channel_id = ? AND slug = ?`, channelID, slug)
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
	for i, c := range conds {
		if i == 0 {
			query += " WHERE "
		} else {
			query += " AND "
		}
		query += c
	}
	// Newest published first; drafts (NULL published_at) sort by creation.
	query += " ORDER BY COALESCE(published_at, created_at) DESC, created_at DESC"
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
	if err := sc.Scan(&e.ID, &e.ChannelID, &e.Slug, &e.Title, &e.Description, &e.MediaKey,
		&e.MediaMIME, &mkind, &e.MediaBytes, &e.DurationSecs, &status, &tstatus, &published,
		&created, &updated); err != nil {
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
