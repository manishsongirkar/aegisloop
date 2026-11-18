//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup puts the child in its own process group and makes context
// cancellation kill the whole group (test runners spawn workers).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
}
