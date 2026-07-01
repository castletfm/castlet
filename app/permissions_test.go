//go:build unix

package app

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/castletfm/castlet/config"
	"github.com/stretchr/testify/require"
)

// TestOpenStoreLocksDownDataDir verifies that the data directory and the sqlite
// database file it creates are owner-only, so a default umask can't leave the
// password hashes / emails / OIDC subjects world-readable to other local users
// on a self-hosted box. Unix-guarded: Windows permission semantics differ.
func TestOpenStoreLocksDownDataDir(t *testing.T) {
	// Force a permissive umask so the assertions catch a regression where the
	// tightening relies on the caller's umask rather than an explicit chmod.
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	// A fresh, not-yet-existing directory so OpenStore's MkdirAll actually
	// creates it (an existing dir is left untouched, matching the real
	// first-run case where the data dir does not exist yet).
	dir := filepath.Join(t.TempDir(), "data")
	cfg := &config.Config{DataDir: dir}

	st, err := OpenStore(cfg)
	require.NoError(t, err)
	defer st.Close()
	require.NoError(t, st.Migrate(t.Context()))

	di, err := os.Stat(dir)
	require.NoError(t, err)
	require.Zero(t, di.Mode().Perm()&0o077, "data dir must not be group/world accessible, got %v", di.Mode().Perm())

	dbi, err := os.Stat(filepath.Join(dir, "castlet.db"))
	require.NoError(t, err)
	require.Zero(t, dbi.Mode().Perm()&0o077, "castlet.db must not be group/world accessible, got %v", dbi.Mode().Perm())

	// The WAL sidecars, when present, must be locked down too.
	for _, sidecar := range []string{"castlet.db-wal", "castlet.db-shm"} {
		si, err := os.Stat(filepath.Join(dir, sidecar))
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		require.Zero(t, si.Mode().Perm()&0o077, "%s must not be group/world accessible, got %v", sidecar, si.Mode().Perm())
	}
}
