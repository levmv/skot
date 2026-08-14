package modelhttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
)

// ErrEventTooLarge reports one event above the local output ceiling. Adapters
// treat it as a locally truncated completion rather than a provider failure.
var ErrEventTooLarge = errors.New("SSE event exceeds the local output limit")

const (
	initialEventBufferBytes = 1024
	doneSentinel            = "[DONE]"
)

// EventStream reads one streamed provider response. It owns SSE framing, the
// idle deadline, and cancellation, so an adapter only decodes payloads of its
// own protocol.
//
// Comments are keep-alive traffic: they carry no data but prove the stream is
// alive, so they refresh the idle deadline without reaching the adapter.
type EventStream struct {
	ctx      context.Context
	stop     context.CancelFunc
	results  <-chan eventReadResult
	idle     time.Duration
	timer    *time.Timer
	deadline <-chan time.Time
	sawDone  bool
}

// OpenEventStream starts reading body in the background. The caller must call
// Close. A non-positive idleTimeout leaves the stream without a deadline.
func OpenEventStream(ctx context.Context, body io.Reader, idleTimeout time.Duration) *EventStream {
	readCtx, stop := context.WithCancel(ctx)
	stream := &EventStream{
		ctx:     ctx,
		stop:    stop,
		results: readEvents(readCtx, newEventReader(body)),
		idle:    idleTimeout,
	}
	if idleTimeout > 0 {
		stream.timer = time.NewTimer(idleTimeout)
		stream.deadline = stream.timer.C
	}
	return stream
}

// Close stops the background reader. It does not close the underlying body.
func (stream *EventStream) Close() {
	stream.stop()
	if stream.timer != nil {
		stream.timer.Stop()
	}
}

// Next returns the payload of the next event. It reports io.EOF at the end of
// the stream, ErrEventTooLarge for an event above the local ceiling, and a
// wrapped agent.ErrModelStreamIdle when the provider goes quiet.
func (stream *EventStream) Next() ([]byte, error) {
	for {
		select {
		case result, ok := <-stream.results:
			if !ok {
				if err := stream.ctx.Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("provider stream reader stopped without a result")
			}
			stream.extendDeadline()
			if result.done {
				stream.sawDone = true
			}
			if result.pulse {
				continue
			}
			return result.payload, result.err
		case <-stream.deadline:
			return nil, fmt.Errorf("%w after %s", agent.ErrModelStreamIdle, stream.idle)
		case <-stream.ctx.Done():
			return nil, stream.ctx.Err()
		}
	}
}

// SawDone reports whether the provider ended the stream with an explicit
// [DONE] sentinel instead of closing the response body. Protocols that define
// no such sentinel never observe it.
func (stream *EventStream) SawDone() bool {
	return stream.sawDone
}

func (stream *EventStream) extendDeadline() {
	if stream.timer == nil {
		return
	}
	if !stream.timer.Stop() {
		select {
		case <-stream.timer.C:
		default:
		}
	}
	stream.timer.Reset(stream.idle)
}

type eventReadResult struct {
	payload []byte
	pulse   bool
	done    bool
	err     error
}

// eventReader turns SSE lines into dispatched events. Its state spans calls
// because one event may be interrupted by a comment.
type eventReader struct {
	scanner *bufio.Scanner
	data    bytes.Buffer
	raw     bytes.Buffer
	done    bool
}

func readEvents(ctx context.Context, reader *eventReader) <-chan eventReadResult {
	results := make(chan eventReadResult, 1)
	go func() {
		defer close(results)
		for {
			result := reader.next()
			select {
			case results <- result:
			case <-ctx.Done():
				return
			}
			if result.err != nil {
				return
			}
		}
	}()
	return results
}

func newEventReader(reader io.Reader) *eventReader {
	scanner := bufio.NewScanner(reader)
	// Most provider events are small deltas. Let Scanner grow for exceptional
	// events instead of reserving a large buffer for every model response.
	scanner.Buffer(make([]byte, initialEventBufferBytes), productlimits.MaxSSETokenBytes+1)
	scanner.Split(scanBoundedEventLines)
	return &eventReader{scanner: scanner}
}

func scanBoundedEventLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if bytes.IndexByte(data, '\n') < 0 && len(data) >= productlimits.MaxSSETokenBytes {
		return 0, nil, ErrEventTooLarge
	}
	return bufio.ScanLines(data, atEOF)
}

func (reader *eventReader) next() eventReadResult {
	if reader.done {
		return eventReadResult{done: true, err: io.EOF}
	}
	for reader.scanner.Scan() {
		line := bytes.TrimSuffix(reader.scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			if reader.data.Len() == 0 {
				continue
			}
			return reader.dispatch()
		}
		if line[0] == ':' {
			return eventReadResult{pulse: true}
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			// Not SSE framing at all. Keep the body so a provider that answers
			// a stream request with plain text still produces one readable error.
			if reader.raw.Len()+len(line) > productlimits.MaxModelCompletionBytes {
				return eventReadResult{err: ErrEventTooLarge}
			}
			reader.raw.Write(line)
			continue
		}
		if !bytes.Equal(field, []byte("data")) {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if reader.data.Len()+len(value)+1 > productlimits.MaxSSETokenBytes {
			return eventReadResult{err: ErrEventTooLarge}
		}
		if reader.data.Len() != 0 {
			reader.data.WriteByte('\n')
		}
		reader.data.Write(value)
	}
	if err := reader.scanner.Err(); err != nil {
		return eventReadResult{err: err}
	}
	// A provider may close the body directly after its last data line.
	if reader.data.Len() != 0 {
		return reader.dispatch()
	}
	if reader.raw.Len() != 0 {
		return eventReadResult{err: errors.New(reader.raw.String())}
	}
	return eventReadResult{err: io.EOF}
}

func (reader *eventReader) dispatch() eventReadResult {
	payload := append([]byte(nil), reader.data.Bytes()...)
	reader.data.Reset()
	if bytes.Equal(payload, []byte(doneSentinel)) {
		reader.done = true
		return eventReadResult{done: true, err: io.EOF}
	}
	return eventReadResult{payload: payload}
}
