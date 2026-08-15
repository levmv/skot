package app

import (
	"context"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestWebCatalogTracksCredentialAvailability(t *testing.T) {
	clearWebCredentialEnvironment(t)
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/test",
		Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if names := availableApplicationTools(t, application); !strings.Contains(names, "web_fetch") || strings.Contains(names, "web_search") {
		t.Fatalf("tools without search credential = %q", names)
	}
	if err := application.Login(context.Background(), "tavily", "search-token"); err != nil {
		t.Fatal(err)
	}
	if names := availableApplicationTools(t, application); !strings.Contains(names, "web_fetch") || !strings.Contains(names, "web_search") {
		t.Fatalf("tools after login = %q", names)
	}
	if err := application.Logout(context.Background(), "tavily"); err != nil {
		t.Fatal(err)
	}
	if names := availableApplicationTools(t, application); strings.Contains(names, "web_search") {
		t.Fatalf("tools after logout = %q", names)
	}
}

func TestWebLoginRollsBackCredentialWhenCatalogReloadFails(t *testing.T) {
	clearWebCredentialEnvironment(t)
	home := t.TempDir()
	application, err := Open(context.Background(), Config{
		Home: home, Root: t.TempDir(), ModelURI: "deepseek/test",
		Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.Login(cancelled, "tavily", "temporary-token"); err == nil {
		t.Fatal("login with cancelled catalog reload succeeded")
	}
	if token, _, err := credentialForProvider(application.config.settings, "tavily"); err != nil || token != "" {
		t.Fatalf("rolled-back credential = %q, %v", token, err)
	}
}

func TestProviderStatusesIncludeWebServices(t *testing.T) {
	clearWebCredentialEnvironment(t)
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/test",
		Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	statuses, err := application.ProviderStatuses()
	if err != nil {
		t.Fatal(err)
	}
	descriptions := make(map[string]string, len(statuses))
	for _, status := range statuses {
		descriptions[status.Name] = status.Description
	}
	if descriptions["tavily"] != "web search" || descriptions["firecrawl"] != "web fetch fallback" || descriptions["exa"] != "web search and fetch" {
		t.Fatalf("provider statuses = %#v", descriptions)
	}
}

func availableApplicationTools(t *testing.T, application *Application) string {
	t.Helper()
	application.mu.RLock()
	toolSets, toolSet, settings := application.config.toolSets, application.state.toolSet, application.config.settings
	tools := append([]agent.Tool(nil), application.config.tools...)
	application.mu.RUnlock()
	selected, err := toolSetTools(toolSets, tools, settings, toolSet)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(selected))
	for _, tool := range selected {
		names = append(names, tool.Spec.Name)
	}
	return strings.Join(names, ",")
}

func clearWebCredentialEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"TAVILY_API_KEY", "EXA_API_KEY", "FIRECRAWL_API_KEY"} {
		t.Setenv(name, "")
	}
}
