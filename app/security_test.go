package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levmv/skot/internal/canonicalpath"
	workspacetools "github.com/levmv/skot/tools"
)

func TestScopeSummaryMakesBoundaryVisible(t *testing.T) {
	workspace := securityState{
		Scope:           workspacetools.ScopeWorkspace,
		ProtectedPaths:  []string{"/private", "/secrets"},
		Backend:         "landlock",
		BackendRequired: true,
	}
	if got := workspace.Summary(); got != "scope: workspace · protected paths: /private, /secrets" {
		t.Fatalf("workspace summary = %q", got)
	}
}

func TestScopeFailsClosedAfterFailedProbe(t *testing.T) {
	state := securityState{Scope: workspacetools.ScopeWorkspace, Failure: "unavailable", BackendRequired: true}
	if err := validateSecurity(state); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("validateSecurity() error = %v", err)
	}
	state.Backend = "landlock"
	if err := validateSecurity(state); err != nil {
		t.Fatalf("active boundary rejected: %v", err)
	}
}

func TestMachineWithoutProtectedPathsNeedsNoBackend(t *testing.T) {
	state := securityState{
		Scope: workspacetools.ScopeMachine,
	}
	if err := validateSecurity(state); err != nil {
		t.Fatalf("machine without restrictions rejected: %v", err)
	}
	state.ProtectedPaths = []string{"/private"}
	state.BackendRequired = true
	state.Failure = "backend unavailable"
	if err := validateSecurity(state); err == nil || !strings.Contains(err.Error(), state.Failure) {
		t.Fatalf("machine protected paths did not fail closed: %v", err)
	}
}

func TestMachineScopeSummaryOmitsImpliedBoundaryDetail(t *testing.T) {
	state := securityState{
		Scope: workspacetools.ScopeMachine,
	}
	if got := state.Summary(); got != "scope: machine" {
		t.Fatalf("summary = %q", got)
	}
}

func TestScopeProbeRejectsToolHomeContainingWorkspace(t *testing.T) {
	toolHome := t.TempDir()
	root := filepath.Join(toolHome, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	state := buildSecurityStateForTest(
		context.Background(), workspacetools.ScopeWorkspace, root, toolHome, nil,
	)
	if state.Backend != "" || !strings.Contains(state.Failure, "tool home must not contain") {
		t.Fatalf("security state = %#v", state)
	}
}

func TestWorkspaceProtectedProbeUsesWorkspaceCarveout(t *testing.T) {
	root := t.TempDir()
	boundary := workspacetools.Boundary{
		Scope: workspacetools.ScopeWorkspace, Workspace: root, ToolHome: t.TempDir(),
		ProtectedPaths: []string{filepath.Join(t.TempDir(), "secret")},
	}
	probe, probeBoundary, err := createBoundaryProbe(boundary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	})
	if !canonicalpath.Contains(root, probe.Name()) {
		t.Fatalf("workspace carve-out probe was created outside the workspace: %s", probe.Name())
	}
	if len(probeBoundary.ProtectedPaths) == 0 || probeBoundary.ProtectedPaths[len(probeBoundary.ProtectedPaths)-1] != probe.Name() {
		t.Fatalf("probe path is not protected: %#v", probeBoundary.ProtectedPaths)
	}
}

func TestWorkspaceFailureNamesMachineEscapeHatch(t *testing.T) {
	err := validateSecurity(securityState{
		Scope:           workspacetools.ScopeWorkspace,
		Failure:         "no platform filesystem boundary is available",
		BackendRequired: true,
	})
	if err == nil || !strings.Contains(err.Error(), "-scope=machine") || !strings.Contains(err.Error(), "SK_SCOPE=machine") {
		t.Fatalf("scope error = %v", err)
	}
}

func TestScopeWorkspaceNoticeDescribesLandlockRootLimitation(t *testing.T) {
	root := t.TempDir()
	protected := filepath.Join(root, "private")
	state := securityState{Scope: workspacetools.ScopeWorkspace, ProtectedPaths: []string{protected}, Backend: "landlock"}
	if got := protectedPathsNotice(state, root); got == "" {
		t.Fatalf("notice = %q", got)
	}
	state.Backend = "seatbelt"
	if got := protectedPathsNotice(state, root); got != "" {
		t.Fatalf("seatbelt notice = %q", got)
	}
	state.Backend = "landlock"
	state.ProtectedPaths = []string{t.TempDir()}
	if got := protectedPathsNotice(state, root); got != "" {
		t.Fatalf("disjoint protected-path notice = %q", got)
	}
}

func TestMachineProtectedPathNeedsBuiltInLSAndExplainsBackend(t *testing.T) {
	protected := filepath.Join(t.TempDir(), "secret")
	if !landlockProtectedPathsNeedBuiltInLS("landlock", []string{protected}) {
		t.Fatal("external protected path did not retain built-in ls")
	}
	if landlockProtectedPathsNeedBuiltInLS("seatbelt", []string{protected}) || landlockProtectedPathsNeedBuiltInLS("landlock", nil) {
		t.Fatal("built-in ls predicate ignored backend or empty paths")
	}
	state := securityState{
		Scope: workspacetools.ScopeMachine, ProtectedPaths: []string{protected}, Backend: "landlock",
		BackendRequired: true,
	}
	if notice := protectedPathsNotice(state, t.TempDir()); !strings.Contains(notice, "ancestor directories") {
		t.Fatalf("machine notice = %q", notice)
	}
	state.Backend = ""
	state.Failure = "backend unavailable"
	if err := validateSecurity(state); err == nil || !strings.Contains(err.Error(), "protected_paths require") {
		t.Fatalf("machine backend error = %v", err)
	}
}

func TestMachineWithoutProtectedPathsBypassesBackend(t *testing.T) {
	root, toolRoot := t.TempDir(), t.TempDir()
	toolHome := workspacetools.WorkspaceToolHome(toolRoot, root)
	state := buildSecurityStateForTest(context.Background(), workspacetools.ScopeMachine, root, toolHome, nil)
	if state.Backend != "" || state.Scope != workspacetools.ScopeMachine || state.Failure != "" || state.BackendRequired {
		t.Fatalf("security state = %#v", state)
	}
}

func TestWorkspacePassesRealBoundaryProbe(t *testing.T) {
	root, toolRoot := t.TempDir(), t.TempDir()
	toolHome := workspacetools.WorkspaceToolHome(toolRoot, root)
	state := buildSecurityStateForTest(context.Background(), workspacetools.ScopeWorkspace, root, toolHome, nil)
	if state.Backend == "" || state.Scope != workspacetools.ScopeWorkspace || state.Failure != "" || !state.BackendRequired {
		t.Fatalf("security state = %#v", state)
	}
}

func TestMachineWithoutProtectedPathsIgnoresUnusedToolHomeOverlap(t *testing.T) {
	toolHome := t.TempDir()
	root := filepath.Join(toolHome, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	state := buildSecurityStateForTest(context.Background(), workspacetools.ScopeMachine, root, toolHome, nil)
	if state.Backend != "" || state.Failure != "" || state.BackendRequired {
		t.Fatalf("machine depends on unused tool home: %#v", state)
	}
}

func buildSecurityStateForTest(ctx context.Context, scope workspacetools.Scope, root, toolHome string, protectedPaths []string) securityState {
	return buildProcessSecurityState(ctx, newSecurityState(scope, protectedPaths), root, toolHome)
}
