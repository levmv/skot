package chatcompletions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	productlimits "github.com/levmv/skot/internal/limits"
)

type sseReader struct {
	scanner *bufio.Scanner
	done    bool
}

type sseReadResult struct {
	payload []byte
	err     error
}

func readSSE(ctx context.Context, reader *sseReader) <-chan sseReadResult {
	results := make(chan sseReadResult, 1)
	go func() {
		defer close(results)
		for {
			payload, err := reader.next()
			select {
			case results <- sseReadResult{payload: payload, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results
}

var errSSETokenTooLarge = errors.New("SSE event exceeds the local output limit")

const initialSSEBufferBytes = 1024

func newSSEReader(reader io.Reader) *sseReader {
	scanner := bufio.NewScanner(reader)
	// Most provider events are small deltas. Let Scanner grow for exceptional
	// events instead of reserving a large buffer for every model response.
	scanner.Buffer(make([]byte, initialSSEBufferBytes), productlimits.MaxSSETokenBytes+1)
	scanner.Split(scanBoundedSSELines)
	return &sseReader{scanner: scanner}
}

func scanBoundedSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if bytes.IndexByte(data, '\n') < 0 && len(data) >= productlimits.MaxSSETokenBytes {
		return 0, nil, errSSETokenTooLarge
	}
	return bufio.ScanLines(data, atEOF)
}

func (reader *sseReader) next() ([]byte, error) {
	if reader.done {
		return nil, io.EOF
	}
	var raw bytes.Buffer
	for reader.scanner.Scan() {
		line := bytes.TrimSuffix(reader.scanner.Bytes(), []byte("\r"))
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			if raw.Len()+len(line) > productlimits.MaxModelCompletionBytes {
				return nil, errSSETokenTooLarge
			}
			raw.Write(line)
			continue
		}
		if !bytes.Equal(field, []byte("data")) {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if bytes.Equal(value, []byte("[DONE]")) {
			reader.done = true
			return nil, io.EOF
		}
		if len(value) == 0 {
			continue
		}
		if bytes.HasPrefix(value, []byte(`{"error":`)) {
			var envelope struct {
				Error *apiError `json:"error"`
			}
			if json.Unmarshal(value, &envelope) == nil && envelope.Error != nil {
				return nil, fmt.Errorf("API error: %s", envelope.Error.message())
			}
		}
		return append([]byte(nil), value...), nil
	}
	if err := reader.scanner.Err(); err != nil {
		return nil, err
	}
	if raw.Len() != 0 {
		return nil, errors.New(raw.String())
	}
	return nil, io.EOF
}
