package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// readChunkSize is how much we pull off the socket at a time. Big enough
	// that syscall overhead is noise, small enough that the sampler still sees
	// progress several times per interval.
	readChunkSize = 64 * 1024

	// maxStreamRetries is how many consecutive transient failures one stream
	// tolerates before retiring. Against a public server with several parallel
	// connections the occasional reset is normal, and ending the whole
	// measurement over one of them wastes a perfectly good run.
	maxStreamRetries = 3

	// retryBackoff keeps a stream that is failing instantly from spinning.
	retryBackoff = 250 * time.Millisecond
)

var (
	errNoUsableSamples = errors.New("the measurement produced no usable samples")
	errEmptyBody       = errors.New("the server returned an empty body")
)

// Run performs the measurement and streams events to ch, which it closes on
// exit. It is meant to be called from a goroutine.
//
// Cancelling ctx stops everything and ends the run silently: the closed channel
// is the signal, and the caller already knows why it cancelled.
func Run(ctx context.Context, cfg Config, ch chan<- Event) {
	defer close(ch)

	// emit never blocks forever: a UI that stops reading is a UI that is going
	// away, and ctx will be cancelled right behind it.
	emit := func(ev Event) {
		select {
		case ch <- ev:
		case <-ctx.Done():
		}
	}

	if err := cfg.Validate(); err != nil {
		emit(ErrEvent{Err: err})
		return
	}

	client := newHTTPClient(cfg.Streams)
	defer client.CloseIdleConnections()

	emit(PhaseEvent{Phase: phaseConnecting})

	// The download phase gets its own cancellable context so that a settled
	// reading, the duration ceiling, the last stream dying and a quit key all
	// tear the connections down the same way.
	dlCtx, stopDownload := context.WithTimeout(ctx, cfg.Duration)
	defer stopDownload()

	var (
		bytesRead atomic.Int64
		streamErr firstError
		running   atomic.Int32
		streams   sync.WaitGroup
	)
	running.Store(int32(cfg.Streams))

	for id := range cfg.Streams {
		streams.Add(1)
		go func() {
			defer streams.Done()
			if err := downloadStream(dlCtx, client, cfg.URL, id, &bytesRead); err != nil {
				streamErr.set(err)
			}
			// With every stream retired there is nothing left to measure, so
			// stop the sampler rather than let it record a tail of zeros.
			if running.Add(-1) == 0 {
				stopDownload()
			}
		}()
	}

	samples, endedEarly := collectSamples(dlCtx, cfg, &bytesRead, emit)

	stopDownload()
	streams.Wait()

	if ctx.Err() != nil {
		return // interrupted by the caller
	}

	// A stream error only decides the outcome when it left us with nothing to
	// report. Losing a connection in the last second of an otherwise healthy
	// run should not throw away ten seconds of readings.
	if !hasUsableSample(samples) {
		err := streamErr.get()
		if err == nil {
			err = errNoUsableSamples
		}
		emit(ErrEvent{Err: err})
		return
	}

	result := summarise(samples, bytesRead.Load(), cfg)
	result.EndedEarly = endedEarly
	emit(DoneEvent{Result: result})
}

// hasUsableSample reports whether anything worth summarising was measured: a
// reading past the warm-up that actually carried bytes.
//
// The "carried bytes" half matters more than it looks. A run against an
// unreachable server still ticks along producing warm samples — every one of
// them 0 Mbps — and summarising those would report a confident 0 Mbps verdict
// instead of the connection error behind it.
func hasUsableSample(samples []Sample) bool {
	for _, s := range samples {
		if s.Warm && s.Mbps > 0 {
			return true
		}
	}
	return false
}

// collectSamples ticks every cfg.Interval, converting the shared byte counter
// into instantaneous speeds. It returns once the context ends or the reading
// has settled, and reports which of the two happened: a run that stopped early
// spent only part of its budget, and the caller has to be able to say so.
func collectSamples(ctx context.Context, cfg Config, counter *atomic.Int64, emit func(Event)) (samples []Sample, endedEarly bool) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	var (
		start     = time.Now()
		prevAt    = start
		prevBytes int64
		flowing   bool
	)

	for {
		select {
		case <-ctx.Done():
			return samples, false

		case <-ticker.C:
			// Timestamp the snapshot when we actually take it, not when the
			// tick was scheduled: otherwise a late goroutine attributes its
			// bytes to the wrong window and the series alternates between a
			// dip and a spike.
			cur := counter.Load()
			now := time.Now()
			seconds := now.Sub(prevAt).Seconds()
			if seconds <= 0 {
				continue
			}

			if !flowing && cur > 0 {
				flowing = true
				emit(PhaseEvent{Phase: phaseMeasuring})
			}

			elapsed := now.Sub(start)
			s := Sample{
				At:    elapsed,
				Mbps:  float64(cur-prevBytes) * 8 / seconds / 1e6,
				Bytes: cur,
				Warm:  elapsed >= cfg.WarmUp,
			}
			prevAt, prevBytes = now, cur

			samples = append(samples, s)
			emit(SampleEvent{Sample: s})

			// Both halves matter. On the recent window alone a run can stop
			// on a quiet tail while the reading as a whole is still swinging,
			// and then report "unstable" for a run it chose to cut short.
			if elapsed >= cfg.MinRun && settled(samples, cfg) && stableOverall(samples) {
				return samples, true
			}
		}
	}
}

// downloadStream keeps one connection busy until the context is cancelled,
// reopening the file whenever it runs out and retrying transient failures a
// few times. It returns nil when the context ended, and the last error when
// the stream gave up on its own.
func downloadStream(ctx context.Context, client *http.Client, rawURL string, id int, counter *atomic.Int64) error {
	buf := make([]byte, readChunkSize)
	failures := 0

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}

		read, err := fetchOnce(ctx, client, cacheBustURL(rawURL, id, attempt), buf, counter)
		if ctx.Err() != nil {
			return nil
		}

		var status statusError
		switch {
		case errors.As(err, &status):
			return err // the server will keep saying no; do not burn the window on it
		case err == nil && read > 0:
			failures = 0 // ran to EOF: reopen the file and keep going
			continue
		case err == nil:
			// A 200 with an empty body. Treating it as success would spin this
			// loop with no delay at all, reopening the file as fast as the
			// server could answer and hammering it for the rest of the run.
			err = errEmptyBody
		}

		failures++
		if failures > maxStreamRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryBackoff):
		}
	}
}

// fetchOnce downloads the body once, accounting every chunk as it arrives so
// the sampler sees progress in real time. It reports how much it read, which is
// what lets the caller tell a finished file from a server answering with
// nothing at all.
func fetchOnce(ctx context.Context, client *http.Client, url string, buf []byte, counter *atomic.Int64) (read int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	// Ask for the raw bytes: if the server gzipped the body we would be
	// counting decompressed bytes, not bytes on the wire.
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Pragma", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, statusError{status: resp.Status}
	}

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			counter.Add(int64(n))
			read += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return read, nil
		}
		if err != nil {
			return read, err
		}
	}
}

// statusError marks a response the server will keep rejecting. Retrying a 404
// or a 403 only wastes what is left of the measurement window.
type statusError struct{ status string }

func (e statusError) Error() string { return "unexpected HTTP status: " + e.status }

// firstError keeps the first error reported by any stream and discards the
// cascade that usually follows it.
type firstError struct {
	mu  sync.Mutex
	err error
}

func (f *firstError) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
	}
}

func (f *firstError) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// cacheBustURL makes every request unique so no proxy or CDN can answer from
// cache. The suffix mixes the stream id, the attempt and the clock, so two
// streams starting in the same nanosecond still ask for different things.
func cacheBustURL(rawURL string, id, attempt int) string {
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	return fmt.Sprintf("%s%snocache=%d-%d-%s",
		rawURL, separator, id, attempt, strconv.FormatInt(time.Now().UnixNano(), 36))
}

func newHTTPClient(streams int) *http.Client {
	return &http.Client{
		// No client timeout on purpose: the download context is what bounds
		// the run, and a blanket timeout here would abort a healthy stream.
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			// HTTP/2 would multiplex every stream over a single TCP
			// connection, which defeats the whole point of opening several.
			ForceAttemptHTTP2:   false,
			TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
			DisableCompression:  true,
			MaxIdleConnsPerHost: streams,
			TLSHandshakeTimeout: 10 * time.Second,
			ReadBufferSize:      256 * 1024,
		},
	}
}
