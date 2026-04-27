//go:build windows

package mcp

import (
	"os/exec"
	"syscall"
)

// detachProcessGroup gives the run subprocess its own console process group
// so Ctrl-C / Ctrl-Break delivered to the MCP server's console does not
// propagate to active runs. This mirrors the Setpgid call on Unix; on
// Windows the equivalent is CREATE_NEW_PROCESS_GROUP.
func detachProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}
