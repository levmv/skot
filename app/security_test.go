package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workspacetools "github.com/levmv/skot/tools"
)

func TestSecuritySummaryMakesEffectiveBoundaryVisible(t *testing.T) {
	host := securityState{
		RequestedPolicy: workspacetools.SandboxAuto,
		EffectivePolicy: workspacetools.SandboxWorkspace,
		Backend:         "landlock",
	}
	if got := host.Summary(); got != "sandbox: workspace (auto)" {
		t.Fatalf("host summary = %q", got)
	}
	container := securityState{
		RequestedPolicy: workspacetools.SandboxAuto,
		EffectivePolicy: workspacetools.SandboxMasked,
		Backend:         "landlock",
		Container:       "docker",
	}
	if got := container.Summary(); got != "sandbox: masked (auto, docker)" {
		t.Fatalf("container summary = %q", got)
	}
}

func TestAutoResolvesToConcretePolicy(t *testing.T) {
	if got := resolveSandboxPolicy(workspacetools.SandboxAuto, ""); got != workspacetools.SandboxWorkspace {
		t.Fatalf("host auto = %q", got)
	}
	if got := resolveSandboxPolicy(workspacetools.SandboxAuto, "docker"); got != workspacetools.SandboxMasked {
		t.Fatalf("container auto = %q", got)
	}
	for _, policy := range []string{workspacetools.SandboxWorkspace, workspacetools.SandboxMasked, workspacetools.SandboxOff} {
		if got := resolveSandboxPolicy(policy, "docker"); got != policy {
			t.Fatalf("explicit %q = %q", policy, got)
		}
	}
}

func TestSandboxFailsClosedAfterFailedProbe(t *testing.T) {
	state := securityState{EffectivePolicy: workspacetools.SandboxWorkspace, Failure: "unavailable"}
	if err := validateSecurity(state); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("validateSecurity() error = %v", err)
	}
	state.Backend = "landlock"
	if err := validateSecurity(state); err != nil {
		t.Fatalf("active sandbox rejected: %v", err)
	}
}

func TestAutoSandboxRejectsFailedAvailableBackend(t *testing.T) {
	state := securityState{
		RequestedPolicy: workspacetools.SandboxAuto,
		EffectivePolicy: workspacetools.SandboxWorkspace,
		Failure:         "workspace probe remained readable",
	}
	err := validateSecurity(state)
	if err == nil || !strings.Contains(err.Error(), state.Failure) {
		t.Fatalf("validateSecurity() error = %v", err)
	}
}

func TestSandboxOffMustBeExplicit(t *testing.T) {
	state := securityState{
		RequestedPolicy: workspacetools.SandboxAuto,
		EffectivePolicy: workspacetools.SandboxOff,
	}
	if err := validateSecurity(state); err == nil || !strings.Contains(err.Error(), "explicitly") {
		t.Fatalf("implicit off error = %v", err)
	}
	state.RequestedPolicy = workspacetools.SandboxOff
	if err := validateSecurity(state); err != nil {
		t.Fatalf("explicit off rejected: %v", err)
	}
}

func TestSandboxProbeRejectsSkotHomeInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	home := root + "/.skot"
	toolHome := workspacetools.WorkspaceToolHome(t.TempDir(), root)
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.SandboxWorkspace, root, home, toolHome)
	if state.Backend != "" || !strings.Contains(state.Failure, "Skot home") || !strings.Contains(state.Failure, "-home") {
		t.Fatalf("security state = %#v", state)
	}
}

func TestOpenExplainsExplicitStartupOptOutAfterSandboxFailure(t *testing.T) {
	root := t.TempDir()
	_, err := Open(context.Background(), Config{
		Home: filepath.Join(root, ".skot"), Root: root,
		Sandbox: workspacetools.SandboxWorkspace, SandboxExplicit: true,
	})
	if err == nil || !strings.Contains(err.Error(), "-sandbox off") {
		t.Fatalf("startup error = %v", err)
	}
}

func TestCanonicalSecurityPathResolvesMissingPathBelowSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-cache")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "cache-alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	got := canonicalSecurityPath(filepath.Join(alias, "skot", "tool-home"))
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedReal, "skot", "tool-home")
	if got != want {
		t.Fatalf("canonical path = %q; want %q", got, want)
	}
}

func TestMaskedPassesRealSandboxProbe(t *testing.T) {
	root, home, toolRoot := t.TempDir(), t.TempDir(), t.TempDir()
	toolHome := workspacetools.WorkspaceToolHome(toolRoot, root)
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.SandboxMasked, root, home, toolHome)
	if state.Backend == "" || state.EffectivePolicy != workspacetools.SandboxMasked || state.Failure != "" {
		t.Fatalf("security state = %#v", state)
	}
}

func TestFullSandboxPassesRealSandboxProbe(t *testing.T) {
	root, home, toolRoot := t.TempDir(), t.TempDir(), t.TempDir()
	toolHome := workspacetools.WorkspaceToolHome(toolRoot, root)
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.SandboxWorkspace, root, home, toolHome)
	if state.Backend == "" || state.EffectivePolicy != workspacetools.SandboxWorkspace || state.Failure != "" {
		t.Fatalf("security state = %#v", state)
	}
}

func TestMaskedIgnoresUnusedToolHomeOverlap(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	toolHome := filepath.Join(root, ".cache", "skot", "tool-home")
	state := buildSecurityStateWithToolHome(context.Background(), workspacetools.SandboxMasked, root, home, toolHome)
	if state.Backend == "" || state.Failure != "" {
		t.Fatalf("masked depends on unused tool home: %#v", state)
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
