//go:build unix

package localfs_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/castletfm/castlet/blob/localfs"
	"github.com/stretchr/testify/require"
)

// TestNewAndPutLockDownPermissions verifies that the media root directory and
// stored object files are owner-only, so a default umask can't leave media
// group/world accessible on a shared host. Unix-guarded: Windows permission
// semantics differ.
func TestNewAndPutLockDownPermissions(t *testing.T) {
	// Force a permissive umask so the assertions catch a regression where the
	// tightening relies on the caller's umask rather than an explicit mode.
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	dir := filepath.Join(t.TempDir(), "media")
	s, err := localfs.New(dir)
	require.NoError(t, err)

	di, err := os.Stat(dir)
	require.NoError(t, err)
	require.Zero(t, di.Mode().Perm()&0o077, "media root must not be group/world accessible, got %v", di.Mode().Perm())

	_, err = s.Put(t.Context(), "abc", strings.NewReader("hello"))
	require.NoError(t, err)

	fi, err := os.Stat(filepath.Join(dir, "abc"))
	require.NoError(t, err)
	require.Zero(t, fi.Mode().Perm()&0o077, "object file must not be group/world accessible, got %v", fi.Mode().Perm())
}
