package decisions_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/levmv/skot/decisions"
	"github.com/levmv/skot/model"
)

func ExampleClient_Evaluate() {
	client, err := decisions.New(decisions.Config{
		Provider: "openrouter", Model: "~typesafe/jev-latest",
		BaseURL:         "https://openrouter.ai/api/v1",
		ResponseProfile: model.ResponseProfileOpenRouter,
		APIKey:          os.Getenv("OPENROUTER_API_KEY"),
	})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := client.Evaluate(ctx, decisions.Request{
		State: "My account was charged twice. Can you refund the duplicate payment?",
		Questions: map[string]decisions.Question{
			"refund": {Type: decisions.Noul, Instructions: "Is the customer requesting a refund?"},
			"team": {
				Type: decisions.Choice, Instructions: "Which team should handle the request?",
				Criteria: map[string]string{"billing": "Payments and refunds", "support": "Technical help"},
			},
			"urgency": {
				Type: decisions.Score, Instructions: "How urgently does this need attention?",
				Criteria: []string{"Can wait", "Needs attention this week", "Needs attention today"},
			},
		},
	})
	// Record usage even when the answers could not be read.
	fmt.Printf("usage: %+v\n", result.Usage)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Answers["refund"].Noul)
	fmt.Println(result.Answers["team"].Choice, *result.Answers["team"].Confidence)
	fmt.Println(result.Answers["urgency"].Score)
}
