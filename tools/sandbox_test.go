package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeScopeValues(t *testing.T) {
	for input, want := range map[string]Scope{
		"":          ScopeWorkspace,
		"workspace": ScopeWorkspace, "machine": ScopeMachine,
	} {
		got, err := NormalizeScope(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeScope(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"auto", "masked", "off", "require"} {
		if _, err := NormalizeScope(input); err == nil {
			t.Fatalf("NormalizeScope accepted obsolete or unknown value %q", input)
		}
	}
}

func TestBoundaryLayoutRejectsUnknownScope(t *testing.T) {
	if err := (Boundary{Scope: Scope("unknown")}).ValidateLayout(); err == nil {
		t.Fatal("process layer accepted an unknown scope")
	}
}

func TestBoundaryNeedsBackendOnlyForActualRestrictions(t *testing.T) {
	if (Boundary{Scope: ScopeMachine}).NeedsBackend() {
		t.Fatal("unrestricted machine scope requires a backend")
	}
	for _, boundary := range []Boundary{
		{Scope: ScopeWorkspace},
		{Scope: ScopeMachine, ProtectedPaths: []string{"/private"}},
	} {
		if !boundary.NeedsBackend() {
			t.Fatalf("restricted boundary does not require a backend: %#v", boundary)
		}
	}
}

func TestBoundaryCommandRejectsUnknownScope(t *testing.T) {
	_, err := BoundaryBashCommand("true", t.TempDir(), Boundary{Scope: Scope("unknown")})
	if err == nil {
		t.Fatal("BoundaryBashCommand accepted an unknown scope")
	}
}

func TestMachineDoesNotValidateUnusedToolHome(t *testing.T) {
	workspace, stateHome := t.TempDir(), t.TempDir()
	manager, err := NewProcessManager(workspace, stateHome, workspace, ScopeMachine)
	if err != nil {
		t.Fatalf("machine depends on unused tool home: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
}

func TestProcessManagerAllowsStateHomeInsideWorkspace(t *testing.T) {
	for _, scope := range []Scope{ScopeWorkspace, ScopeMachine} {
		t.Run(string(scope), func(t *testing.T) {
			workspace := t.TempDir()
			stateHome := filepath.Join(workspace, ".skot")
			toolHomeRoot := filepath.Join(workspace, ".cache", "skot", "tool-home")
			manager, err := NewProcessManager(workspace, stateHome, toolHomeRoot, scope)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
		})
	}
}

func TestBoundaryLayoutRejectsUnsafeContainment(t *testing.T) {
	workspaceParent := t.TempDir()
	workspace := filepath.Join(workspaceParent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		boundary  Boundary
		wantError string
	}{
		{
			name: "protected path contains workspace",
			boundary: Boundary{
				Scope: ScopeWorkspace, Workspace: workspace, ToolHome: t.TempDir(),
				ProtectedPaths: []string{workspaceParent},
			},
			wantError: "contains the workspace",
		},
		{
			name: "tool home contains workspace",
			boundary: Boundary{
				Scope: ScopeWorkspace, Workspace: workspace, ToolHome: workspaceParent,
				ProtectedPaths: []string{t.TempDir()},
			},
			wantError: "tool home must not contain",
		},
		{
			name: "protected path overlaps tool home",
			boundary: Boundary{
				Scope: ScopeWorkspace, Workspace: t.TempDir(), ToolHome: filepath.Join(workspaceParent, "tool-home"),
				ProtectedPaths: []string{workspaceParent},
			},
			wantError: "overlaps tool home",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.boundary.ValidateLayout()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("ValidateLayout error = %v", err)
			}
		})
	}
}
