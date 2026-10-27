//go:build linux

package tools

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// processGroupMemberCount returns the number of live processes currently
// visible in the payload process group. It is reporting only: signal delivery
// still targets the group directly and does not depend on this observation.
func processGroupMemberCount(command *exec.Cmd) int {
	live, _ := scanProcessGroup(command)
	return live
}

// processGroupZombies returns the pids of a payload group's members when a scan
// finds only zombies, or nil when it sees a live member or no member at all.
func processGroupZombies(command *exec.Cmd) []int {
	live, zombies := scanProcessGroup(command)
	if live > 0 {
		return nil
	}
	return zombies
}

// scanProcessGroup reads /proc in directory order, so repeated scans list the
// same zombies identically.
func scanProcessGroup(command *exec.Cmd) (live int, zombies []int) {
	if command == nil || command.Process == nil || command.Process.Pid <= 1 {
		return 0, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		_, fieldsText, found := strings.CutLast(string(stat), ")")
		if !found {
			continue
		}
		fields := strings.Fields(fieldsText)
		if len(fields) < 3 || fields[0] == "X" {
			continue
		}
		if group, err := strconv.Atoi(fields[2]); err != nil || group != command.Process.Pid {
			continue
		}
		if fields[0] == "Z" && !otherThreadsRemain(entry.Name()) {
			zombies = append(zombies, pid)
		} else {
			live++
		}
	}
	return live, zombies
}

// otherThreadsRemain reports whether a zombie-state process still has running
// threads: a leader that calls pthread_exit stays a zombie until they finish.
// An unreadable task list counts as running so that it cannot settle a group.
func otherThreadsRemain(pid string) bool {
	tasks, err := os.ReadDir("/proc/" + pid + "/task")
	return err != nil || len(tasks) > 1
}
