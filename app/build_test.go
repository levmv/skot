package app

import (
	"runtime/debug"
	"testing"
)

func TestBuildSnapshotUsesAvailableProvenance(t *testing.T) {
	snapshot := buildSnapshot(" v1.2.3 ", []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: " abc123 "},
		{Key: "vcs.modified", Value: "false"},
	})
	if snapshot.Version != "v1.2.3" || snapshot.Revision != "abc123" || snapshot.Modified == nil || *snapshot.Modified {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestBuildSnapshotLeavesUnavailableVCSStatusUnknown(t *testing.T) {
	snapshot := buildSnapshot("dev", []debug.BuildSetting{{Key: "vcs.modified", Value: "unknown"}})
	if snapshot.Version != "dev" || snapshot.Revision != "" || snapshot.Modified != nil {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
