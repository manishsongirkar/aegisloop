//go:build windows

package runner

import (
	"os/exec"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }
