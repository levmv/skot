//go:build darwin

package tools

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

// processGroupMemberCount is observational only; failure to inspect a group
// must never prevent the normal kill path.
func processGroupMemberCount(command *exec.Cmd) int {
	if command == nil || command.Process == nil || command.Process.Pid <= 1 {
		return 0
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", command.Process.Pid)
	if err != nil {
		return 0
	}
	return len(processes)
}
