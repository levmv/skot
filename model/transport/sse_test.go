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
					// Close must release an upstream writer even when the
					// consumer has stopped requesting events.
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

func TestEventStreamDrainTiming(t *testing.T) {
	type write struct {
		at   time.Duration
		text string
	}
	for _, test := range []struct {
		name   string
		writes []write
		want   []string
		end    time.Duration
	}{
		{name: "two quick comments then silence", writes: []write{{time.Second, ": ping\n: ping\n\n"}}, end: 5 * time.Second},
		{name: "spaced comments", writes: []write{{15 * time.Second, ": ping\n\n"}, {30 * time.Second, ": ping\n\n"}}, end: 30 * time.Second},
		{name: "usage between comments", writes: []write{
			{time.Second, ": ping\n\n"}, {3 * time.Second, "data: usage\n\n"},
			{4 * time.Second, ": ping\n\n"}, {5 * time.Second, ": ping\n\n"},
		}, want: []string{"usage"}, end: 8 * time.Second},
		{name: "usage after two comments", writes: []write{
			{time.Second, ": ping\n: ping\n\n"}, {4 * time.Second, "data: usage\n\n"}, {5 * time.Second, ": ping\n: ping\n\n"},
		}, want: []string{"usage"}, end: 9 * time.Second},
		{name: "empty data resets waiting", writes: []write{
			{time.Second, ": ping\n: ping\n\n"}, {4 * time.Second, "data:\n\n"}, {5 * time.Second, ": ping\n: ping\n\n"},
		}, end: 9 * time.Second},
		{name: "silence reaches absolute limit", end: 45 * time.Second},
		{name: "one comment reaches absolute limit", writes: []write{{time.Second, ": ping\n\n"}}, end: 45 * time.Second},
		{name: "data cannot extend absolute limit", writes: []write{
			{10 * time.Second, "data: a\n\n"}, {20 * time.Second, "data: b\n\n"},
			{30 * time.Second, "data: c\n\n"}, {40 * time.Second, "data: d\n\n"},
		}, want: []string{"a", "b", "c", "d"}, end: 45 * time.Second},
		{name: "comments inside an unfinished event", writes: []write{
			{time.Second, "data: first\n: ping\n: ping\n"}, {10 * time.Second, "data: second\n\n"}, {11 * time.Second, ": ping\n: ping\n\n"},
		}, want: []string{"first\nsecond"}, end: 15 * time.Second},
		{name: "unfinished data line", writes: []write{
			{time.Second, ": ping\n: ping\n\n"}, {4 * time.Second, "data: us"},
			{8 * time.Second, "age\n\n"}, {9 * time.Second, ": ping\n: ping\n\n"},
		}, want: []string{"usage"}, end: 13 * time.Second},
		{name: "unfinished event reaches absolute limit", writes: []write{
			{time.Second, "data: partial\n: ping\n: ping\n"},
		}, end: 45 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer writer.Close()
				stream := OpenEventStream(t.Context(), reader, StreamOptions{IdleTimeout: 45 * time.Second})
				defer stream.Close()
				started := time.Now()
				go func() {
					// Earlier heartbeats must not count toward trailing drain.
					if _, err := io.WriteString(writer, ": before\n: before\n\ndata: finish\n\n"); err != nil {
						return
					}
					for _, write := range test.writes {
						synctest.Sleep(write.at - time.Since(started))
						if _, err := io.WriteString(writer, write.text); err != nil {
							return
						}
					}
				}()
				if event, err := stream.Next(); err != nil || string(event.Data) != "finish" {
					t.Fatalf("finish event = %q, %v", event.Data, err)
				}
				if err := stream.BeginDrain(DrainOptions{MinComments: 2, QuietPeriod: 5 * time.Second, MaxDuration: 45 * time.Second}); err != nil {
					t.Fatal(err)
				}
				var got []string
				for {
					event, err := stream.Next()
					if err != nil {
						if !errors.Is(err, ErrDrainComplete) || stream.SawDone() || time.Since(started) != test.end {
							t.Fatalf("end after %s: %v, SawDone=%t", time.Since(started), err, stream.SawDone())
						}
						break
					}
					got = append(got, string(event.Data))
					// A caller may see the same finish reason after every chunk.
					// Re-arming must not replace any of the original options.
					if err := stream.BeginDrain(DrainOptions{MinComments: 100, QuietPeriod: time.Hour, MaxDuration: time.Hour}); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(got, test.want) {
					t.Fatalf("events = %q, want %q", got, test.want)
				}
				if _, err := io.WriteString(writer, "more"); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("body still open after drain: %v", err)
				}
			})
		})
	}
}

func TestEventStreamDrainKeepsBufferedTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		stream := OpenEventStream(t.Context(), reader, StreamOptions{})
		defer stream.Close()
		go func() {
			_, _ = io.WriteString(writer, "data: finish\n\ndata: usage\n\n: ping\n: ping\n\n")
		}()
		if _, err := stream.Next(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // Let any eager reader consume the buffered comments.
		if err := stream.BeginDrain(DrainOptions{MinComments: 2, QuietPeriod: 5 * time.Second, MaxDuration: 45 * time.Second}); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if event, err := stream.Next(); err != nil || string(event.Data) != "usage" {
			t.Fatalf("trailing event = %q, %v", event.Data, err)
		}
		if _, err := stream.Next(); !errors.Is(err, ErrDrainComplete) || time.Since(started) != 5*time.Second || stream.SawDone() {
			t.Fatalf("end after %s: %v, SawDone=%t", time.Since(started), err, stream.SawDone())
		}
	})
}

func TestEventStreamDrainPreservesTerminationCause(t *testing.T) {
	for _, test := range []struct {
		name, text string
		readErr    error
		deadline   time.Duration
		idle       time.Duration
		wantErr    error
		wantDone   bool
	}{
		{name: "done", text: "data: [DONE]\n\n", wantErr: io.EOF, wantDone: true},
		{name: "EOF", readErr: io.EOF, wantErr: io.EOF},
		{name: "read error", readErr: io.ErrUnexpectedEOF, wantErr: io.ErrUnexpectedEOF},
		{name: "earlier idle", idle: 3 * time.Second, wantErr: model.ErrModelStreamIdle},
		{name: "parent at quiet deadline", text: ": ping\n: ping\n\n", deadline: 5 * time.Second, wantErr: context.DeadlineExceeded},
		{name: "parent at absolute deadline", deadline: 45 * time.Second, wantErr: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if test.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, test.deadline)
					defer cancel()
				}
				reader, writer := io.Pipe()
				defer writer.Close()
				stream := OpenEventStream(ctx, reader, StreamOptions{IdleTimeout: test.idle})
				defer stream.Close()
				if err := stream.BeginDrain(DrainOptions{MinComments: 2, QuietPeriod: 5 * time.Second, MaxDuration: 45 * time.Second}); err != nil {
					t.Fatal(err)
				}
				go func() {
					synctest.Sleep(time.Second)
					if test.text != "" {
						_, _ = io.WriteString(writer, test.text)
					}
					if test.readErr != nil {
						_ = writer.CloseWithError(test.readErr)
					}
				}()
				if _, err := stream.Next(); !errors.Is(err, test.wantErr) || stream.SawDone() != test.wantDone {
					t.Fatalf("end = %v, SawDone=%t", err, stream.SawDone())
				}
			})
		})
	}
}

func TestEventStreamDrainPreservesReadyResult(t *testing.T) {
	for _, test := range []struct {
		name    string
		readErr error
		cancel  bool
	}{
		{name: "usage"},
		{name: "read error", readErr: io.ErrUnexpectedEOF},
		{name: "cancellation overrides ready usage", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reader, writer := io.Pipe()
				defer writer.Close()
				stream := OpenEventStream(ctx, reader, StreamOptions{})
				defer stream.Close()
				if err := stream.BeginDrain(DrainOptions{MinComments: 2, QuietPeriod: 5 * time.Second, MaxDuration: 45 * time.Second}); err != nil {
					t.Fatal(err)
				}
				go func() {
					if test.readErr != nil {
						_ = writer.CloseWithError(test.readErr)
					} else {
						_, _ = io.WriteString(writer, "data: usage\n\n")
					}
				}()
				// Stage the point in Next after requesting a read but before
				// receiving its result. Both the result and timer are then ready
				// when the consumer resumes, without relying on scheduling luck.
				stream.requests <- struct{}{}
				synctest.Wait()
				synctest.Sleep(45 * time.Second)
				if test.cancel {
					cancel()
				}
				event, err := stream.Next()
				switch {
				case test.cancel:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled end = %v", err)
					}
				case test.readErr != nil:
					if !errors.Is(err, test.readErr) {
						t.Fatalf("upstream end = %v", err)
					}
				default:
					if err != nil || string(event.Data) != "usage" {
						t.Fatalf("ready event = %q, %v", event.Data, err)
					}
					if _, err := stream.Next(); !errors.Is(err, ErrDrainComplete) || stream.SawDone() {
						t.Fatalf("local end = %v, SawDone=%t", err, stream.SawDone())
					}
				}
			})
		})
	}
}
