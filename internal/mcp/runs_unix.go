//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// detachProcessGroup puts the run subprocess in its own process group so a
// SIGTERM/SIGINT delivered to the MCP server's group does not propagate to
// active runs. Without this, the §8.4 "runs survive MCP server restarts"
// invariant breaks under shell Ctrl-C and systemd shutdowns even though
// context.WithoutCancel already isolates from in-process cancellation.
func detachProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
