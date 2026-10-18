package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/levmv/skot/model"
)

func TestEncodedBodiesRemainIndependent(t *testing.T) {
	for _, encoding := range []RequestEncoding{RequestEncodingGzip, RequestEncodingZstd} {
		t.Run(string(encoding), func(t *testing.T) {
			inputs := []string{"", `{}`, strings.Repeat("first body ", 1024), strings.Repeat("other body ", 32)}
			encoded := make([][]byte, 2*len(inputs))
			encode := func(i int) {
				var err error
				encoded[i], err = encodeBody(t.Context(), encoding, []byte(inputs[i%len(inputs)]))
				if err != nil {
					t.Error(err)
				}
			}
			for i := range inputs {
				encode(i)
			}
			var calls sync.WaitGroup
			for i := range inputs {
				calls.Go(func() { encode(len(inputs) + i) })
			}
			calls.Wait()
			// Keep earlier results alive through later compressions, including
			// concurrent ones, as HTTP requests can still be reading those bytes.
			for i, body := range encoded {
				decoded, err := decodeRequestBody(string(encoding), bytes.NewReader(body))
				if err != nil || string(decoded) != inputs[i%len(inputs)] {
					t.Errorf("body %d: decoded = %q, error = %v", i, decoded, err)
				}
			}
		})
	}
}

func TestPostZstdConcurrencyAndCancellation(t *testing.T) {
	const helperEnv = "SKOT_TEST_ZSTD_PROCESS"
	if os.Getenv(helperEnv) != "1" {
		// A fresh process isolates startup settings and lets synctest own the
		// compression slot channel, regardless of which tests ran before it.
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPostZstdConcurrencyAndCancellation$")
		command.Env = append(os.Environ(), helperEnv+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("zstd concurrency: %v\n%s", err, output)
		}
		return
	}
	synctest.Test(t, func(t *testing.T) {
		if err := SetZstdConcurrency(2); err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{}, 3)
		releaseHTTP := make(chan struct{})
		var requests sync.WaitGroup
		defer func() { close(releaseHTTP); requests.Wait() }()
		connection := testConnection(t, Config{
			RequestEncoding: RequestEncodingZstd,
			HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				entered <- struct{}{}
				<-releaseHTTP
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})},
		})
		func() {
			// Hold both slots to reproduce a full queue without relying on CPU
			// speed or a large body taking long enough to compress.
			slots := zstdEncoder().slots
			for range 2 {
				slots <- struct{}{}
				defer func() { <-slots }()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				response, err := connection.Post(ctx, []byte(`{}`), nil)
				if response != nil {
					response.Body.Close()
				}
				result <- err
			}()
			synctest.Wait()
			if len(entered) != 0 {
				t.Fatal("request bypassed the occupied compression slots")
			}
			cancel()
			synctest.Wait()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) || !errors.Is(err, model.ErrRequestNotSent) {
					t.Fatalf("queued request error = %v, want Canceled and ErrRequestNotSent", err)
				}
			default:
				t.Fatal("queued compression ignored cancellation")
			}
		}()
		// More requests than slots must reach HTTP while earlier requests are
		// still active: neither cancelled waiters nor HTTP may retain a slot.
		for range 3 {
			requests.Go(func() {
				response, err := connection.Post(t.Context(), []byte(`{}`), nil)
				if err != nil {
					t.Error(err)
					return
				}
				response.Body.Close()
			})
		}
		synctest.Wait()
		if got := len(entered); got != 3 {
			t.Fatalf("only %d of 3 requests reached HTTP; compression slots were retained", got)
		}
	})
}
