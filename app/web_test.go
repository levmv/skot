package app

import (
	"context"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestWebCatalogIncludesPublicSearchWithoutCredentials(t *testing.T) {
	clearWebCredentialEnvironment(t)
	application, err := Open(context.Background(), Config{
		Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/test",
		Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if names := availableApplicationTools(t, application); !strings.Contains(names, "web_fetch") || !strings.Contains(names, "web_search") {
		t.Fatalf("tools without search credential = %q", names)
	}
}

func TestCancelledWebLoginDoesNotStoreCredential(t *testing.T) {
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
	if err := application.Login(cancelled, "keenable", "temporary-token"); err == nil {
		t.Fatal("cancelled login succeeded")
	}
	if token, _, err := credentialForProvider(application.config.settings, "keenable"); err != nil || token != "" {
		t.Fatalf("credential after cancelled login = %q, %v", token, err)
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
	toolServices := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		descriptions[status.Name] = status.Description
		toolServices[status.Name] = status.ToolService
	}
	if descriptions["keenable"] != "web search and fetch" || descriptions["tavily"] != "web search" || descriptions["firecrawl"] != "web fetch" || descriptions["exa"] != "web search and fetch" {
		t.Fatalf("provider statuses = %#v", descriptions)
	}
	if toolServices["deepseek"] || !toolServices["keenable"] || !toolServices["tavily"] || !toolServices["firecrawl"] || !toolServices["exa"] {
		t.Fatalf("tool service statuses = %#v", toolServices)
	}
}

func availableApplicationTools(t *testing.T, application *Application) string {
	t.Helper()
	application.mu.RLock()
	toolSets, toolSet := application.config.toolSets, application.state.toolSet
	tools := append([]agent.Tool(nil), application.config.tools...)
	application.mu.RUnlock()
	selected, err := toolSets.Tools(tools, toolSet)
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
	for _, name := range []string{"KEENABLE_API_KEY", "TAVILY_API_KEY", "EXA_API_KEY", "FIRECRAWL_API_KEY"} {
		t.Setenv(name, "")
	}
}
