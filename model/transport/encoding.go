package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// RequestEncoding names the Content-Encoding used to compress request bodies.
// Enable it only for endpoints known to decode that coding: most others reject
// a compressed body as malformed JSON. Zstd connections share an encoder;
// [SetZstdConcurrency] controls its process-wide parallelism.
type RequestEncoding string

const (
	RequestEncodingGzip RequestEncoding = "gzip"
	RequestEncodingZstd RequestEncoding = "zstd"
)

// SetZstdConcurrency sets the process-wide limit for simultaneous zstd request
// compressions across all Connections. The default is min(GOMAXPROCS, 4).
// Each compression is synchronous; the limit does not restrict active HTTP
// requests or response streams. Waiting for a slot respects the request's
// context; an active compression runs to completion. Call it at startup, before
// the first zstd request is compressed. It returns an error if n is not positive
// or zstd compression has already started.
func SetZstdConcurrency(n int) error {
	if n <= 0 {
		return errors.New("zstd concurrency must be positive")
	}
	zstdSettings.Lock()
	defer zstdSettings.Unlock()
	if zstdSettings.started {
		return errors.New("set zstd concurrency before the first zstd request is compressed")
	}
	zstdSettings.concurrency = n
	return nil
}

var zstdSettings struct {
	sync.Mutex
	concurrency int
	started     bool
}

var zstdEncoder = sync.OnceValue(func() *zstdRequestEncoder {
	zstdSettings.Lock()
	defer zstdSettings.Unlock()
	concurrency := zstdSettings.concurrency
	if concurrency == 0 {
		concurrency = min(runtime.GOMAXPROCS(0), 4)
	}
	// Bound both the number and size of retained compression buffers. The
	// 1 MiB window also stays below HTTP zstd's 8 MiB limit (RFC 9659).
	// These options are valid, so NewWriter cannot fail. EncodeAll supports
	// concurrent calls and returns its slot before Post sends the HTTP request.
	encoder, _ := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(concurrency),
		zstd.WithWindowSize(1<<20),
		zstd.WithLowerEncoderMem(true),
	)
	zstdSettings.started = true
	return &zstdRequestEncoder{encoder: encoder, slots: make(chan struct{}, concurrency)}
})

type zstdRequestEncoder struct {
	encoder *zstd.Encoder
	slots   chan struct{}
}

var gzipWriters = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return writer
}}

func (encoder *zstdRequestEncoder) encode(ctx context.Context, body []byte) ([]byte, error) {
	// Match the encoder's capacity so EncodeAll never queues internally. Its
	// own wait cannot be cancelled; this gate keeps that wait in our context.
	select {
	case encoder.slots <- struct{}{}:
		defer func() { <-encoder.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Cancellation can race with a slot becoming available.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body = encoder.encoder.EncodeAll(body, nil)
	return body, ctx.Err()
}

// Both encoders write to memory; only context cancellation can fail.
func encodeBody(ctx context.Context, encoding RequestEncoding, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch encoding {
	case RequestEncodingGzip:
		var encoded bytes.Buffer
		writer := gzipWriters.Get().(*gzip.Writer)
		writer.Reset(&encoded)
		_, _ = writer.Write(body)
		_ = writer.Close()
		// Detach the request buffer before retaining the compressor for reuse.
		writer.Reset(io.Discard)
		gzipWriters.Put(writer)
		return encoded.Bytes(), ctx.Err()
	case RequestEncodingZstd:
		return zstdEncoder().encode(ctx, body)
	}
	return body, nil
}
