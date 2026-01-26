//go:build !windows

package daemon

import (
	"os/exec"
	"syscall"
)

// setProcAttr sets Unix-specific process attributes for background server processes.
// This detaches the process from the parent's process group so it survives daemon restart.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
