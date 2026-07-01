//go:build unix

package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/castletfm/castlet/transcribe"
	"github.com/castletfm/castlet/transcribe/command"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
}

// TestStdoutCap verifies that output exceeding the stdout cap fails the job with
// a clear error instead of growing unbounded or silently truncating into a
// mis-parsed transcript.
func TestStdoutCap(t *testing.T) {
	requireSh(t)

	// Emit well over the stdout cap (64 MiB) of bytes.
	tr := command.New("/bin/sh", command.WithArgs("-c", "head -c 70000000 /dev/zero"))
	_, err := tr.Transcribe(t.Context(), transcribe.Input{
		Audio:    strings.NewReader("x"),
		Filename: "a.wav",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "stdout")
}

// TestTempDir verifies that the audio is staged in the directory configured by
// WithTempDir (defaulting to os.TempDir when unset) and cleaned up afterward.
// The wrapper records the audio path ($1) so the test can inspect its directory.
func TestTempDir(t *testing.T) {
	requireSh(t) // records the staged audio path passed as {{audio}}

	run := func(t *testing.T, wantDir string, opts ...command.Option) {
		t.Helper()
		rec := filepath.Join(t.TempDir(), "audio-path")
		// $1 is the staged audio file; record it, then emit valid JSON.
		args := append([]command.Option{
			command.WithArgs("-c", `printf '%s' "$1" > `+rec+`; printf '{"language":"en","segments":[]}'`, "sh", "{{audio}}"),
		}, opts...)
		tr := command.New("/bin/sh", args...)
		_, err := tr.Transcribe(t.Context(), transcribe.Input{Audio: strings.NewReader("x"), Filename: "a.wav"})
		require.NoError(t, err)

		staged, err := os.ReadFile(rec)
		require.NoError(t, err)
		assert.Equal(t, wantDir, filepath.Dir(string(staged)), "audio must be staged in the expected dir")
		_, statErr := os.Stat(string(staged))
		assert.True(t, os.IsNotExist(statErr), "staged audio must be removed after Transcribe")
	}

	t.Run("configured", func(t *testing.T) {
		dir := t.TempDir()
		run(t, dir, command.WithTempDir(dir))
	})

	t.Run("default_is_os_tempdir", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir) // os.TempDir honors TMPDIR on unix
		require.Equal(t, dir, os.TempDir())
		run(t, dir)
	})
}

// TestNormalOutput confirms that output within the caps is parsed as before.
func TestNormalOutput(t *testing.T) {
	requireSh(t)

	const out = `{"language":"en","segments":[{"start":0,"end":1.5,"text":" hello world "}]}`
	tr := command.New("/bin/sh", command.WithArgs("-c", "printf '%s' '"+out+"'"))
	res, err := tr.Transcribe(t.Context(), transcribe.Input{
		Audio:    strings.NewReader("x"),
		Filename: "a.wav",
	})
	require.NoError(t, err)
	require.Equal(t, "en", res.Language)
	require.Len(t, res.Segments, 1)
	require.Equal(t, "hello world", res.Segments[0].Text)
}
