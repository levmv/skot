package provider_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/levmv/skot/model/chatcompletions"
	"github.com/levmv/skot/model/transport"
	"github.com/levmv/skot/provider"
)

func ExampleModel_json() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	selected, err := provider.Open(ctx, provider.Config{URI: "deepseek/deepseek-flash", API: "chat_completions"})
	if err != nil {
		log.Print(err)
		return
	}
	body, err := json.Marshal(map[string]any{
		"model":    selected.Connection.APIModel(),
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})
	if err != nil {
		log.Print(err)
		return
	}
	response, postErr := selected.Connection.Post(ctx, body, http.Header{"Accept": {"application/json"}})
	if response == nil {
		log.Print(postErr)
		return
	}
	defer response.Body.Close()
	const limit = 16 << 20
	reply, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(reply) > limit {
		// An incomplete HTTP error body needs a local error representation too.
		log.Printf("incomplete response: %v; request: %v", err, postErr)
		return
	}
	if postErr != nil {
		fmt.Printf("HTTP %d: %s\n", response.StatusCode, reply)
		return
	}
	observer := chatcompletions.NewObserver(selected.Connection)
	observer.ObserveHeader(response.Header)
	err = observer.ObserveResponse(reply)
	log.Printf("usage: %+v", observer.Usage()) // Also available on an in-band error.
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Printf("%s\n", reply)
}

func ExampleModel_stream() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	selected, err := provider.Open(ctx, provider.Config{URI: "deepseek/deepseek-flash", API: "chat_completions"})
	if err != nil {
		log.Print(err)
		return
	}
	body, err := json.Marshal(map[string]any{
		"model": selected.Connection.APIModel(), "stream": true,
		"messages":       []map[string]string{{"role": "user", "content": "Hello"}},
		"stream_options": map[string]bool{"include_usage": true},
	})
	if err != nil {
		log.Print(err)
		return
	}
	response, err := selected.Connection.Post(ctx, body, http.Header{"Accept": {"text/event-stream"}})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		log.Print(err)
		return
	}
	stream := transport.OpenEventStream(ctx, response.Body, transport.StreamOptions{
		IdleTimeout: 30 * time.Second, MaxEventBytes: 1 << 20,
	})
	defer stream.Close()
	observer := chatcompletions.NewObserver(selected.Connection)
	observer.ObserveHeader(response.Header)
	remaining := 16 << 20
	client := os.Stdout // An HTTP client writer also needs flushing and a write deadline.
read:
	for {
		var event transport.Event
		event, err = stream.Next()
		if errors.Is(err, io.EOF) {
			err = observer.End(stream.SawDone())
			break
		}
		if err != nil {
			break
		}
		if len(event.Data) > remaining {
			err = errors.New("response too large")
			break
		}
		remaining -= len(event.Data)
		if err = observer.ObserveChunk(event.Data); err != nil {
			break
		}
		if event.Name != "" {
			if _, err = fmt.Fprintf(client, "event: %s\n", event.Name); err != nil {
				break
			}
		}
		for line := range bytes.SplitSeq(event.Data, []byte("\n")) {
			if _, err = fmt.Fprintf(client, "data: %s\n", line); err != nil {
				break read
			}
		}
		if _, err = io.WriteString(client, "\n"); err != nil {
			break
		}
	}
	log.Printf("usage: %+v", observer.Usage()) // Account before finalizing, including on errors.
	if err != nil {
		log.Print(err)
		return
	}
	if _, err := io.WriteString(client, "data: [DONE]\n\n"); err != nil {
		log.Print(err)
	}
}
