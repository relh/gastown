//go:build windows

package daemon

import (
	"os/exec"
)

// setProcAttr is a no-op on Windows since Setpgid is not available.
// Windows process detachment would need CREATE_NEW_PROCESS_GROUP or similar.
func setProcAttr(cmd *exec.Cmd) {
	// No-op on Windows
}
