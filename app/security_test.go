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

func TestScopeSummaryMakesEffectiveBoundaryVisible(t *testing.T) {
	host := securityState{
		RequestedScope: workspacetools.ScopeAuto,
		EffectiveScope: workspacetools.ScopeWorkspace,
		Backend:        "landlock",
	}
	if got := host.Summary(); got != "scope: workspace (auto)" {
		t.Fatalf("host summary = %q", got)
	}
	container := securityState{
		RequestedScope:     workspacetools.ScopeAuto,
		EffectiveScope:     workspacetools.ScopeMachine,
		Backend:            "landlock",
		Container:          "docker",
		ProtectedPathCount: 2,
		BackendRequired:    true,
	}
	if got := container.Summary(); got != "scope: machine (auto, docker) · protected paths: 2" {
		t.Fatalf("container summary = %q", got)
	}
}

func TestAutoResolvesToConcreteScope(t *testing.T) {
	if got := resolveScope(workspacetools.ScopeAuto, ""); got != workspacetools.ScopeWorkspace {
		t.Fatalf("host auto = %q", got)
	}
	if got := resolveScope(workspacetools.ScopeAuto, "docker"); got != workspacetools.ScopeMachine {
		t.Fatalf("container auto = %q", got)
	}
	for _, scope := range []workspacetools.Scope{workspacetools.ScopeWorkspace, workspacetools.ScopeMachine} {
		if got := resolveScope(scope, "docker"); got != scope {
			t.Fatalf("explicit %q = %q", scope, got)
		}
	}
}

func TestScopeFailsClosedAfterFailedProbe(t *testing.T) {
	state := securityState{EffectiveScope: workspacetools.ScopeWorkspace, Failure: "unavailable", BackendRequired: true}
	if err := validateSecurity(state); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("validateSecurity() error = %v", err)
	}
	state.Backend = "landlock"
	if err := validateSecurity(state); err != nil {
		t.Fatalf("active boundary rejected: %v", err)
	}
}

func TestAutoScopeRejectsFailedAvailableBackend(t *testing.T) {
	state := securityState{
		RequestedScope:  workspacetools.ScopeAuto,
		EffectiveScope:  workspacetools.ScopeWorkspace,
		Failure:         "workspace probe remained readable",
		BackendRequired: true,
	}
	err := validateSecurity(state)
	if err == nil || !strings.Contains(err.Error(), state.Failure) {
		t.Fatalf("validateSecurity() error = %v", err)
	}
}

func TestMachineWithoutProtectedPathsNeedsNoBackend(t *testing.T) {
	state := securityState{
		RequestedScope: workspacetools.ScopeAuto,
		EffectiveScope: workspacetools.ScopeMachine,
	}
	if err := validateSecurity(state); err != nil {
		t.Fatalf("machine without restrictions rejected: %v", err)
	}
	state.ProtectedPathCount = 1
	state.BackendRequired = true
	state.Failure = "backend unavailable"
	if err := validateSecurity(state); err == nil || !strings.Contains(err.Error(), state.Failure) {
		t.Fatalf("machine protected paths did not fail closed: %v", err)
	}
}

func TestMachineWithoutProtectedPathsReportsNoBoundary(t *testing.T) {
	state := securityState{
		RequestedScope: workspacetools.ScopeMachine,
		EffectiveScope: workspacetools.ScopeMachine,
	}
	if got := state.Summary(); got != "scope: machine · no additional filesystem boundary" {
		t.Fatalf("summary = %q", got)
	}
}

func TestScopeProbeDoesNotDependOnSkotHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".skot")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	toolHome := workspacetools.WorkspaceToolHome(filepath.Join(root, ".cache", "skot", "tool-home"), root)
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.ScopeWorkspace, root, toolHome)
	if state.Backend == "" || state.Failure != "" {
		t.Fatalf("workspace state = %#v", state)
	}
	protected := filepath.Join(t.TempDir(), "secret")
	state = buildSecurityStateWithToolHome(context.Background(), workspacetools.ScopeMachine, root, toolHome, []string{protected})
	if state.Backend == "" || state.Failure != "" || state.ProtectedPathCount != 1 || !state.BackendRequired {
		t.Fatalf("machine state = %#v", state)
	}
}

func TestScopeProbeRejectsToolHomeContainingWorkspace(t *testing.T) {
	toolHome := t.TempDir()
	root := filepath.Join(toolHome, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	state := buildSecurityStateWithToolHome(
		context.Background(), workspacetools.ScopeWorkspace, root, toolHome,
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
		EffectiveScope:  workspacetools.ScopeWorkspace,
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
	state := securityState{EffectiveScope: workspacetools.ScopeWorkspace, Backend: "landlock"}
	if got := protectedPathsNotice(state, root, []string{protected}); got == "" {
		t.Fatalf("notice = %q", got)
	}
	state.Backend = "seatbelt"
	if got := protectedPathsNotice(state, root, []string{protected}); got != "" {
		t.Fatalf("seatbelt notice = %q", got)
	}
	state.Backend = "landlock"
	if got := protectedPathsNotice(state, root, []string{t.TempDir()}); got != "" {
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
		EffectiveScope: workspacetools.ScopeMachine, Backend: "landlock",
		ProtectedPathCount: 1, BackendRequired: true,
	}
	if notice := protectedPathsNotice(state, t.TempDir(), []string{protected}); !strings.Contains(notice, "ancestor directories") {
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
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.ScopeMachine, root, toolHome)
	if state.Backend != "" || state.EffectiveScope != workspacetools.ScopeMachine || state.Failure != "" || state.BackendRequired {
		t.Fatalf("security state = %#v", state)
	}
}

func TestWorkspacePassesRealBoundaryProbe(t *testing.T) {
	root, toolRoot := t.TempDir(), t.TempDir()
	toolHome := workspacetools.WorkspaceToolHome(toolRoot, root)
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.ScopeWorkspace, root, toolHome)
	if state.Backend == "" || state.EffectiveScope != workspacetools.ScopeWorkspace || state.Failure != "" || !state.BackendRequired {
		t.Fatalf("security state = %#v", state)
	}
}

func TestMachineWithoutProtectedPathsIgnoresUnusedToolHomeOverlap(t *testing.T) {
	root := t.TempDir()
	toolHome := filepath.Join(root, ".cache", "skot", "tool-home")
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.ScopeMachine, root, toolHome)
	if state.Backend != "" || state.Failure != "" {
		t.Fatalf("machine depends on unused tool home: %#v", state)
	}
}

func TestTrustedContainerIDAcceptsKnownValuesOnly(t *testing.T) {
	if got := trustedContainerID([]byte("container=podman\x00")); got != "podman" {
		t.Fatalf("podman ID = %q", got)
	}
	if got := trustedContainerID([]byte("container=surprise\x00")); got != "" {
		t.Fatalf("unknown container ID = %q", got)
	}
}

func TestCgroupContainerIDRecognizesRuntimeBoundaries(t *testing.T) {
	id := strings.Repeat("a", 64)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "docker cgroupfs", raw: "5:memory:/docker/" + id, want: "docker"},
		{name: "docker systemd", raw: "0::/system.slice/docker-" + id + ".scope", want: "docker"},
		{name: "rootless podman", raw: "0::/user.slice/user-1000.slice/libpod-" + id + ".scope", want: "podman"},
		{name: "kubernetes", raw: "0::/kubepods.slice/kubepods-burstable.slice/pod.scope", want: "kubernetes"},
		{name: "containerd scope", raw: "0::/system.slice/cri-containerd-" + id + ".scope", want: "containerd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cgroupContainerID([]byte(test.raw)); got != test.want {
				t.Fatalf("cgroupContainerID() = %q; want %q", got, test.want)
			}
		})
	}
}

func TestCgroupContainerIDRejectsHostServicesAndNames(t *testing.T) {
	tests := []string{
		"0::/system.slice/containerd.service",
		"0::/user.slice/docker-compose-project.scope",
		"0::/system.slice/docker-not-a-container.scope",
		"malformed",
	}
	for _, raw := range tests {
		if got := cgroupContainerID([]byte(raw)); got != "" {
			t.Fatalf("cgroupContainerID(%q) = %q", raw, got)
		}
	}
}
