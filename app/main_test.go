package app

import (
	"os"
	"testing"

	workspacetools "github.com/levmv/skot/tools"
)

func TestMain(m *testing.M) {
	if workspacetools.RunBoundaryChildIfRequested() {
		return
	}
	if workspacetools.RunJobWorkerIfRequested() {
		return
	}
	cache, err := os.MkdirTemp("", "skot-app-test-cache-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("XDG_CACHE_HOME", cache)
	code := m.Run()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}
