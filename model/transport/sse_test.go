package transport

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/model"
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
			name: "leading empty data line is preserved",
			body: "data:\ndata: text\n\n",
			want: []string{"\ntext"},
		},
		{
			name: "comment inside an event does not split it",
			body: "data: {\"a\":\n: still working\ndata: 1}\n\n",
			want: []string{"{\"a\":\n1}"},
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
			stream := OpenEventStream(context.Background(), io.NopCloser(strings.NewReader(test.body)), StreamOptions{})
			defer stream.Close()
			var got []string
			var err error
			for {
				var payload Event
				payload, err = stream.Next()
				if err != nil {
					break
				}
				got = append(got, string(payload.Data))
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
	synctest.Test(t, testEventStreamCommentsExtendIdleDeadline)
}

func testEventStreamCommentsExtendIdleDeadline(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	pulses := 6
	interval := 10 * time.Millisecond
	go func() {
		defer writer.Close()
		for range pulses {
			synctest.Sleep(interval)
			if _, err := io.WriteString(writer, ": OPENROUTER PROCESSING\n\n"); err != nil {
				return
			}
		}
		_, _ = io.WriteString(writer, "data: {\"a\":1}\n\n")
	}()

	stream := OpenEventStream(context.Background(), reader, StreamOptions{IdleTimeout: 3 * interval})
	defer stream.Close()
	payload, err := stream.Next()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if string(payload.Data) != `{"a":1}` {
		t.Fatalf("payload = %q", payload)
	}
}

func TestEventStreamReportsIdleTimeout(t *testing.T) {
	synctest.Test(t, testEventStreamReportsIdleTimeout)
}

func testEventStreamReportsIdleTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()

	stream := OpenEventStream(context.Background(), reader, StreamOptions{IdleTimeout: 20 * time.Millisecond})
	defer stream.Close()
	if _, err := stream.Next(); !errors.Is(err, model.ErrModelStreamIdle) {
		t.Fatalf("error = %v, want idle timeout", err)
	}
}

func TestEventStreamBoundsOneEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", productlimits.MaxSSETokenBytes/2) + "\n"
	stream := OpenEventStream(context.Background(), io.NopCloser(strings.NewReader(line+line+line+"\n")), StreamOptions{})
	defer stream.Close()
	if _, err := stream.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("error = %v, want %v", err, ErrEventTooLarge)
	}
}

func TestEventStreamPreservesNamesAndOwnedData(t *testing.T) {
	body := "event: custom\ndata: first\ndata: second\n\ndata: third\n\nevent: message\ndata: fourth\n\n"
	stream := OpenEventStream(t.Context(), io.NopCloser(strings.NewReader(body)), StreamOptions{})
	defer stream.Close()
	var events []Event
	for {
		event, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	want := []Event{{Name: "custom", Data: []byte("first\nsecond")}, {Data: []byte("third")}, {Name: "message", Data: []byte("fourth")}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v", events)
	}
}

func TestEventStreamConsumerTimeDoesNotCountAsIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		stream := OpenEventStream(t.Context(), reader, StreamOptions{IdleTimeout: time.Second})
		defer stream.Close()
		for _, want := range []string{"first", "second"} {
			synctest.Sleep(10 * time.Second)
			// Deliver after Next starts: an expired timer left running during
			// consumer work must fail deterministically, without a select race.
			go func() {
				synctest.Sleep(500 * time.Millisecond)
				_, _ = io.WriteString(writer, "data: "+want+"\n\n")
			}()
			event, err := stream.Next()
			if err != nil || string(event.Data) != want {
				t.Fatalf("event=%q error=%v", event.Data, err)
			}
		}
	})
}

func TestEventStreamIncompleteLineKeepsStreamAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			for _, part := range []string{"data: ", "a", "b", "c", "d", "\n\n"} {
				synctest.Sleep(time.Second)
				if _, err := io.WriteString(writer, part); err != nil {
					return
				}
			}
		}()
		stream := OpenEventStream(t.Context(), reader, StreamOptions{IdleTimeout: 3 * time.Second})
		defer stream.Close()
		event, err := stream.Next()
		if err != nil || string(event.Data) != "abcd" {
			t.Fatalf("event=%q error=%v", event.Data, err)
		}
	})
}

func TestEventStreamConfiguredLimit(t *testing.T) {
	for _, test := range []struct {
		name, body string
		wantLarge  bool
	}{
		{name: "small", body: "event: hi\ndata: 12\n\n"},
		{name: "combined name and data", body: "event: name\ndata: 1234567890\ndata: 1234\n\n", wantLarge: true},
		{name: "name after data", body: "data: 1234567890\ndata: 1234\nevent: name\n\n", wantLarge: true},
		{name: "unterminated line", body: strings.Repeat("x", 17), wantLarge: true},
		{name: "terminated line", body: strings.Repeat("x", 17) + "\n", wantLarge: true},
		{name: "raw fallback", body: "1234567890\n1234567890\n", wantLarge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := OpenEventStream(t.Context(), io.NopCloser(strings.NewReader(test.body)), StreamOptions{MaxEventBytes: 16})
			defer stream.Close()
			_, err := stream.Next()
			if errors.Is(err, ErrEventTooLarge) != test.wantLarge || (!test.wantLarge && err != nil) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEventStreamCloseReleasesBodyOnEveryExit(t *testing.T) {
	for _, reason := range []string{"cancelled", "limit", "consumer stopped", "concurrent close"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer writer.Close()
				body := &trackedBody{ReadCloser: reader}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				stream := OpenEventStream(ctx, body, StreamOptions{MaxEventBytes: 16})
				switch reason {
				case "cancelled":
					cancel()
					if _, err := stream.Next(); !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				case "limit":
					go func() { _, _ = io.WriteString(writer, strings.Repeat("x", 17)) }()
					if _, err := stream.Next(); !errors.Is(err, ErrEventTooLarge) {
						t.Fatal(err)
					}
				case "consumer stopped":
					// Fill the queue so Close must also interrupt dispatch.
					go func() { _, _ = io.WriteString(writer, strings.Repeat("data: one\n\n", 4)) }()
					if _, err := stream.Next(); err != nil {
						t.Fatal(err)
					}
				case "concurrent close":
					go func() {
						synctest.Sleep(time.Second)
						_ = stream.Close()
					}()
					if _, err := stream.Next(); !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				for range 2 {
					if err := stream.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if body.closes.Load() != 1 {
					t.Fatalf("body closes = %d", body.closes.Load())
				}
				if _, err := io.WriteString(writer, "more"); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("writer = %v", err)
				}
			})
		})
	}
}
