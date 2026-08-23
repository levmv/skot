//go:build darwin

package tools

import (
	"strings"
	"testing"
)

func TestSeatbeltProfileKeepsNetworkOpen(t *testing.T) {
	profile := seatbeltProfile(Boundary{Scope: ScopeWorkspace, Workspace: "/workspace", ToolHome: "/tool-home"})
	if !strings.Contains(profile, "(allow network*)") {
		t.Fatal("seatbelt profile does not keep network open")
	}
}

func TestSeatbeltProfileDoesNotGrantSharedTemp(t *testing.T) {
	profile := seatbeltProfile(Boundary{Scope: ScopeWorkspace, Workspace: "/workspace", ToolHome: "/tool-home"})
	for _, shared := range []string{"/private/tmp", "/private/var/tmp", "TEMP_DIR"} {
		if strings.Contains(profile, shared) {
			t.Fatalf("seatbelt profile grants shared temp through %q", shared)
		}
	}
}

func TestSeatbeltProfileGrantsAddedDirectoryBeforeProtectedDenies(t *testing.T) {
	profile := seatbeltProfile(Boundary{
		Scope: ScopeWorkspace, Workspace: "/workspace", ToolHome: "/tool-home",
		AddedPaths: []string{"/shared"}, ProtectedPaths: []string{"/shared/private"},
	})
	grant := strings.Index(profile, `(subpath "/shared")`)
	deny := strings.Index(profile, `(deny file-read* file-write* (literal "/shared/private"))`)
	if grant < 0 || deny < 0 || grant >= deny {
		t.Fatalf("added-directory grant and protected deny are not ordered:\n%s", profile)
	}
}

func TestSeatbeltMaskedAllowsAmbientAuthorityExceptState(t *testing.T) {
	profile := seatbeltProfile(Boundary{Scope: ScopeMachine, ProtectedPaths: []string{"/private/skot-state"}})
	for _, want := range []string{"(allow default)", "deny file-read* file-write*", `literal "/private/skot-state"`, `subpath "/private/skot-state"`} {
		if !strings.Contains(profile, want) {
			t.Fatalf("masked profile %q does not contain %q", profile, want)
		}
	}
}
