package toolpolicy

import (
	"context"
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestBuiltInToolSetsSelectExactOrderedTools(t *testing.T) {
	catalog := builtInCatalog("custom")
	toolSets, err := NewToolSets(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for toolSet, want := range map[string]string{
		ToolSetReadOnly: "read,ls,grep,glob,web_fetch,web_search",
		ToolSetEdit:     "read,ls,grep,glob,edit,write,web_fetch,web_search",
		ToolSetDefault:  "read,grep,glob,edit,write,bash,job,web_fetch,web_search",
		ToolSetNone:     "",
	} {
		if got := toolNames(mustToolSetTools(t, toolSets, catalog, toolSet)); got != want {
			t.Fatalf("%s tools = %q, want %q", toolSet, got, want)
		}
	}
}

func TestBuiltInOptionsAddDefaultLSWithoutChangingOverrides(t *testing.T) {
	catalog := builtInCatalog("custom")
	toolSets, err := NewToolSetsWithOptions(catalog, BuiltInOptions{DefaultIncludesLS: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(mustToolSetTools(t, toolSets, catalog, ToolSetDefault)); got != "read,ls,grep,glob,edit,write,bash,job,web_fetch,web_search" {
		t.Fatalf("conditional default tools = %q", got)
	}
	toolSets, err = NewToolSetsWithOptions(
		catalog, BuiltInOptions{DefaultIncludesLS: true}, map[string][]string{ToolSetDefault: {"custom"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(mustToolSetTools(t, toolSets, catalog, ToolSetDefault)); got != "custom" {
		t.Fatalf("overridden default tools = %q", got)
	}
}

func TestConfiguredToolSetsReplaceBuiltInsAndAddNames(t *testing.T) {
	catalog := builtInCatalog("custom")
	toolSets, err := NewToolSets(catalog,
		map[string][]string{"default": {"read", "custom"}, "review": {"custom", "read"}},
		map[string][]string{" DEFAULT ": {"custom"}, "empty": nil},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(mustToolSetTools(t, toolSets, catalog, ToolSetDefault)); got != "custom" {
		t.Fatalf("overridden default tools = %q", got)
	}
	if got := toolNames(mustToolSetTools(t, toolSets, catalog, "review")); got != "custom,read" {
		t.Fatalf("review tools = %q", got)
	}
	if got := toolNames(mustToolSetTools(t, toolSets, catalog, "empty")); got != "" {
		t.Fatalf("empty tools = %q", got)
	}
	if got := strings.Join(toolSets.Names(), ","); got != "default,edit,read-only,none,empty,review" {
		t.Fatalf("tool set names = %q", got)
	}
	if got := strings.Join(toolSets.ToolNames(ToolSetDefault), ","); got != "custom" {
		t.Fatalf("default tool names = %q", got)
	}
	if names := toolSets.ToolNames(ToolSetDefault); len(names) != 0 {
		names[0] = "read"
	}
	if got := strings.Join(toolSets.ToolNames(ToolSetDefault), ","); got != "custom" {
		t.Fatalf("tool set tool names were aliased: %q", got)
	}
	if got := toolSets.ToolNames("missing"); got != nil {
		t.Fatalf("unknown tool set tool names = %#v", got)
	}
	if got, err := toolSets.Normalize(" REVIEW "); err != nil || got != "review" {
		t.Fatalf("normalized tool set = %q, %v", got, err)
	}
	if got, err := toolSets.Normalize(""); err != nil || got != ToolSetDefault {
		t.Fatalf("default tool set = %q, %v", got, err)
	}
}

func TestConfiguredToolSetsRejectInvalidDefinitions(t *testing.T) {
	catalog := builtInCatalog()
	for name, configured := range map[string]map[string][]string{
		"unknown tool":         {"review": {"missing"}},
		"duplicate tool":       {"review": {"read", "read"}},
		"empty tool":           {"review": {""}},
		"empty tool set":       {"": {"read"}},
		"whitespace tool set":  {"code review": {"read"}},
		"normalized duplicate": {"DEFAULT": {"read"}, " default ": {"read"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewToolSets(catalog, configured); err == nil {
				t.Fatal("invalid tool set definition accepted")
			}
		})
	}
}

func TestNormalizeRejectsUnknownToolSet(t *testing.T) {
	toolSets, err := NewToolSets(builtInCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"admin", "full"} {
		if _, err := toolSets.Normalize(input); err == nil || !strings.Contains(err.Error(), "default, edit, read-only, none") {
			t.Fatalf("unknown tool set %q error = %v", input, err)
		}
	}
}

func TestToolsRejectsCatalogDrift(t *testing.T) {
	catalog := builtInCatalog()
	toolSets, err := NewToolSets(catalog)
	if err != nil {
		t.Fatal(err)
	}
	withoutJob := make([]agent.Tool, 0, len(catalog)-1)
	for _, tool := range catalog {
		if tool.Spec.Name != "job" {
			withoutJob = append(withoutJob, tool)
		}
	}
	if _, err := toolSets.Tools(withoutJob, ToolSetDefault); err == nil || !strings.Contains(err.Error(), `requires tool "job"`) {
		t.Fatalf("catalog drift error = %v", err)
	}
}

func TestBackgroundCapableToolsRequireJobWithoutChangingExactToolSet(t *testing.T) {
	catalog := builtInCatalog("worker")
	toolSets, err := NewToolSets(catalog, map[string][]string{
		"safe":   {"read", "worker", "job"},
		"broken": {"worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := toolSets.RequireTogether("job", map[string]struct{}{"worker": {}}); err == nil || !strings.Contains(err.Error(), `tool set "broken"`) {
		t.Fatalf("dependency error = %v", err)
	}
	if got := toolNames(mustToolSetTools(t, toolSets, catalog, "safe")); got != "read,worker,job" {
		t.Fatalf("tool set was modified = %q", got)
	}
}

func builtInCatalog(extra ...string) []agent.Tool {
	names := []string{"read", "ls", "grep", "glob", "edit", "write", "bash", "job", "web_fetch", "web_search"}
	return testCatalog(append(names, extra...)...)
}

func testCatalog(names ...string) []agent.Tool {
	tools := make([]agent.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, agent.Tool{
			Spec: agent.ToolSpec{Name: name, InputSchema: jsontext.Value(`{"type":"object"}`)},
			Run:  func(context.Context, string) (agent.ToolOutput, error) { return agent.ToolOutput{}, nil },
		})
	}
	return tools
}

func mustToolSetTools(t *testing.T, toolSets ToolSets, catalog []agent.Tool, toolSet string) []agent.Tool {
	t.Helper()
	tools, err := toolSets.Tools(catalog, toolSet)
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
