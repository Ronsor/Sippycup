package libsippycup

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestShutdownWorkAdmission(t *testing.T) {
	e := NewEngine(&Config{})
	if !e.beginWork() {
		t.Fatal("running engine rejected work")
	}

	closed := make(chan struct{})
	go func() {
		e.Close()
		close(closed)
	}()
	<-e.Done()

	if e.beginWork() {
		e.wg.Done()
		t.Fatal("shutdown admitted new work")
	}

	select {
	case <-closed:
		t.Fatal("Close returned before admitted work finished")
	default:
	}

	e.wg.Done()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after work drained")
	}

	recorder := httptest.NewRecorder()
	e.serveHTTP("127.0.0.1:0", recorder, httptest.NewRequest(http.MethodPost, "/calls", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("late HTTP request returned %d", recorder.Code)
	}
}

func TestConcurrentShutdownAdmission(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		e := NewEngine(&Config{})
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i := 0; i < 20; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				if e.beginWork() {
					defer e.wg.Done()
					// Children are admitted while the parent still owns a
					// reference, even if shutdown has begun.
					e.wg.Add(1)
					go e.wg.Done()
				}
			}()
		}

		close(start)
		e.Close()
		workers.Wait()
		if e.beginWork() {
			e.wg.Done()
			t.Fatal("closed engine admitted work")
		}
	}
}

func TestHTTPBodyLimitOverflow(t *testing.T) {
	for _, limit := range []int64{0, 1, math.MaxInt64 - 1, math.MaxInt64} {
		err := checkDurations(nil, &LimitConfig{MaxHTTPBodyBytes: limit})
		if (err != nil) != (limit == math.MaxInt64) {
			t.Fatalf("limit %d: unexpected validation result: %v", limit, err)
		}
	}
}

func TestPumpPreservesRepeatedDTMF(t *testing.T) {
	ctx := context.Background()
	format := Format{"pcm_s16le", 8000, 1}
	src := newChannelLeg(ctx, format, 10)
	defer src.Close()
	dst := newChannelLeg(ctx, format, 10)
	defer dst.Close()

	var digits []byte
	dst.writeFn = func(_ context.Context, frame Frame) error {
		digits = append(digits, frame.Digit)
		return nil
	}
	for _, digit := range []byte("1155##") {
		src.emit(Frame{Digit: digit})
	}
	src.fail(io.EOF)

	node := &Node{Type: "audiosocket"}
	if err := pump(ctx, src, dst, node, node, 0); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}

	if string(digits) != "1155##" {
		t.Fatalf("repeated digits were lost: %q", digits)
	}
}
