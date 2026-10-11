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

// ErrDrainComplete reports that a local drain limit ended the stream.
// It is distinct from EOF, upstream errors, idle timeout, and cancellation.
var ErrDrainComplete = errors.New("SSE trailing-event drain completed")

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

// DrainOptions bounds reading after the caller has validated protocol-level
// completion. All fields must be positive; transport supplies no defaults.
type DrainOptions struct {
	// MinComments is the required number of comment lines since the last data
	// field. Comments inside an unfinished data event do not count.
	MinComments int
	// QuietPeriod is measured from BeginDrain or the latest data field.
	// Both QuietPeriod and MinComments must be satisfied. An unfinished line
	// or data event prevents this early exit.
	QuietPeriod time.Duration
	// MaxDuration is an absolute limit from the first BeginDrain call,
	// including time spent by the consumer between calls to Next.
	MaxDuration time.Duration
}

// EventStream owns a response body; call Close on every exit. Use Next,
// BeginDrain, and SawDone from one goroutine. Close may run concurrently.
type EventStream struct {
	ctx          context.Context
	stop         context.CancelFunc
	body         io.ReadCloser
	activity     *activityReader
	reader       *eventReader
	requests     chan struct{}
	changed      chan struct{}
	finished     chan struct{}
	drainExpired chan struct{}
	idle         time.Duration

	// Parsing progress, result publication, and drain expiry share this lock.
	// A completed result survives local expiry; no further events are parsed.
	mu          sync.Mutex
	result      eventReadResult
	ready       bool
	endCause    error // Completion cause; a parsed result is returned first.
	partialLine bool
	drain       *eventDrain
	drainTimer  *time.Timer

	sawDone   bool
	terminal  error // The terminal status already returned by Next.
	closeOnce sync.Once
	closeErr  error
}

type eventDrain struct {
	options  DrainOptions
	deadline time.Time
	lastData time.Time
	comments int
}

// OpenEventStream takes ownership of body. Its Close must interrupt Read
// without draining the stream; Connection.Post provides such a body.
// Cancellation interrupts Next; Close releases the reader.
func OpenEventStream(ctx context.Context, body io.ReadCloser, options StreamOptions) *EventStream {
	readCtx, stop := context.WithCancel(ctx)
	activity := &activityReader{reader: body}
	limit := options.MaxEventBytes
	if limit <= 0 {
		limit = productlimits.MaxSSETokenBytes
	}
	stream := &EventStream{
		ctx:          readCtx,
		stop:         stop,
		body:         body,
		activity:     activity,
		requests:     make(chan struct{}, 1),
		changed:      make(chan struct{}, 1),
		finished:     make(chan struct{}),
		drainExpired: make(chan struct{}),
		idle:         options.IdleTimeout,
	}
	stream.reader = newEventReader(activity, limit, stream.notePartialLine)
	go stream.readEvents()
	return stream
}

// Close stops background reading and closes the body. Repeated calls are safe.
func (stream *EventStream) Close() error {
	stream.stop()
	stream.mu.Lock()
	if stream.drainTimer != nil {
		stream.drainTimer.Stop()
	}
	stream.mu.Unlock()
	stream.closeBody()
	<-stream.finished
	return stream.closeErr
}

func (stream *EventStream) closeBody() {
	stream.closeOnce.Do(func() { stream.closeErr = stream.body.Close() })
}

// BeginDrain starts bounded reading of trailing events. The first call fixes
// the options and absolute deadline; subsequent calls are no-ops. New data
// restarts only the quiet period and comment count. EOF and [DONE] still end
// normally. Local expiry closes the body, preserves any already assembled
// event, then makes Next return ErrDrainComplete without setting SawDone.
func (stream *EventStream) BeginDrain(options DrainOptions) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.drain != nil {
		return nil
	}
	if options.MinComments <= 0 || options.QuietPeriod <= 0 || options.MaxDuration <= 0 {
		return errors.New("SSE drain requires positive MinComments, QuietPeriod, and MaxDuration")
	}
	if err := stream.contextErr(); err != nil {
		return err
	}
	if stream.endCause != nil || stream.terminal != nil {
		return nil
	}
	now := time.Now()
	stream.drain = &eventDrain{options: options, deadline: now.Add(options.MaxDuration), lastData: now}
	stream.drainTimer = time.AfterFunc(options.MaxDuration, stream.expireDrain)
	return nil
}

func (stream *EventStream) drainDeadlineLocked() time.Time {
	drain := stream.drain
	deadline := drain.deadline
	if drain.comments >= drain.options.MinComments && !stream.reader.hasData && !stream.partialLine {
		if quiet := drain.lastData.Add(drain.options.QuietPeriod); quiet.Before(deadline) {
			deadline = quiet
		}
	}
	return deadline
}

// updateDrainLocked runs after parsing as well as from the timer. Checking
// here prevents a busy stream from postponing expiry by repeatedly resetting
// an already due timer. The caller closes the body after releasing mu.
func (stream *EventStream) updateDrainLocked() bool {
	if stream.drain == nil {
		return false
	}
	if stream.endCause != nil || stream.ctx.Err() != nil {
		stream.drainTimer.Stop()
		return false
	}
	if remaining := time.Until(stream.drainDeadlineLocked()); remaining > 0 {
		stream.drainTimer.Reset(remaining)
		return false
	}
	stream.endCause = ErrDrainComplete
	close(stream.drainExpired)
	return true
}

func (stream *EventStream) expireDrain() {
	stream.mu.Lock()
	expired := stream.updateDrainLocked()
	stream.mu.Unlock()
	// Do not cancel ctx: local completion must not become context.Canceled.
	if expired {
		stream.closeBody()
		stream.notify()
	}
}

func (stream *EventStream) notePartialLine() {
	stream.mu.Lock()
	stream.partialLine = true
	stream.mu.Unlock()
}

// Next skips comments and returns the next event, io.EOF, ErrEventTooLarge,
// ErrDrainComplete, model.ErrModelStreamIdle, or a read/cancellation error.
// [DONE] becomes EOF with SawDone set.
func (stream *EventStream) Next() (Event, error) {
	if err := stream.contextErr(); err != nil {
		return Event{}, err
	}
	if stream.terminal != nil {
		return Event{}, stream.terminal
	}
	stream.expireDrain()
	if event, err, ready := stream.takeResult(); ready {
		return event, err
	}
	// The reader pauses after each event. BeginDrain can therefore run before
	// comments following that event are consumed, even within the same Read.
	stream.requests <- struct{}{}
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
		case <-stream.changed:
			if event, err, ready := stream.takeResult(); ready {
				return event, err
			}
		case <-deadline:
			// A ready result wins; at coincident deadlines bounded drain wins
			// over idle timeout. The parent context still takes precedence.
			stream.expireDrain()
			if event, err, ready := stream.takeResult(); ready {
				return event, err
			}
			last := stream.activity.lastRead()
			if last.Before(started) {
				last = started
			}
			if remaining := stream.idle - time.Since(last); remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			if err := stream.contextErr(); err != nil {
				return Event{}, err
			}
			stream.terminal = fmt.Errorf("%w after %s", model.ErrModelStreamIdle, stream.idle)
			return Event{}, stream.terminal
		case <-stream.ctx.Done():
			return Event{}, stream.ctx.Err()
		}
	}
}

func (stream *EventStream) contextErr() error {
	if err := stream.ctx.Err(); err != nil {
		return err
	}
	// The parent's deadline also wins if its cancellation callback has not
	// been scheduled yet alongside our drain or idle timer.
	if deadline, ok := stream.ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (stream *EventStream) takeResult() (Event, error, bool) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if err := stream.contextErr(); err != nil {
		return Event{}, err, true
	}
	if stream.ready {
		result := stream.result
		stream.result, stream.ready = eventReadResult{}, false
		stream.sawDone, stream.terminal = result.done, result.err
		return result.event, result.err, true
	}
	if stream.endCause != nil {
		stream.terminal = stream.endCause
		return Event{}, stream.terminal, true
	}
	return Event{}, nil, false
}

// SawDone reports whether Next consumed an explicit [DONE] sentinel.
func (stream *EventStream) SawDone() bool { return stream.sawDone }

func (stream *EventStream) notify() {
	select {
	case stream.changed <- struct{}{}:
	default:
	}
}

func (stream *EventStream) readEvents() {
	defer close(stream.finished)
	for {
		select {
		case <-stream.ctx.Done():
			return
		case <-stream.drainExpired:
			return
		case <-stream.requests:
		}
		for {
			ok := stream.reader.scanner.Scan()
			stream.mu.Lock()
			if stream.endCause != nil || stream.ctx.Err() != nil {
				stream.mu.Unlock()
				return
			}
			stream.partialLine = false
			var result eventReadResult
			if ok {
				result = stream.reader.consumeLine(stream.reader.scanner.Bytes(), stream.drain)
			} else {
				result = stream.reader.end()
			}
			ready := result.err != nil || result.event.Data != nil
			if ready {
				stream.result, stream.ready = result, true
				stream.endCause = result.err
			}
			expired := stream.updateDrainLocked()
			stream.mu.Unlock()
			if expired {
				stream.closeBody()
			}
			if ready || expired {
				stream.notify()
			}
			if result.err != nil || expired {
				return
			}
			if ready {
				break
			}
		}
	}
}

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
}

func newEventReader(reader io.Reader, limit int, onPartialLine func()) *eventReader {
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
		advance, token, err := bufio.ScanLines(data, atEOF)
		if advance == 0 && len(data) > 0 && err == nil {
			// A partial line may be the start of the next data event.
			onPartialLine()
		}
		return advance, token, err
	})
	return &eventReader{scanner: scanner, limit: limit}
}

func (reader *eventReader) consumeLine(line []byte, drain *eventDrain) eventReadResult {
	if len(line) == 0 {
		if reader.data.Len() != 0 {
			return reader.dispatch()
		}
		reader.name = ""
		reader.hasData = false
		return eventReadResult{}
	}
	if line[0] == ':' {
		if drain != nil && !reader.hasData && drain.comments < drain.options.MinComments {
			drain.comments++
		}
		return eventReadResult{}
	}
	field, value, found := bytes.Cut(line, []byte(":"))
	if !found {
		// Retain a bounded fallback for non-SSE plain-text failures.
		if reader.raw.Len()+len(line) > reader.limit {
			return eventReadResult{err: ErrEventTooLarge}
		}
		reader.raw.Write(line)
		return eventReadResult{}
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(field) {
	case "event":
		if reader.data.Len()+len(value) > reader.limit {
			return eventReadResult{err: ErrEventTooLarge}
		}
		reader.name = string(value)
	case "data":
		if drain != nil {
			drain.lastData = time.Now()
			drain.comments = 0
		}
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
	return eventReadResult{}
}

func (reader *eventReader) end() eventReadResult {
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
		return eventReadResult{done: true, err: io.EOF}
	}
	return eventReadResult{event: Event{Name: name, Data: payload}}
}
