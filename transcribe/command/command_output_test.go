//go:build unix

package command_test

import (
	"os"
	"strings"
	"testing"

	"github.com/castletfm/castlet/transcribe"
	"github.com/castletfm/castlet/transcribe/command"
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
