package sqlite

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/stretchr/testify/require"
)

// dsnFor must encode the filesystem path so it survives SQLite's URI parser
// intact. A path containing '?', '#', '%', '&', or a space would otherwise be
// reinterpreted as URI syntax and open the wrong database. The produced DSN
// must therefore round-trip back to the exact literal path and still carry the
// three per-connection pragmas.
func TestDSNForRoundTrip(t *testing.T) {
	for _, path := range []string{
		"/var/lib/castlet/castlet.db",
		"relative/dir/castlet.db",
		"/data/weird#dir/castlet.db",
		"/data/q?dir/castlet.db",
		"/data/pct%dir/castlet.db",
		"/data/amp&dir/castlet.db",
		"/data/sp ace/castlet.db",
		"/data/all ?#%&/castlet.db",
	} {
		dsn := dsnFor(path)

		u, err := url.Parse(dsn)
		require.NoErrorf(t, err, "dsn %q must parse", dsn)
		require.Equal(t, "file", u.Scheme)

		// The driver splits pragmas off at the first '?', so no path character
		// may leak into URI syntax. Recovering the path from the parsed DSN must
		// yield exactly the input. A rooted path parses into u.Path (already
		// decoded); a relative one lands in u.Opaque and must be unescaped.
		got := u.Path
		if u.Opaque != "" {
			got, err = url.PathUnescape(u.Opaque)
			require.NoErrorf(t, err, "opaque %q must unescape", u.Opaque)
		}
		require.Equalf(t, path, got, "path must round-trip literally for %q", path)

		q := u.Query()
		require.ElementsMatch(t, []string{
			"journal_mode(WAL)",
			"busy_timeout(5000)",
			"foreign_keys(1)",
		}, q["_pragma"], "pragmas must be preserved for %q", path)
	}
}

// Open must create and use exactly the on-disk file even when the data
// directory contains characters that are significant in a URI. Before the fix
// a path segment like "weird#dir" would be truncated at the '#' (or misparsed
// at '?'/'%'), so the driver opened a different database than the one on disk.
func TestOpenMigrateSpecialPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weird#dir")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dbPath := filepath.Join(dir, "castlet.db")

	s, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, s.Migrate(t.Context()))

	// The database file must live at the literal path, not some URI-mangled
	// variant.
	_, err = os.Stat(dbPath)
	require.NoError(t, err, "database must be created at the literal path")

	// Pragmas must still take effect on the connection.
	var journalMode string
	require.NoError(t, s.db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journalMode))
	require.Equal(t, "wal", journalMode)

	// A user must round-trip through the store backed by this file.
	want := &model.User{ID: "u1", Email: "weird@example.com", DisplayName: "W", PasswordHash: "x", CreatedAt: time.Now()}
	require.NoError(t, s.CreateUser(t.Context(), want))
	got, err := s.UserByID(t.Context(), "u1")
	require.NoError(t, err)
	require.Equal(t, want.Email, got.Email)
}
