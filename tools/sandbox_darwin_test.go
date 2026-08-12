//go:build darwin

package tools

import (
	"strings"
	"testing"
)

func TestSeatbeltProfileKeepsNetworkOpen(t *testing.T) {
	profile := seatbeltProfile(Sandbox{Policy: SandboxWorkspace, Workspace: "/workspace", ToolHome: "/tool-home"})
	if !strings.Contains(profile, "(allow network*)") {
		t.Fatal("seatbelt profile does not keep network open")
	}
}

func TestSeatbeltProfileDoesNotGrantSharedTemp(t *testing.T) {
	profile := seatbeltProfile(Sandbox{Policy: SandboxWorkspace, Workspace: "/workspace", ToolHome: "/tool-home"})
	for _, shared := range []string{"/private/tmp", "/private/var/tmp", "TEMP_DIR"} {
		if strings.Contains(profile, shared) {
			t.Fatalf("seatbelt profile grants shared temp through %q", shared)
		}
	}
}

func TestSeatbeltMaskedAllowsAmbientAuthorityExceptState(t *testing.T) {
	profile := seatbeltProfile(Sandbox{Policy: SandboxMasked, ProtectedPaths: []string{"/private/skot-state"}})
	for _, want := range []string{"(allow default)", "deny file-read* file-write*", `literal "/private/skot-state"`, `subpath "/private/skot-state"`} {
		if !strings.Contains(profile, want) {
			t.Fatalf("masked profile %q does not contain %q", profile, want)
		}
	}
}
