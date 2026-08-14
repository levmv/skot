package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelhttp"
)

const openCodeGoLiveOptIn = "SK_LIVE_OPENCODE_GO"

type openCodeGoLiveRoute struct {
	URI        string
	ToolEffort string
}

var openCodeGoLiveRoutes = []openCodeGoLiveRoute{
	{URI: "opencode-go/gpt-5.6-luna", ToolEffort: "high"},
	{URI: "opencode-go/deepseek-v4-flash", ToolEffort: "high"},
	{URI: "opencode-go/deepseek-v4-pro", ToolEffort: "high"},
	{URI: "opencode-go/kimi-k3", ToolEffort: "max"},
	{URI: "opencode-go/glm-5.2", ToolEffort: "high"},
}

// TestOpenCodeGoLiveBaseline is intentionally absent from ordinary CI even
// when a developer happens to have a credential in the environment. It makes
// paid external requests and certifies route behavior, not merely reachability.
func TestOpenCodeGoLiveBaseline(t *testing.T) {
	if strings.TrimSpace(os.Getenv(openCodeGoLiveOptIn)) != "1" {
		t.Skip("set " + openCodeGoLiveOptIn + "=1 to run paid OpenCode Go checks")
	}
	if strings.TrimSpace(os.Getenv("OPENCODE_API_KEY")) == "" {
		t.Fatal("OPENCODE_API_KEY is required when live OpenCode Go checks are enabled")
	}

	t.Run("01_luna_encrypted_state", liveOpenCodeGoLunaState)
	t.Run("02_streamed_text_and_usage", liveOpenCodeGoTextAndUsage)
	t.Run("03_tool_call_and_consecutive_tool_turns", liveOpenCodeGoToolTurns)
	t.Run("04_declared_provider_state_replay", liveOpenCodeGoReplayPolicies)
	t.Run("05_reasoning_choices_and_tool_schema", liveOpenCodeGoReasoningChoices)
	t.Run("06_effective_context_boundary", liveOpenCodeGoContextBoundary)
	t.Run("07_structured_provider_errors", liveOpenCodeGoProviderErrors)
	t.Run("08_optional_fields_accepted", liveOpenCodeGoOptionalFields)
}

func liveOpenCodeGoLunaState(t *testing.T) {
	backend := openCodeGoLiveBackend(t, "opencode-go/gpt-5.6-luna", "high", nil)
	epoch := "live_luna_epoch"
	firstInput := agent.Item{Kind: agent.ItemUserText, Text: "Briefly explain why 2 + 2 = 4. Use reasoning and finish with LUNA_STATE_OK."}
	first := openCodeGoLiveComplete(t, backend, agent.ModelRequest{
		ProviderEpoch: epoch, Items: []agent.Item{firstInput},
	})
	assertOpenCodeGoLiveUsage(t, first.Usage)
	if !strings.Contains(openCodeGoLiveText(first), "LUNA_STATE_OK") {
		t.Fatalf("first Luna text does not contain sentinel: %q", openCodeGoLiveText(first))
	}
	reasoning := openCodeGoLiveReasoning(first)
	if len(reasoning) == 0 {
		t.Fatal("Luna returned no reasoning item")
	}
	for index, item := range reasoning {
		if len(item.ProviderData) == 0 {
			t.Fatalf("reasoning item %d has no encrypted provider state", index)
		}
		var state struct {
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if err := json.Unmarshal(item.ProviderData[0].Data, &state); err != nil || state.ID == "" || state.EncryptedContent == "" {
			t.Fatalf("reasoning item %d state is incomplete: id=%q encrypted=%t err=%v", index, state.ID, state.EncryptedContent != "", err)
		}
	}

}

func liveOpenCodeGoTextAndUsage(t *testing.T) {
	for _, candidate := range openCodeGoLiveRoutes {
		t.Run(strings.TrimPrefix(candidate.URI, "opencode-go/"), func(t *testing.T) {
			backend := openCodeGoLiveBackend(t, candidate.URI, candidate.ToolEffort, nil)
			response := openCodeGoLiveComplete(t, backend, agent.ModelRequest{Items: []agent.Item{{
				Kind: agent.ItemUserText, Text: "Reply exactly SKOT_STREAM_OK and nothing else.",
			}}})
			if !strings.Contains(openCodeGoLiveText(response), "SKOT_STREAM_OK") {
				t.Fatalf("text = %q", openCodeGoLiveText(response))
			}
			assertOpenCodeGoLiveUsage(t, response.Usage)
		})
	}
}

func liveOpenCodeGoToolTurns(t *testing.T) {
	tool := openCodeGoLiveTool()
	for _, candidate := range openCodeGoLiveRoutes {
		t.Run(strings.TrimPrefix(candidate.URI, "opencode-go/"), func(t *testing.T) {
			backend := openCodeGoLiveBackend(t, candidate.URI, candidate.ToolEffort, nil)
			epoch := "live_tools_epoch"
			items := []agent.Item{{Kind: agent.ItemUserText, Text: "Call baseline_step with step 1. Do not answer with text."}}
			first := openCodeGoLiveComplete(t, backend, agent.ModelRequest{ProviderEpoch: epoch, Items: items, Tools: []agent.ToolSpec{tool}})
			firstItems := ownOpenCodeGoLiveResponse(first, backend.Info(), epoch, "tool_first")
			firstCall := requireOpenCodeGoLiveToolCall(t, firstItems, 1)
			items = append(items, firstItems...)
			items = append(items, agent.Item{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{
				CallID: firstCall.ID, Content: "Step 1 succeeded. Call baseline_step with step 2 now; do not answer with text.",
			}})

			second := openCodeGoLiveComplete(t, backend, agent.ModelRequest{ProviderEpoch: epoch, Items: items, Tools: []agent.ToolSpec{tool}})
			secondItems := ownOpenCodeGoLiveResponse(second, backend.Info(), epoch, "tool_second")
			secondCall := requireOpenCodeGoLiveToolCall(t, secondItems, 2)
			items = append(items, secondItems...)
			items = append(items, agent.Item{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{
				CallID: secondCall.ID, Content: "Step 2 succeeded. Do not call another tool. Reply exactly SKOT_TOOL_DONE.",
			}})

			third := openCodeGoLiveComplete(t, backend, agent.ModelRequest{ProviderEpoch: epoch, Items: items, Tools: []agent.ToolSpec{tool}})
			if !strings.Contains(openCodeGoLiveText(third), "SKOT_TOOL_DONE") {
				t.Fatalf("final tool-loop text = %q", openCodeGoLiveText(third))
			}
			for _, item := range third.Items {
				if item.Kind == agent.ItemToolCall {
					t.Fatalf("model requested an unexpected third tool: %#v", item.ToolCall)
				}
			}
		})
	}
}

func liveOpenCodeGoReplayPolicies(t *testing.T) {
	t.Run("responses_encrypted_item", func(t *testing.T) {
		var requests [][]byte
		backend := openCodeGoLiveBackend(t, "opencode-go/gpt-5.6-luna", "high", openCodeGoRecordingClient(&requests))
		epoch := "live_luna_replay_epoch"
		firstInput := agent.Item{Kind: agent.ItemUserText, Text: "Reason briefly, then reply LUNA_REPLAY_SOURCE."}
		first := openCodeGoLiveComplete(t, backend, agent.ModelRequest{ProviderEpoch: epoch, Items: []agent.Item{firstInput}})
		items := []agent.Item{firstInput}
		items = append(items, ownOpenCodeGoLiveResponse(first, backend.Info(), epoch, "luna_replay_source")...)
		items = append(items, agent.Item{Kind: agent.ItemUserText, Text: "Reply exactly LUNA_REPLAY_OK."})
		second := openCodeGoLiveComplete(t, backend, agent.ModelRequest{ProviderEpoch: epoch, Items: items})
		if !strings.Contains(openCodeGoLiveText(second), "LUNA_REPLAY_OK") || len(requests) != 2 {
			t.Fatalf("Luna replay text/requests = %q/%d", openCodeGoLiveText(second), len(requests))
		}
		var wire struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(requests[1], &wire); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, raw := range wire.Input {
			var item struct {
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			}
			if json.Unmarshal(raw, &item) == nil && item.Type == "reasoning" && item.EncryptedContent != "" {
				found = true
			}
		}
		if !found {
			t.Fatal("second Responses request did not replay encrypted reasoning")
		}
	})

	for _, candidate := range openCodeGoLiveRoutes[1:] {
		t.Run(strings.TrimPrefix(candidate.URI, "opencode-go/"), func(t *testing.T) {
			var requests [][]byte
			backend := openCodeGoLiveBackend(t, candidate.URI, candidate.ToolEffort, openCodeGoRecordingClient(&requests))
			epoch := "live_chat_replay_epoch"
			tool := openCodeGoLiveTool()
			firstInput := agent.Item{Kind: agent.ItemUserText, Text: "Think about the request, then call baseline_step with step 1. Do not answer with text."}
			first := openCodeGoLiveComplete(t, backend, agent.ModelRequest{ProviderEpoch: epoch, Items: []agent.Item{firstInput}, Tools: []agent.ToolSpec{tool}})
			if len(openCodeGoLiveReasoning(first)) == 0 {
				t.Fatal("route returned no reasoning to exercise its replay policy")
			}
			owned := ownOpenCodeGoLiveResponse(first, backend.Info(), epoch, "chat_replay_source")
			call := requireOpenCodeGoLiveToolCall(t, owned, 1)
			items := []agent.Item{firstInput}
			items = append(items, owned...)
			items = append(items,
				agent.Item{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: call.ID, Content: "Step 1 succeeded."}},
				agent.Item{Kind: agent.ItemUserText, Text: "Reply exactly CHAT_REPLAY_OK without another tool."},
			)
			second := openCodeGoLiveComplete(t, backend, agent.ModelRequest{ProviderEpoch: epoch, Items: items, Tools: []agent.ToolSpec{tool}})
			if !strings.Contains(openCodeGoLiveText(second), "CHAT_REPLAY_OK") || len(requests) != 2 {
				t.Fatalf("chat replay text/requests = %q/%d", openCodeGoLiveText(second), len(requests))
			}
			var wire struct {
				Messages []struct {
					Role             string `json:"role"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(requests[1], &wire); err != nil {
				t.Fatal(err)
			}
			var replayed string
			for _, message := range wire.Messages {
				if message.Role == "assistant" {
					replayed += message.ReasoningContent
				}
			}
			route, err := resolveModelRoute(candidate.URI, candidate.ToolEffort, modelRouteOverrides{}, modelRouteEnrichment{})
			if err != nil {
				t.Fatal(err)
			}
			wantReplay := route.ChatTraits.ReasoningReplay == "tool_turns"
			if (replayed != "") != wantReplay {
				t.Fatalf("replayed reasoning present=%t, want %t", replayed != "", wantReplay)
			}
		})
	}
}

func liveOpenCodeGoReasoningChoices(t *testing.T) {
	for _, candidate := range openCodeGoLiveRoutes {
		declaration, ok := catalogModelSpec(candidate.URI)
		if !ok {
			t.Fatalf("missing declaration for %s", candidate.URI)
		}
		for _, effort := range declaration.ReasoningEfforts {
			name := effort
			if name == "" {
				name = "default"
			}
			t.Run(strings.TrimPrefix(candidate.URI, "opencode-go/")+"/"+name, func(t *testing.T) {
				backend := openCodeGoLiveBackend(t, candidate.URI, effort, nil)
				response := openCodeGoLiveComplete(t, backend, agent.ModelRequest{Items: []agent.Item{{
					Kind: agent.ItemUserText, Text: "Reply exactly EFFORT_OK.",
				}}})
				if !strings.Contains(openCodeGoLiveText(response), "EFFORT_OK") {
					t.Fatalf("text = %q", openCodeGoLiveText(response))
				}
			})
		}
	}
}

func liveOpenCodeGoProviderErrors(t *testing.T) {
	t.Run("authentication", liveOpenCodeGoAuthenticationError)
	t.Run("invalid_prompt", liveOpenCodeGoInvalidPromptError)
}

func liveOpenCodeGoAuthenticationError(t *testing.T) {
	original := os.Getenv("OPENCODE_API_KEY")
	if err := os.Setenv("OPENCODE_API_KEY", "skot-deliberately-invalid-live-baseline-key"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("OPENCODE_API_KEY", original) })
	backend := openCodeGoLiveBackend(t, "opencode-go/gpt-5.6-luna", "low", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err := backend.Complete(ctx, agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "Reply OK."}}}, nil)
	var providerErr *agent.ProviderError
	if err == nil || !errors.As(err, &providerErr) {
		t.Fatalf("invalid credential error = %v", err)
	}
	if providerErr.Kind != agent.ProviderErrorAuthentication {
		t.Fatalf("invalid credential metadata = %#v; error=%v", providerErr, err)
	}
	t.Logf("authentication payload: status=%d code=%q text=%q", providerErr.StatusCode, providerErr.Code, err)
}

func liveOpenCodeGoInvalidPromptError(t *testing.T) {
	body := strings.NewReader(`{"model":"gpt-5.6-luna","stream":true,"store":false,"input":1}`)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://opencode.ai/zen/go/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(os.Getenv("OPENCODE_API_KEY")))
	request.Header.Set("Content-Type", "application/json")
	response, err := modelhttp.DefaultClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode invalid-prompt response: %v; body=%q", err, payload)
	}
	if response.StatusCode != http.StatusBadRequest || envelope.Error.Code != "invalid_prompt" || strings.TrimSpace(envelope.Error.Message) == "" {
		t.Fatalf("invalid-prompt response = %s code=%q message=%q body=%q", response.Status, envelope.Error.Code, envelope.Error.Message, payload)
	}
	t.Logf("invalid-prompt payload: status=%s code=%q", response.Status, envelope.Error.Code)
}

func liveOpenCodeGoOptionalFields(t *testing.T) {
	var requests [][]byte
	client := openCodeGoRecordingClient(&requests)
	backend := openCodeGoLiveBackend(t, "opencode-go/gpt-5.6-luna", "high", client)
	response := openCodeGoLiveComplete(t, backend, agent.ModelRequest{Items: []agent.Item{{
		Kind: agent.ItemUserText, Text: "Reply exactly OPTIONAL_FIELDS_OK.",
	}}})
	if !strings.Contains(openCodeGoLiveText(response), "OPTIONAL_FIELDS_OK") || len(requests) != 1 {
		t.Fatalf("text/requests = %q/%d", openCodeGoLiveText(response), len(requests))
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(requests[0], &wire); err != nil {
		t.Fatal(err)
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(wire["reasoning"], &reasoning); err != nil {
		t.Fatalf("reasoning field = %s: %v", wire["reasoning"], err)
	}
	if string(reasoning["summary"]) != `"auto"` || string(reasoning["effort"]) != `"high"` {
		t.Fatalf("reasoning controls = %s", wire["reasoning"])
	}
	if _, exists := wire["include"]; exists {
		t.Fatal("live request used legacy reasoning.encrypted_content include")
	}
	if _, exists := reasoning["context"]; exists {
		t.Fatal("live request sent an undeclared reasoning.context")
	}
}

func liveOpenCodeGoContextBoundary(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SK_LIVE_OPENCODE_GO_CONTEXT")) != "1" {
		t.Skip("set SK_LIVE_OPENCODE_GO_CONTEXT=1 to send several expensive large-context turns per route")
	}
	for _, candidate := range openCodeGoLiveRoutes {
		t.Run(strings.TrimPrefix(candidate.URI, "opencode-go/"), func(t *testing.T) {
			route, err := resolveModelRoute(candidate.URI, candidate.ToolEffort, modelRouteOverrides{}, modelRouteEnrichment{})
			if err != nil {
				t.Fatal(err)
			}
			backend := openCodeGoLiveBackend(t, candidate.URI, candidate.ToolEffort, nil)
			runtime, err := agent.New(agent.Config{
				Model: backend, Journal: &applicationMemoryJournal{},
				SessionID:     "live-context-" + strings.TrimPrefix(candidate.URI, "opencode-go/"),
				RequestPolicy: agent.ModelRequestPolicy{MaxAttempts: 1, StreamIdleTimeout: 5 * time.Minute},
			})
			if err != nil {
				t.Fatal(err)
			}
			reserve := min(max(route.ContextWindow/5, 8*1024), 32*1024)
			inputLimit := route.ContextWindow - reserve
			blockTokens := max(1, inputLimit*42/100)
			prefix := strings.Repeat("abc ", blockTokens)
			compacted := false
			for turn := 1; turn <= 3; turn++ {
				sentinel := fmt.Sprintf("CONTEXT_TURN_%d_OK", turn)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				result, runErr := runtime.Run(ctx, prefix+"\nReply exactly "+sentinel+".", func(event agent.Event) {
					if event.Kind == agent.EventContextCompacted {
						compacted = true
					}
				})
				cancel()
				if runErr != nil {
					t.Fatalf("turn %d: %v", turn, runErr)
				}
				if !strings.Contains(result.Answer, sentinel) {
					t.Fatalf("turn %d answer = %q", turn, result.Answer)
				}
			}
			report, err := runtime.ContextReport(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !compacted || report.CompactionCount == 0 || report.TotalInputTokens > report.InputLimit {
				t.Fatalf("compaction/report = %t / %#v", compacted, report)
			}
		})
	}
}

func openCodeGoLiveTool() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "baseline_step", Description: "Advance the compatibility baseline by one exact step.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"integer","enum":[1,2]}},"required":["step"],"additionalProperties":false}`),
	}
}

func openCodeGoLiveBackend(t *testing.T, uri, effort string, client *http.Client) agent.Model {
	t.Helper()
	route, err := resolveModelRoute(uri, effort, modelRouteOverrides{}, modelRouteEnrichment{})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := buildModelBackend(route, nil, modelBackendOptions{requireCredential: true, httpClient: client})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func openCodeGoLiveComplete(t *testing.T, backend agent.Model, request agent.ModelRequest) agent.ModelResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	response, err := backend.Complete(ctx, request, nil)
	if err != nil {
		var providerErr *agent.ProviderError
		if errors.As(err, &providerErr) {
			t.Fatalf("%v [status=%d kind=%q code=%q retryable=%t]", err, providerErr.StatusCode, providerErr.Kind, providerErr.Code, providerErr.Retryable)
		}
		t.Fatal(err)
	}
	return response
}

func ownOpenCodeGoLiveResponse(response agent.ModelResponse, info agent.ModelInfo, epoch, responseID string) []agent.Item {
	items := make([]agent.Item, len(response.Items))
	for index, item := range response.Items {
		items[index] = item
		switch item.Kind {
		case agent.ItemAssistantText, agent.ItemReasoning, agent.ItemToolCall:
			items[index].ResponseID = responseID
		}
		if item.Kind == agent.ItemReasoning {
			items[index].ProviderContext = &agent.ProviderContext{Backend: info.Backend, Epoch: epoch}
		}
		if item.ToolCall != nil {
			call := *item.ToolCall
			call.ProviderReferences = append([]agent.ProviderReference(nil), item.ToolCall.ProviderReferences...)
			if call.ID == "" {
				call.ID = fmt.Sprintf("%s_call_%d", responseID, index)
			}
			for referenceIndex := range call.ProviderReferences {
				call.ProviderReferences[referenceIndex].Backend = info.Backend
				call.ProviderReferences[referenceIndex].Epoch = epoch
			}
			items[index].ToolCall = &call
		}
	}
	return items
}

func requireOpenCodeGoLiveToolCall(t *testing.T, items []agent.Item, step int) *agent.ToolCall {
	t.Helper()
	for _, item := range items {
		if item.Kind != agent.ItemToolCall || item.ToolCall == nil {
			continue
		}
		if item.ToolCall.Name != "baseline_step" {
			t.Fatalf("tool name = %q", item.ToolCall.Name)
		}
		var arguments struct {
			Step int `json:"step"`
		}
		if err := json.Unmarshal([]byte(item.ToolCall.RawArguments), &arguments); err != nil || arguments.Step != step {
			t.Fatalf("tool arguments = %q; want step %d; error=%v", item.ToolCall.RawArguments, step, err)
		}
		return item.ToolCall
	}
	kinds := make([]agent.ItemKind, len(items))
	for index, item := range items {
		kinds[index] = item.Kind
	}
	t.Fatalf("response contains no baseline_step call; item kinds=%v", kinds)
	return nil
}

func openCodeGoLiveText(response agent.ModelResponse) string {
	var text strings.Builder
	for _, item := range response.Items {
		if item.Kind == agent.ItemAssistantText {
			text.WriteString(item.Text)
		}
	}
	return text.String()
}

func openCodeGoLiveReasoning(response agent.ModelResponse) []agent.Item {
	var reasoning []agent.Item
	for _, item := range response.Items {
		if item.Kind == agent.ItemReasoning {
			reasoning = append(reasoning, item)
		}
	}
	return reasoning
}

func assertOpenCodeGoLiveUsage(t *testing.T, usage agent.ModelUsage) {
	t.Helper()
	if usage.InputTokens <= 0 || usage.OutputTokens <= 0 || usage.TotalTokens <= 0 {
		t.Fatalf("reported usage = %#v", usage)
	}
}

func openCodeGoRecordingClient(recorded *[][]byte) *http.Client {
	base := modelhttp.DefaultClient()
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	base.Transport = appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		*recorded = append(*recorded, append([]byte(nil), body...))
		request.Body = io.NopCloser(bytes.NewReader(body))
		return transport.RoundTrip(request)
	})
	return base
}
