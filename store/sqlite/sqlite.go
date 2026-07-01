// Package sqlite is the default store.Store implementation, backed by
// modernc.org/sqlite (a pure-Go, cgo-free SQLite driver). It targets a single
// node: the connection pool is capped at one writer and the database runs in
// WAL mode with a busy timeout, which is ample for a standalone podcast server
// and avoids "database is locked" errors without cgo.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/castletfm/castlet/store"
	"github.com/lestrrat-go/option/v3"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed schema.sql
var schema string

// Store is the SQLite-backed store.Store implementation.
type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

// Option configures Open.
type Option = option.Interface

type identMaxOpenConns struct{}

// WithMaxOpenConns overrides the maximum number of open connections. The
// default is 1, the safe choice for a single SQLite file under concurrent
// writes. Raise it only with a backend that tolerates concurrent writers.
func WithMaxOpenConns(n int) Option { return option.New(identMaxOpenConns{}, n) }

// Open opens (creating if necessary) the SQLite database at path and returns a
// Store. Call Migrate before use. path is a filesystem path; ":memory:" is not
// recommended because the single shared connection is the only one that sees
// an in-memory database.
func Open(path string, options ...Option) (*Store, error) {
	maxOpen := 1
	for _, o := range options {
		switch o.Ident().(type) {
		case identMaxOpenConns:
			maxOpen = option.MustGet[int](o)
		}
	}

	dsn := dsnFor(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpen)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

// dsnFor builds a modernc.org/sqlite DSN with pragmas applied per connection:
// WAL for concurrent readers, a busy timeout so brief write contention waits
// rather than errors, and foreign-key enforcement for our ON DELETE CASCADEs.
func dsnFor(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	return "file:" + path + "?" + q.Encode()
}

// Migrate applies the schema. It is idempotent and also upgrades databases
// created by earlier versions in place.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	// Backfill columns added after the original schema for pre-existing
	// databases. CREATE TABLE IF NOT EXISTS above is a no-op on them, so the
	// new columns must be added with ALTER; a duplicate-column error means the
	// column already exists and is ignored.
	for _, ddl := range []string{
		`ALTER TABLE users ADD COLUMN oidc_issuer TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN oidc_subject TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE episodes ADD COLUMN language TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE episodes ADD COLUMN position INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("sqlite: migrate alter: %w", err)
		}
	}
	// Created after the columns exist so an in-place upgrade does not reference
	// a missing column.
	if _, err := s.db.ExecContext(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc ON users(oidc_issuer, oidc_subject) WHERE oidc_subject <> ''`); err != nil {
		return fmt.Errorf("sqlite: migrate index: %w", err)
	}
	return nil
}

func isDuplicateColumn(err error) bool {
	return strings.Contains(err.Error(), "duplicate column name")
}

// Ping verifies the database is reachable, backing the readiness probe. It uses
// the driver's connection check rather than a query so it stays cheap even when
// polled frequently by a load balancer or process supervisor.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	return nil
}

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// --- time helpers -----------------------------------------------------------

func toUnix(t time.Time) int64 { return t.UTC().Unix() }

func fromUnix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func toUnixPtr(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toUnix(*t), Valid: true}
}

func fromUnixPtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromUnix(n.Int64)
	return &t
}

// mapErr translates driver-specific errors into store sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return store.ErrConflict
		}
	}
	return err
}
