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

	// --password-file wins over --password.
	got, err := resolvePassword(path, "from-flag", nil, os.Stderr)
	require.NoError(t, err)
	assert.Equal(t, "from-file", got)

	// With no file and a non-terminal stdin (nil), the --password flag is used.
	got, err = resolvePassword("", "from-flag", nil, os.Stderr)
	require.NoError(t, err)
	assert.Equal(t, "from-flag", got)

	// A missing password file surfaces an error.
	_, err = resolvePassword(filepath.Join(t.TempDir(), "nope"), "from-flag", nil, os.Stderr)
	assert.Error(t, err)
}
