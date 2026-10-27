//go:build darwin || linux

package tools

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func configureProcessSession(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processExited reports whether a started process has exited and been reaped.
func processExited(command *exec.Cmd) bool {
	if command == nil || command.Process == nil {
		return false
	}
	return errors.Is(syscall.Kill(command.Process.Pid, 0), syscall.ESRCH)
}

// processGroupExists asks the kernel whether a payload group still has members.
// Unlike a /proc scan the check is atomic, so a member that forks and exits
// cannot hide its child. Zombies count too: when init does not reap orphans,
// they hold the job until its timeout instead of letting live work escape.
func processGroupExists(command *exec.Cmd) bool {
	if command == nil || command.Process == nil || command.Process.Pid <= 1 {
		return false
	}
	return !errors.Is(syscall.Kill(-command.Process.Pid, 0), syscall.ESRCH)
}

func killProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
