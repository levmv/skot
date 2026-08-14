package modelhttp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
)

func TestEventStreamFraming(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     []string
		wantErr  error
		wantDone bool
	}{
		{
			name: "one data line per event",
			body: "data: {\"a\":1}\n\ndata: {\"a\":2}\n\n",
			want: []string{`{"a":1}`, `{"a":2}`},
		},
		{
			name: "carriage returns and comments",
			body: ": keep-alive\r\ndata: {\"a\":1}\r\n\r\n",
			want: []string{`{"a":1}`},
		},
		{
			name: "multiple data lines join into one event",
			body: "data: {\"a\":\ndata: 1}\n\ndata: {\"a\":2}\n\n",
			want: []string{"{\"a\":\n1}", `{"a":2}`},
		},
		{
			name: "comment inside an event does not split it",
			body: "data: {\"a\":\n: still working\ndata: 1}\n\n",
			want: []string{"{\"a\":\n1}"},
		},
		{
			name: "named events keep only their data",
			body: "event: message_stop\nid: 7\ndata: {\"type\":\"message_stop\"}\n\n",
			want: []string{`{"type":"message_stop"}`},
		},
		{
			name: "final event without a trailing blank line",
			body: "data: {\"a\":1}\n",
			want: []string{`{"a":1}`},
		},
		{
			name:     "done sentinel ends the stream",
			body:     "data: {\"a\":1}\n\ndata: [DONE]\n\ndata: {\"a\":2}\n\n",
			want:     []string{`{"a":1}`},
			wantDone: true,
		},
		{
			name:    "plain body is reported as one error",
			body:    "upstream proxy failure\n",
			wantErr: errors.New("upstream proxy failure"),
		},
		{
			name: "empty data lines dispatch nothing",
			body: "data:\n\ndata: {\"a\":1}\n\n",
			want: []string{`{"a":1}`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := OpenEventStream(context.Background(), strings.NewReader(test.body), 0)
			defer stream.Close()
			var got []string
			var err error
			for {
				var payload []byte
				payload, err = stream.Next()
				if err != nil {
					break
				}
				got = append(got, string(payload))
			}
			if test.wantErr != nil {
				if err == nil || err.Error() != test.wantErr.Error() {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
			} else if !errors.Is(err, io.EOF) {
				t.Fatalf("error = %v, want EOF", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("events = %q, want %q", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("event %d = %q, want %q", index, got[index], test.want[index])
				}
			}
			if stream.SawDone() != test.wantDone {
				t.Fatalf("SawDone = %v, want %v", stream.SawDone(), test.wantDone)
			}
		})
	}
}

// Keep-alive comments are the only sign of life some gateways send while a
// model reasons. They must hold the idle deadline open without reaching the
// adapter as an event.
func TestEventStreamCommentsExtendIdleDeadline(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	pulses := 6
	interval := 10 * time.Millisecond
	go func() {
		defer writer.Close()
		for range pulses {
			time.Sleep(interval)
			if _, err := io.WriteString(writer, ": OPENROUTER PROCESSING\n\n"); err != nil {
				return
			}
		}
		_, _ = io.WriteString(writer, "data: {\"a\":1}\n\n")
	}()

	stream := OpenEventStream(context.Background(), reader, 3*interval)
	defer stream.Close()
	payload, err := stream.Next()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if string(payload) != `{"a":1}` {
		t.Fatalf("payload = %q", payload)
	}
}

func TestEventStreamReportsIdleTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()

	stream := OpenEventStream(context.Background(), reader, 20*time.Millisecond)
	defer stream.Close()
	if _, err := stream.Next(); !errors.Is(err, agent.ErrModelStreamIdle) {
		t.Fatalf("error = %v, want idle timeout", err)
	}
}

func TestEventStreamBoundsOneEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", productlimits.MaxSSETokenBytes/2) + "\n"
	stream := OpenEventStream(context.Background(), strings.NewReader(line+line+line+"\n"), 0)
	defer stream.Close()
	if _, err := stream.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("error = %v, want %v", err, ErrEventTooLarge)
	}
}

func TestEventStreamStopsOnCancelledContext(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream := OpenEventStream(ctx, reader, 0)
	defer stream.Close()
	cancel()
	if _, err := stream.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}
