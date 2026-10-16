package provider_test

import (
	"context"
	"fmt"
	"log"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

func ExampleOpen() {
	ctx := context.Background()
	selected, err := provider.Open(ctx, provider.Config{URI: "deepseek/deepseek-flash"})
	if err != nil {
		log.Fatal(err)
	}
	// Load these two values from your storage when continuing a conversation.
	var replay model.ReplayContext
	items := []model.Item{{Kind: model.ItemUserText, Text: "Hello"}}
	nextReplay, err := replay.ForModel(selected.Info)
	if err != nil {
		log.Fatal(err)
	}
	request := nextReplay.PrepareRequest(selected.Backend, model.Request{
		SessionID: "chat-123", Items: items,
	})
	response, err := selected.Backend.Complete(ctx, request, nil)
	// Account for this attempt even when err is non-nil.
	fmt.Printf("usage: %+v\n", response.Usage)
	if err != nil {
		log.Fatal(err)
	}
	response, err = nextReplay.AcceptResponse(response)
	if err != nil {
		log.Fatal(err)
	}
	items = append(items, response.Items...)
	// Save nextReplay and items. A fork loads them from its branch point.
	// For a tool call, execute the tool and append a ToolResult item with CallID
	// set to ToolCall.ID, then prepare the next request.
	fmt.Printf("saved %d items\n", len(items))
}

func ExampleNew() {
	selected, err := provider.New(provider.ExplicitConfig{
		Provider: "my-gateway", Model: "exact-upstream-model",
		API: provider.Responses, BaseURL: "https://gateway.example/v1",
		ContextWindow: 128_000, ReasoningEffort: "medium", APIKey: "caller-key",
	})
	if err != nil {
		log.Fatal(err)
	}
	// Construction is offline. Complete and Connection.Post use the same route.
	// Use ReplayContext as in ExampleOpen to continue a conversation.
	fmt.Println(selected.Connection.API(), selected.Connection.APIModel())
	// Output: responses exact-upstream-model
}
