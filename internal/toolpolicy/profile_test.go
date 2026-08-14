package toolpolicy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestDefaultProfilesSelectExactOrderedTools(t *testing.T) {
	catalog := testCatalog("read", "ls", "grep", "glob", "edit", "write", "bash", "job", "custom")
	profiles, err := NewProfiles(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for profile, want := range map[string]string{
		ProfileReadOnly: "read,ls,grep,glob",
		ProfileEdit:     "read,ls,grep,glob,edit,write",
		ProfileFull:     "read,grep,glob,edit,write,bash,job",
	} {
		if got := toolNames(mustProfileTools(t, profiles, catalog, profile)); got != want {
			t.Fatalf("%s tools = %q, want %q", profile, got, want)
		}
	}
}

func TestDefaultProfilesIncludeKnownWebTools(t *testing.T) {
	catalog := testCatalog("read", "ls", "grep", "glob", "edit", "write", "bash", "job", "web_fetch", "web_search")
	profiles, err := NewProfiles(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{ProfileReadOnly, ProfileEdit, ProfileFull} {
		got := toolNames(mustProfileTools(t, profiles, catalog, profile))
		if !strings.HasSuffix(got, "web_fetch,web_search") {
			t.Fatalf("%s tools = %q", profile, got)
		}
	}
}

func TestConfiguredProfilesReplaceDefaultsAndAddNames(t *testing.T) {
	catalog := testCatalog("read", "ls", "grep", "glob", "edit", "write", "bash", "job", "custom")
	profiles, err := NewProfiles(catalog,
		map[string][]string{"full": {"read", "custom"}, "review": {"custom", "read"}},
		map[string][]string{" FULL ": {"custom"}, "empty": nil},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(mustProfileTools(t, profiles, catalog, ProfileFull)); got != "custom" {
		t.Fatalf("overridden full tools = %q", got)
	}
	if got := toolNames(mustProfileTools(t, profiles, catalog, "review")); got != "custom,read" {
		t.Fatalf("review tools = %q", got)
	}
	if got := toolNames(mustProfileTools(t, profiles, catalog, "empty")); got != "" {
		t.Fatalf("empty tools = %q", got)
	}
	if got := strings.Join(profiles.Names(), ","); got != "read-only,edit,full,empty,review" {
		t.Fatalf("profile names = %q", got)
	}
	if got := strings.Join(profiles.ToolNames(ProfileFull), ","); got != "custom" {
		t.Fatalf("full tool names = %q", got)
	}
	if names := profiles.ToolNames(ProfileFull); len(names) != 0 {
		names[0] = "read"
	}
	if got := strings.Join(profiles.ToolNames(ProfileFull), ","); got != "custom" {
		t.Fatalf("profile tool names were aliased: %q", got)
	}
	if got := profiles.ToolNames("missing"); got != nil {
		t.Fatalf("unknown profile tool names = %#v", got)
	}
	if got, err := profiles.Normalize(" REVIEW "); err != nil || got != "review" {
		t.Fatalf("normalized profile = %q, %v", got, err)
	}
	if got, err := profiles.Normalize(""); err != nil || got != ProfileFull {
		t.Fatalf("default profile = %q, %v", got, err)
	}
}

func TestConfiguredProfilesRejectInvalidDefinitions(t *testing.T) {
	catalog := testCatalog("read", "ls", "grep", "glob", "edit", "write", "bash", "job")
	for name, configured := range map[string]map[string][]string{
		"unknown tool":         {"review": {"missing"}},
		"duplicate tool":       {"review": {"read", "read"}},
		"empty tool":           {"review": {""}},
		"empty profile":        {"": {"read"}},
		"whitespace profile":   {"code review": {"read"}},
		"normalized duplicate": {"FULL": {"read"}, " full ": {"read"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProfiles(catalog, configured); err == nil {
				t.Fatal("invalid profile definition accepted")
			}
		})
	}
}

func TestNormalizeRejectsUnknownProfile(t *testing.T) {
	profiles, err := NewProfiles(testCatalog("read", "ls", "grep", "glob", "edit", "write", "bash", "job"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.Normalize("admin"); err == nil || !strings.Contains(err.Error(), "read-only, edit, full") {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestToolsRejectsCatalogDrift(t *testing.T) {
	catalog := testCatalog("read", "ls", "grep", "glob", "edit", "write", "bash", "job")
	profiles, err := NewProfiles(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.Tools(catalog[:len(catalog)-1], ProfileFull); err == nil || !strings.Contains(err.Error(), `requires tool "job"`) {
		t.Fatalf("catalog drift error = %v", err)
	}
}

func TestBackgroundCapableToolsRequireJobWithoutChangingExactProfile(t *testing.T) {
	catalog := testCatalog("read", "ls", "grep", "glob", "edit", "write", "bash", "job", "worker")
	profiles, err := NewProfiles(catalog, map[string][]string{
		"safe":   {"read", "worker", "job"},
		"broken": {"worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.RequireTogether("job", map[string]struct{}{"worker": {}}); err == nil || !strings.Contains(err.Error(), `profile "broken"`) {
		t.Fatalf("dependency error = %v", err)
	}
	if got := toolNames(mustProfileTools(t, profiles, catalog, "safe")); got != "read,worker,job" {
		t.Fatalf("profile was modified = %q", got)
	}
}

func testCatalog(names ...string) []agent.Tool {
	tools := make([]agent.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, agent.Tool{
			Spec: agent.ToolSpec{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run:  func(context.Context, string) (agent.ToolOutput, error) { return agent.ToolOutput{}, nil },
		})
	}
	return tools
}

func mustProfileTools(t *testing.T, profiles Profiles, catalog []agent.Tool, profile string) []agent.Tool {
	t.Helper()
	tools, err := profiles.Tools(catalog, profile)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func toolNames(tools []agent.Tool) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Spec.Name)
	}
	return strings.Join(names, ",")
}
