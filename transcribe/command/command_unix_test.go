//go:build unix

package command_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/castletfm/castlet/transcribe"
	"github.com/castletfm/castlet/transcribe/command"
	"github.com/stretchr/testify/require"
)

// TestKillsProcessGroup verifies that cancelling the job context kills not just
// the wrapper command but a grandchild it backgrounds (as a docker/whisper
// wrapper would). Without the process-group kill, exec.CommandContext signals
// only the direct child and the grandchild leaks.
func TestKillsProcessGroup(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "wrap.sh")
	// The wrapper backgrounds a long-lived grandchild that records its PID, then
	// waits on it — mimicking a wrapper that shells out to another process. The
	// sleep is far longer than the assertion window so, without the group kill,
	// the grandchild would still be alive when we check.
	body := "#!/bin/sh\n" +
		"sh -c 'echo $$ > \"" + pidFile + "\"; sleep 120' &\n" +
		"wait\n"
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))

	tr := command.New("/bin/sh", command.WithArgs(script))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tr.Transcribe(ctx, transcribe.Input{Audio: strings.NewReader("x"), Filename: "a.wav"})
	}()
	t.Cleanup(func() {
		// Reap the goroutine, but don't hang the suite if the command is somehow
		// still winding down.
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	})

	// Wait for the grandchild to publish its PID.
	pid := waitPID(t, pidFile)
	require.True(t, alive(pid), "grandchild should be running before cancel")

	cancel()

	// The grandchild must die shortly after the group kill. We deliberately do
	// NOT wait for Transcribe to return first: a leaked grandchild keeps the
	// command's stdout pipe open, so cmd.Wait (and thus Transcribe) would block
	// until the grandchild's own sleep ends — exactly the leak this guards
	// against. Polling liveness directly makes the missing-fix case fail fast.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d still alive after context cancel", pid)
}

func waitPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				pid, perr := strconv.Atoi(s)
				if perr == nil {
					return pid
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("grandchild did not report its pid in time")
	return 0
}

// alive reports whether pid names a live (non-reaped) process.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
