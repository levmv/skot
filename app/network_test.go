package app

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplicationProxySurvivesSessionChanges(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !request.URL.IsAbs() || request.Host != "provider.invalid" {
			t.Errorf("unexpected proxy request: %s %s", request.Method, request.URL)
		}
		writeChildTestAnswer(t, writer, "through proxy")
	}))
	t.Cleanup(proxy.Close)
	home, root := t.TempDir(), t.TempDir()
	raw, _ := json.Marshal(map[string]string{"proxy": proxy.URL})
	if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	config := childTestConfig(home, root, "http://provider.invalid/v1", true)
	config.RetryBudget = time.Second
	application, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	run := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result, err := application.Run(ctx, "hello", nil)
		if err != nil || result.Answer != "through proxy" {
			t.Fatalf("proxied run = %+v, %v", result, err)
		}
	}
	run()
	parentID := application.SessionID()
	if err := application.SwitchModel(t.Context(), "deepseek/another-proxy-test", "", "chat_completions"); err != nil {
		t.Fatal(err)
	}
	run()
	tool := childTestTool(t, application)
	started := runChildTestTool(t, tool, parentID, childToolArgs{Action: "start", Prompt: "child"})
	childID := strings.Fields(started.Content.Text())[1]
	checked := runChildTestTool(t, tool, parentID, childToolArgs{Action: "check", IDs: []string{childID}, Wait: "all"})
	if !strings.Contains(checked.Content.Text(), "through proxy") {
		t.Fatalf("child result = %s", checked.Content.Text())
	}
	if _, err := application.ClearSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	run()
	if _, err := application.ResumeSession(t.Context(), parentID); err != nil {
		t.Fatal(err)
	}
	run()

	// An explicit Go setting overrides the file for this instance only.
	overrideProxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeChildTestAnswer(t, writer, "override proxy")
	}))
	t.Cleanup(overrideProxy.Close)
	override := config
	override.ProxyURL = overrideProxy.URL
	other, err := Open(t.Context(), override)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if result, err := other.Run(t.Context(), "hello", nil); err != nil || result.Answer != "override proxy" {
		t.Fatalf("override run = %+v, %v", result, err)
	}
	run()
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	config.Resume, config.ResumePrefix = true, parentID
	application, err = Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	run()
}

func TestApplicationProxyCoversServiceRequests(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	clearWebCredentialEnvironment(t)
	for _, test := range []struct {
		name string
		host string
		run  func(*testing.T, *Application) error
	}{
		{"model metadata", "openrouter.ai:443", func(t *testing.T, application *Application) error {
			return application.SwitchModelWithContextWindow(t.Context(), "openrouter/vendor/proxy-test", "", "chat_completions", 0)
		}},
		{"web search", "api.keenable.ai:443", func(t *testing.T, application *Application) error {
			for _, tool := range application.config.tools {
				if tool.Spec.Name == "web_search" {
					_, err := tool.Run(t.Context(), `{"query":"hello"}`)
					return err
				}
			}
			return fmt.Errorf("web search unavailable")
		}},
		{"browser token exchange", "auth.openai.com:443", func(t *testing.T, application *Application) error {
			login, err := application.BeginBrowserLogin(t.Context(), "openai-codex")
			if err != nil {
				t.Fatal(err)
			}
			defer login.Close()
			authorization, _ := url.Parse(login.AuthorizationURL())
			query := url.Values{"state": {authorization.Query().Get("state")}, "code": {"test-code"}}
			if err := login.SubmitRedirect("http://localhost:1455/auth/callback?" + query.Encode()); err != nil {
				t.Fatal(err)
			}
			return login.Wait()
		}},
		{"quota", "chatgpt.com:443", func(t *testing.T, application *Application) error {
			saveTestCodexTokens(t, application.config.settings, testCodexTokens())
			if err := application.SwitchModel(t.Context(), "openai-codex/gpt-6-astra", "", ""); err != nil {
				t.Fatal(err)
			}
			return application.RefreshAccountQuota(t.Context())
		}},
		{"token refresh", "auth.openai.com:443", func(t *testing.T, application *Application) error {
			tokens := testCodexTokens()
			tokens.ExpiresAt = time.Now().Add(-time.Hour)
			saveTestCodexTokens(t, application.config.settings, tokens)
			if err := application.SwitchModel(t.Context(), "openai-codex/gpt-6-astra", "", ""); err != nil {
				t.Fatal(err)
			}
			return application.RefreshAccountQuota(t.Context())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan string, 8)
			proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests <- request.Host
				http.Error(writer, "proxy unavailable", http.StatusBadGateway)
			}))
			t.Cleanup(proxy.Close)
			application, err := Open(t.Context(), Config{
				Home: t.TempDir(), Root: t.TempDir(), ModelURI: "deepseek/test",
				Interactive: true, ToolSet: ToolSetReadOnly, ProxyURL: proxy.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = application.Close() })
			if err := test.run(t, application); err == nil {
				t.Fatal("request succeeded despite proxy failure")
			}
			select {
			case host := <-requests:
				if host != test.host {
					t.Fatalf("proxy destination = %q; want %q", host, test.host)
				}
			default:
				t.Fatal("request bypassed the configured proxy")
			}
		})
	}
}

func TestApplicationRejectsInvalidProxy(t *testing.T) {
	application, err := Open(t.Context(), Config{Home: t.TempDir(), Root: t.TempDir(), ProxyURL: "ftp://proxy.invalid"})
	if err == nil {
		_ = application.Close()
		t.Fatal("invalid proxy accepted")
	}
	if !strings.Contains(err.Error(), "invalid proxy URL") {
		t.Fatalf("proxy error = %v", err)
	}
}
