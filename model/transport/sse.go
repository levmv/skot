package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/model"
)

// ErrEventTooLarge reports an event or line above MaxEventBytes.
var ErrEventTooLarge = errors.New("SSE event exceeds the local output limit")

const initialEventBufferBytes = 1024

// Event contains one SSE event. Data belongs to the caller and remains valid
// after Next. Name is empty when no event name was supplied; no default is added.
type Event struct {
	Name string
	Data []byte
}

type StreamOptions struct {
	// IdleTimeout limits silence while Next waits for upstream. Any bytes,
	// including comments and incomplete lines, refresh it. <= 0 disables it.
	IdleTimeout time.Duration
	// MaxEventBytes bounds accumulated data plus the event name, and each line.
	// <= 0 selects Skot's default (16 MiB + 64 KiB).
	MaxEventBytes int
}

// EventStream owns a response body; call Close on every exit. Use Next and
// SawDone from one goroutine. Close may run concurrently to interrupt a read.
type EventStream struct {
	ctx       context.Context
	stop      context.CancelFunc
	body      io.ReadCloser
	activity  *activityReader
	results   <-chan eventReadResult
	finished  <-chan struct{}
	idle      time.Duration
	sawDone   bool
	terminal  error
	closeOnce sync.Once
	closeErr  error
}

// OpenEventStream takes ownership of body. Its Close must unblock Read, as HTTP
// response bodies do. Cancellation interrupts Next; Close releases the reader.
func OpenEventStream(ctx context.Context, body io.ReadCloser, options StreamOptions) *EventStream {
	readCtx, stop := context.WithCancel(ctx)
	activity := &activityReader{reader: body}
	limit := options.MaxEventBytes
	if limit <= 0 {
		limit = productlimits.MaxSSETokenBytes
	}
	results, finished := readEvents(readCtx, newEventReader(activity, limit))
	return &EventStream{
		ctx: readCtx, stop: stop, body: body, activity: activity,
		results: results, finished: finished, idle: options.IdleTimeout,
	}
}

// Close stops background reading and closes the body. Repeated calls are safe.
func (stream *EventStream) Close() error {
	stream.closeOnce.Do(func() {
		stream.stop()
		stream.closeErr = stream.body.Close()
		<-stream.finished
	})
	return stream.closeErr
}

// Next skips comments and returns the next event, io.EOF, ErrEventTooLarge,
// model.ErrModelStreamIdle, or a read/cancellation error. [DONE] becomes EOF
// with SawDone set.
func (stream *EventStream) Next() (Event, error) {
	if err := stream.ctx.Err(); err != nil {
		return Event{}, err
	}
	if stream.terminal != nil {
		return Event{}, stream.terminal
	}
	started := time.Now()
	var deadline <-chan time.Time
	var timer *time.Timer
	if stream.idle > 0 {
		timer = time.NewTimer(stream.idle)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case result := <-stream.results:
			return stream.accept(result)
		case <-deadline:
			// A ready event wins even when scheduling delayed this consumer.
			select {
			case result := <-stream.results:
				return stream.accept(result)
			default:
			}
			last := stream.activity.lastRead()
			if last.Before(started) {
				last = started
			}
			if remaining := stream.idle - time.Since(last); remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			stream.terminal = fmt.Errorf("%w after %s", model.ErrModelStreamIdle, stream.idle)
			return Event{}, stream.terminal
		case <-stream.ctx.Done():
			return Event{}, stream.ctx.Err()
		}
	}
}

func (stream *EventStream) accept(result eventReadResult) (Event, error) {
	// The reader closes its channel only after a terminal result or cancellation.
	if err := stream.ctx.Err(); err != nil {
		return Event{}, err
	}
	stream.sawDone = result.done
	stream.terminal = result.err
	return result.event, result.err
}

// SawDone reports whether Next consumed an explicit [DONE] sentinel.
func (stream *EventStream) SawDone() bool { return stream.sawDone }

// Partial lines also prove upstream activity. Preserve monotonic time while
// sharing read progress with the consumer's idle timer.
type activityReader struct {
	reader io.Reader
	mu     sync.Mutex
	last   time.Time
}

func (reader *activityReader) Read(p []byte) (int, error) {
	n, err := reader.reader.Read(p)
	if n > 0 {
		reader.mu.Lock()
		reader.last = time.Now()
		reader.mu.Unlock()
	}
	return n, err
}

func (reader *activityReader) lastRead() time.Time {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.last
}

type eventReadResult struct {
	event Event
	done  bool
	err   error
}

type eventReader struct {
	scanner *bufio.Scanner
	limit   int
	data    bytes.Buffer
	raw     bytes.Buffer
	name    string
	hasData bool
	done    bool
}

func readEvents(ctx context.Context, reader *eventReader) (<-chan eventReadResult, <-chan struct{}) {
	results := make(chan eventReadResult, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer close(results)
		for ctx.Err() == nil {
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
	return results, finished
}

func newEventReader(reader io.Reader, limit int) *eventReader {
	scanner := bufio.NewScanner(reader)
	// Most events are small. Grow only for exceptional events. The split
	// function rejects oversized lines, including those already in the buffer.
	bufferLimit := limit
	if bufferLimit < int(^uint(0)>>1) {
		bufferLimit++ // Room to detect a line one byte over the limit.
	}
	scanner.Buffer(make([]byte, min(initialEventBufferBytes, bufferLimit)), bufferLimit)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		length := bytes.IndexByte(data, '\n')
		if length < 0 {
			length = len(data)
		}
		if length > limit {
			return 0, nil, ErrEventTooLarge
		}
		return bufio.ScanLines(data, atEOF)
	})
	return &eventReader{scanner: scanner, limit: limit}
}

func (reader *eventReader) next() eventReadResult {
	if reader.done {
		return eventReadResult{done: true, err: io.EOF}
	}
	for reader.scanner.Scan() {
		line := reader.scanner.Bytes()
		if len(line) == 0 {
			if reader.data.Len() != 0 {
				return reader.dispatch()
			}
			reader.name = ""
			reader.hasData = false
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			// Retain a bounded fallback for non-SSE plain-text failures.
			if reader.raw.Len()+len(line) > reader.limit {
				return eventReadResult{err: ErrEventTooLarge}
			}
			reader.raw.Write(line)
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			if reader.data.Len()+len(value) > reader.limit {
				return eventReadResult{err: ErrEventTooLarge}
			}
			reader.name = string(value)
		case "data":
			separator := 0
			if reader.hasData {
				separator = 1
			}
			if reader.data.Len()+separator+len(value)+len(reader.name) > reader.limit {
				return eventReadResult{err: ErrEventTooLarge}
			}
			if separator != 0 {
				reader.data.WriteByte('\n')
			}
			reader.data.Write(value)
			reader.hasData = true
		}
	}
	if err := reader.scanner.Err(); err != nil {
		return eventReadResult{err: err}
	}
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
	reader.hasData = false
	name := reader.name
	reader.name = ""
	if bytes.Equal(payload, []byte("[DONE]")) {
		reader.done = true
		return eventReadResult{done: true, err: io.EOF}
	}
	return eventReadResult{event: Event{Name: name, Data: payload}}
}
