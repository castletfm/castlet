//go:build unix

package command

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group and, on context
// cancellation, sends SIGKILL to the whole group (the negative pgid). This
// reaps grandchildren spawned by wrapper scripts, not just the direct child.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the entire process group. Per the os/exec Cancel
		// contract: return nil on a successful kill (os/exec then relies on the
		// kill / WaitDelay to end Wait and surfaces the deadline error correctly);
		// return os.ErrProcessDone only when the group is already gone (ESRCH); and
		// return any other error as-is.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
