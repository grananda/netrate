package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testConfig points a short, fast measurement at a local server. MinRun is far
// beyond Duration so the run always ends on the duration ceiling, never on an
// early settle, which keeps the tests deterministic.
func testConfig(url string) Config {
	return Config{
		URL:      url,
		Streams:  2,
		Duration: 900 * time.Millisecond,
		WarmUp:   150 * time.Millisecond,
		Interval: 30 * time.Millisecond,
		MinRun:   time.Hour,
	}
}

// runEngine drains a whole measurement and hands back the events in order.
func runEngine(t *testing.T, ctx context.Context, cfg Config) []Event {
	t.Helper()
	ch := make(chan Event, 256)
	go Run(ctx, cfg, ch)

	var events []Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func lastEvent(events []Event) Event {
	if len(events) == 0 {
		return nil
	}
	return events[len(events)-1]
}

// endlessBytes keeps writing until the client goes away, so the engine always
// has something to measure.
func endlessBytes(w http.ResponseWriter, r *http.Request) {
	chunk := make([]byte, 32*1024)
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
}

func TestRunProducesAResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(endlessBytes))
	defer srv.Close()

	events := runEngine(t, context.Background(), testConfig(srv.URL))

	done, ok := lastEvent(events).(DoneEvent)
	if !ok {
		t.Fatalf("last event is %T, want DoneEvent (events: %v)", lastEvent(events), events)
	}
	if done.Result.Median <= 0 {
		t.Fatalf("median = %v, want a positive speed", done.Result.Median)
	}
	if done.Result.Samples == 0 {
		t.Fatal("the result counted no warm samples")
	}
	if done.Result.Bytes == 0 {
		t.Fatal("the result counted no bytes")
	}
	if done.Result.Streams != 2 {
		t.Fatalf("streams = %d, want 2", done.Result.Streams)
	}
	// testConfig puts MinRun an hour out, so nothing may end this run early.
	if done.Result.EndedEarly {
		t.Fatal("the run reported an early exit it could not have taken")
	}
}

// steadyCounter feeds a byte counter as a function of elapsed time, so the
// sampler sees a perfectly constant rate. Driving the early exit off a real
// download instead would make the test depend on how quiet the machine is.
func steadyCounter(t *testing.T, counter *atomic.Int64, bytesPerSecond float64) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		start := time.Now()
		for {
			select {
			case <-done:
				return
			default:
			}
			counter.Store(int64(time.Since(start).Seconds() * bytesPerSecond))
			time.Sleep(100 * time.Microsecond)
		}
	}()
}

// A reading that has settled ends the run before the duration budget is spent.
func TestCollectSamplesEndsEarlyOnASettledReading(t *testing.T) {
	cfg := Config{
		URL: "https://example.com/f.dat", Streams: 1,
		Duration: 20 * time.Second, // far more than it should need
		WarmUp:   100 * time.Millisecond,
		Interval: 50 * time.Millisecond,
		MinRun:   2200 * time.Millisecond,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the test config does not validate: %v", err)
	}

	var counter atomic.Int64
	steadyCounter(t, &counter, 80e6) // 640 Mbps

	start := time.Now()
	samples, endedEarly := collectSamples(context.Background(), cfg, &counter, func(Event) {})
	elapsed := time.Since(start)

	if !endedEarly {
		t.Fatalf("a dead-steady reading ran the full %s without settling", cfg.Duration)
	}
	if elapsed > cfg.MinRun+2*time.Second {
		t.Fatalf("settled after %s, want it soon after the %s floor", elapsed, cfg.MinRun)
	}
	if elapsed < cfg.MinRun {
		t.Fatalf("ended after %s, before the %s floor", elapsed, cfg.MinRun)
	}
	if !stableOverall(samples) {
		t.Fatal("the run ended early on a reading it does not consider stable")
	}
}

// Setting the floor to the budget is the documented way to always measure for
// the whole duration.
func TestCollectSamplesRunsTheWholeBudget(t *testing.T) {
	cfg := Config{
		URL: "https://example.com/f.dat", Streams: 1,
		Duration: time.Second,
		WarmUp:   100 * time.Millisecond,
		Interval: 20 * time.Millisecond,
		MinRun:   time.Second, // never reached before the deadline
	}

	var counter atomic.Int64
	steadyCounter(t, &counter, 80e6)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	start := time.Now()
	_, endedEarly := collectSamples(ctx, cfg, &counter, func(Event) {})
	elapsed := time.Since(start)

	if endedEarly {
		t.Fatal("the run stopped early with the floor set to the whole budget")
	}
	if elapsed < cfg.Duration-100*time.Millisecond {
		t.Fatalf("ran %s of the %s budget", elapsed, cfg.Duration)
	}
}

func TestRunEmitsPhasesAndSamples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(endlessBytes))
	defer srv.Close()

	events := runEngine(t, context.Background(), testConfig(srv.URL))

	var phases []string
	samples := 0
	for _, ev := range events {
		switch ev := ev.(type) {
		case PhaseEvent:
			phases = append(phases, ev.Phase)
		case SampleEvent:
			samples++
		}
	}

	if len(phases) < 2 || phases[0] != phaseConnecting || phases[1] != phaseMeasuring {
		t.Fatalf("phases = %v, want %q then %q", phases, phaseConnecting, phaseMeasuring)
	}
	if samples < 5 {
		t.Fatalf("only %d samples reached the consumer", samples)
	}
}

// A 404 is not going to fix itself. Retrying it would burn the measurement
// window and delay the error the user needs to see.
func TestRunDoesNotRetryARejectedStatus(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	events := runEngine(t, context.Background(), cfg)

	failed, ok := lastEvent(events).(ErrEvent)
	if !ok {
		t.Fatalf("last event is %T, want ErrEvent", lastEvent(events))
	}
	if !strings.Contains(failed.Err.Error(), "404") {
		t.Fatalf("error = %v, want it to mention the 404", failed.Err)
	}
	if got := requests.Load(); got != int32(cfg.Streams) {
		t.Fatalf("%d requests for %d streams: a permanent status was retried", got, cfg.Streams)
	}
}

// One dropped connection out of several is normal against a public server and
// must not end the whole measurement.
func TestRunRetriesATransientFailure(t *testing.T) {
	var aborted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if aborted.CompareAndSwap(false, true) {
			panic(http.ErrAbortHandler) // kills the connection without logging
		}
		endlessBytes(w, r)
	}))
	defer srv.Close()

	events := runEngine(t, context.Background(), testConfig(srv.URL))

	if _, ok := lastEvent(events).(DoneEvent); !ok {
		t.Fatalf("last event is %T, want the run to survive one dropped connection", lastEvent(events))
	}
	if !aborted.Load() {
		t.Fatal("the test server never got to drop a connection")
	}
}

func TestRunReportsAnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(endlessBytes))
	url := srv.URL
	srv.Close() // nothing is listening any more

	events := runEngine(t, context.Background(), testConfig(url))

	if _, ok := lastEvent(events).(ErrEvent); !ok {
		t.Fatalf("last event is %T, want ErrEvent", lastEvent(events))
	}
}

// A cancelled run says nothing: the closed channel is the signal, and the
// caller already knows why it cancelled.
func TestRunStopsQuietlyWhenCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(endlessBytes))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	defer cancel()

	cfg := testConfig(srv.URL)
	cfg.Duration = 30 * time.Second // would run far past the cancellation

	start := time.Now()
	events := runEngine(t, ctx, cfg)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("run took %s: cancellation did not reach the streams", elapsed)
	}
	switch ev := lastEvent(events).(type) {
	case DoneEvent:
		t.Fatal("a cancelled run must not report a verdict")
	case ErrEvent:
		t.Fatalf("a cancelled run must not report an error, got %v", ev.Err)
	}
}

// The engine is a public entry point, so it validates rather than panicking in
// time.NewTicker or expiring the download context before the first byte.
func TestRunRejectsAnInvalidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Interval = 0

	events := runEngine(t, context.Background(), cfg)

	if len(events) != 1 {
		t.Fatalf("got %d events, want just the error", len(events))
	}
	if _, ok := events[0].(ErrEvent); !ok {
		t.Fatalf("got %T, want ErrEvent", events[0])
	}
}

func TestStatusErrorIsRecognisable(t *testing.T) {
	var target statusError
	if !errors.As(error(statusError{status: "404 Not Found"}), &target) {
		t.Fatal("errors.As failed to unwrap a statusError")
	}
}

func TestCacheBustURL(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		wantPrefix string
	}{
		{"no query yet", "https://example.com/f.dat", "https://example.com/f.dat?nocache=1-2-"},
		{"existing query", "https://example.com/f.dat?a=b", "https://example.com/f.dat?a=b&nocache=1-2-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheBustURL(tc.url, 1, 2)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("got %q, want prefix %q", got, tc.wantPrefix)
			}
			if got == tc.wantPrefix {
				t.Fatal("the timestamp suffix is missing, so two attempts would collide")
			}
		})
	}
}

// Regression: a 200 with an empty body used to count as "ran to EOF, reopen the
// file", so the stream loop spun with no delay at all — thousands of requests a
// second at whatever URL the user happened to point it at.
func TestRunDoesNotSpinOnAnEmptyBody(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK) // 200, and not one byte of body
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	events := runEngine(t, context.Background(), cfg)

	if _, ok := lastEvent(events).(ErrEvent); !ok {
		t.Fatalf("last event is %T, want ErrEvent: a server sending nothing is a failure",
			lastEvent(events))
	}

	// Each stream may retry maxStreamRetries times before retiring, and every
	// retry waits retryBackoff. Anything beyond that is the hot loop.
	budget := int32(cfg.Streams * (maxStreamRetries + 1))
	if got := requests.Load(); got > budget {
		t.Fatalf("%d requests in %s, want at most %d: the stream loop is spinning",
			got, cfg.Duration, budget)
	}
	t.Logf("%d requests over %s (ceiling %d)", requests.Load(), cfg.Duration, budget)
}

// A file that legitimately runs out mid-run is reopened, not treated as an
// error: that is how a short file still fills a long measurement.
func TestRunReopensAFinishedFile(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(make([]byte, 512*1024)) // a real but short body
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	events := runEngine(t, context.Background(), cfg)

	if _, ok := lastEvent(events).(DoneEvent); !ok {
		t.Fatalf("last event is %T, want DoneEvent", lastEvent(events))
	}
	if got := requests.Load(); got <= int32(cfg.Streams) {
		t.Fatalf("%d requests: the streams never reopened the file", got)
	}
}
