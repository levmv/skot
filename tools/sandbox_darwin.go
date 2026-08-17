//go:build darwin

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/levmv/skot/internal/canonicalpath"
)

const sandboxExecPath = "/usr/bin/sandbox-exec"

func runSandboxChildIfRequested() bool { return false }

func sandboxBackend() string { return "seatbelt" }

func sandboxedBashCommand(command, workdir string, boundary Boundary) (*exec.Cmd, error) {
	return sandboxedProgramCommand("/bin/bash", []string{"/bin/bash", "-lc", command}, workdir, boundary, nil)
}

func sandboxedProgramCommand(program string, argv []string, workdir string, boundary Boundary, environment map[string]string) (*exec.Cmd, error) {
	argv = resolvedProgramArgv(program, argv)
	if err := validateConcreteScope(boundary.Scope); err != nil {
		return nil, err
	}
	if !boundary.NeedsBackend() {
		return ambientProgramCommand(program, argv, workdir, environment), nil
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return nil, errors.New("macOS sandbox-exec is unavailable")
	}
	arguments := []string{"-p", seatbeltProfile(boundary)}
	arguments = append(arguments, argv...)
	cmd := exec.Command(sandboxExecPath, arguments...)
	cmd.Dir = workdir
	cmd.Env = mergeToolEnv(sandboxBaseEnv(boundary), environment)
	return cmd, nil
}

func hardenSupervisor() {}

func seatbeltProfile(boundary Boundary) string {
	var profile string
	if boundary.Scope == ScopeMachine {
		profile = "(version 1)\n(allow default)\n"
	} else {
		profile = fmt.Sprintf(fullSeatbeltProfile,
			canonicalpath.Resolve(boundary.Workspace),
			canonicalpath.Resolve(boundary.ToolHome),
			canonicalpath.Resolve(boundary.Workspace),
			canonicalpath.Resolve(boundary.ToolHome),
		)
	}
	for _, path := range boundary.ProtectedPaths {
		path = canonicalpath.Resolve(path)
		profile += fmt.Sprintf("(deny file-read* file-write* (literal %q))\n", path)
		profile += fmt.Sprintf("(deny file-read* file-write* (subpath %q))\n", path)
	}
	return profile
}

const fullSeatbeltProfile = `(version 1)
(deny default)

(allow process-exec)
(allow process-fork)
(allow process-info* (target same-sandbox))
(allow signal (target same-sandbox))

(allow file-read*
  (literal "/")
  (subpath %q)
  (subpath %q)
  (subpath "/Applications")
  (subpath "/Library")
  (subpath "/System")
  (subpath "/bin")
  (subpath "/dev")
  (subpath "/etc")
  (subpath "/nix")
  (subpath "/opt")
  (subpath "/private/etc")
  (subpath "/private/var/db/dyld")
  (subpath "/private/var/db/timezone")
  (subpath "/private/var/select")
  (subpath "/sbin")
  (subpath "/usr"))
(allow file-read-metadata)

(allow file-write*
  (subpath %q)
  (subpath %q)
  (subpath "/dev/fd")
  (literal "/dev/null")
  (literal "/dev/ptmx")
  (literal "/dev/stderr")
  (literal "/dev/stdout")
  (regex #"^/dev/ttys[0-9]*$"))
(allow file-ioctl (regex #"^/dev/tty.*"))
(allow pseudo-tty)

(allow network*)
(allow system-socket)
(allow sysctl-read)
(allow user-preference-read)
(allow ipc-posix-shm)
(allow ipc-posix-sem)
(allow distributed-notification-post)
(allow mach-lookup
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SystemConfiguration.configd")
  (global-name "com.apple.bsd.dirhelper")
  (global-name "com.apple.mDNSResponder")
  (global-name "com.apple.mDNSResponderHelper")
  (global-name "com.apple.networkd")
  (global-name "com.apple.ocspd")
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.sysmond")
  (global-name "com.apple.trustd")
  (global-name "com.apple.trustd.agent"))
`
