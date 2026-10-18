package images_test

import (
	"context"
	"os"

	"github.com/levmv/skot/images"
)

// An edit sends the previous result unchanged as a reference, keeping its
// format and resolution.
func ExampleClient_Generate() {
	client, err := images.New(images.Config{
		API: images.OpenAIImages, Provider: "openai", Model: "gpt-image-2",
		APIKey: os.Getenv("OPENAI_API_KEY"),
	})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	created, err := client.Generate(ctx, images.Request{Prompt: "A bright living room"}, nil)
	recordAttempt(created, err) // Usage is reported even when Generate fails.
	if err != nil {
		panic(err)
	}
	edited, err := client.Generate(ctx, images.Request{
		Prompt:     "Make it darker and keep the furniture layout",
		References: []images.Image{created.Images[0]},
	}, nil)
	recordAttempt(edited, err)
	if err != nil {
		panic(err)
	}
}

func recordAttempt(images.Response, error) {}
