package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadPasswordFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "plain", content: "hunter2", want: "hunter2"},
		{name: "trailing newline", content: "hunter2\n", want: "hunter2"},
		{name: "trailing crlf", content: "hunter2\r\n", want: "hunter2"},
		{name: "empty", content: "", want: ""},
		{name: "internal spaces preserved", content: "a b c\n", want: "a b c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "pw")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))

			got, err := readPasswordFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadPasswordFileMissing(t *testing.T) {
	t.Parallel()
	_, err := readPasswordFile(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.Error(t, err)
}

func TestResolvePassword(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pw")
	require.NoError(t, os.WriteFile(path, []byte("from-file\n"), 0o600))

	// --password-file wins over --password when both are supplied.
	got, err := resolvePassword(path, true, "from-flag", true, nil, os.Stderr)
	require.NoError(t, err)
	assert.Equal(t, "from-file", got)

	// With no file and a non-terminal stdin (nil), the --password flag is used.
	got, err = resolvePassword("", false, "from-flag", true, nil, os.Stderr)
	require.NoError(t, err)
	assert.Equal(t, "from-flag", got)

	// A supplied --password is honored WITHOUT prompting, even when stdin is a
	// real (non-terminal) file. promptPassword would fail on a non-terminal fd,
	// so a successful flag result proves the prompt branch was not taken. This
	// guards the precedence: --password is always used before any TTY prompt.
	in, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = in.Close() })
	got, err = resolvePassword("", false, "from-flag", true, in, os.Stderr)
	require.NoError(t, err)
	assert.Equal(t, "from-flag", got)

	// An explicitly supplied but empty --password is returned verbatim (NOT
	// treated as absent): precedence keys on the flag being supplied, not on its
	// value. The caller's empty-password validation rejects it afterward. If the
	// prompt branch were taken instead, promptPassword would fail on the
	// non-terminal fd, so a nil error with an empty result proves no prompt.
	got, err = resolvePassword("", false, "", true, in, os.Stderr)
	require.NoError(t, err)
	assert.Equal(t, "", got)

	// An explicitly supplied but empty --password-file selects the file source
	// and errors on the empty path; it does NOT fall back to the --password flag.
	_, err = resolvePassword("", true, "from-flag", true, nil, os.Stderr)
	assert.Error(t, err)

	// A supplied but missing password file surfaces an error.
	_, err = resolvePassword(filepath.Join(t.TempDir(), "nope"), true, "from-flag", true, nil, os.Stderr)
	assert.Error(t, err)

	// No password source (no file, no flag, non-terminal stdin) is an error.
	_, err = resolvePassword("", false, "", false, nil, os.Stderr)
	assert.Error(t, err)
}
