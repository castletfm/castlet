//go:build !unix

package command

import "os/exec"

// setProcessGroup is a no-op on platforms without POSIX process groups, so the
// package still builds everywhere (CI runs only on unix). exec.CommandContext's
// default direct-child kill applies there.
func setProcessGroup(cmd *exec.Cmd) {}
