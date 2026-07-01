//go:build unix

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/castletfm/castlet/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStagingDirHardensExistingDir proves stagingDir corrects a pre-existing,
// world-/group-readable staging directory to owner-only. MkdirAll only applies
// its mode when it creates the dir, so a dir left from a prior run (or
// pre-created) would otherwise leak private staged media.
func TestStagingDirHardensExistingDir(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "tmp")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// umask may already have stripped group/other bits; force the loose mode.
	require.NoError(t, os.Chmod(dir, 0o755))

	got, err := stagingDir(&config.Config{DataDir: data})
	require.NoError(t, err)
	require.Equal(t, dir, got)

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Zero(t, fi.Mode().Perm()&0o077, "staging dir must be owner-only after hardening")
}

// TestStagingDirCreatesOwnerOnly proves a freshly created staging dir is
// owner-only.
func TestStagingDirCreatesOwnerOnly(t *testing.T) {
	data := t.TempDir()
	dir, err := stagingDir(&config.Config{DataDir: data})
	require.NoError(t, err)

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Zero(t, fi.Mode().Perm()&0o077, "new staging dir must be owner-only")
}

// TestStagingDirRejectsSymlink proves stagingDir refuses to stage into a
// symlinked path, which could point staging at an unexpected or attacker-chosen
// location.
func TestStagingDirRejectsSymlink(t *testing.T) {
	data := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, filepath.Join(data, "tmp")))

	_, err := stagingDir(&config.Config{DataDir: data})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
}
