package app

import (
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/levmv/skot/agent"
)

func currentBuildSnapshot(version string) agent.BuildSnapshot {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return buildSnapshot(version, settings)
}

func buildSnapshot(version string, settings []debug.BuildSetting) agent.BuildSnapshot {
	snapshot := agent.BuildSnapshot{Version: strings.TrimSpace(version)}
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			snapshot.Revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified, err := strconv.ParseBool(setting.Value)
			if err == nil {
				snapshot.Modified = &modified
			}
		}
	}
	return snapshot
}
