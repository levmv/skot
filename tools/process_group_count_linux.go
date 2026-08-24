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
	if command == nil || command.Process == nil || command.Process.Pid <= 1 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
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
		if len(fields) < 3 || fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		group, err := strconv.Atoi(fields[2])
		if err == nil && group == command.Process.Pid {
			count++
		}
	}
	return count
}
