package tools

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if RunBoundaryChildIfRequested() {
		return
	}
	if RunJobWorkerIfRequested() {
		return
	}
	os.Exit(m.Run())
}
