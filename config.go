package main

import (
	"fmt"
	"net/url"
	"time"
)

// Config describes how a download measurement is carried out.
//
// The important idea: we measure for a fixed *duration*, not for a fixed
// number of bytes. A fixed-size file finishes in a second on fibre (too
// little time to leave TCP slow start) and takes minutes on a slow link.
type Config struct {
	URL      string        // large, incompressible file
	Streams  int           // concurrent TCP connections
	Duration time.Duration // hard ceiling for the download phase
	WarmUp   time.Duration // leading window discarded from the statistics
	Interval time.Duration // sampling period for instantaneous speed
	MinRun   time.Duration // minimum time before a settled reading may end the run
}

// minRunFraction is how much of the duration budget a run has to spend before a
// settled reading is allowed to stop it.
//
// It is a fraction rather than a constant on purpose. When it was fixed at six
// seconds, -duration was very nearly inert: a stable link ended the run at six
// seconds whether you asked for fourteen or for five minutes.
const (
	minRunFraction = 0.45

	// defaultDuration is named so DefaultConfig cannot set the budget in one
	// place and derive the floor from a different number in the next.
	defaultDuration = 14 * time.Second
)

// defaultMinRun tracks the duration budget unless the caller pins it.
func defaultMinRun(duration time.Duration) time.Duration {
	return time.Duration(float64(duration) * minRunFraction)
}

func DefaultConfig() Config {
	return Config{
		URL:      "https://speedtest.milkywan.fr/files/1G.iso",
		Streams:  6,
		Duration: defaultDuration,
		WarmUp:   2500 * time.Millisecond,
		Interval: 250 * time.Millisecond,
		MinRun:   defaultMinRun(defaultDuration),
	}
}

// Validate reports why this configuration could not produce a measurement.
//
// Every field is reachable from a command-line flag, and several of them are
// load-bearing in ways that fail obscurely: a zero Interval panics the ticker,
// a zero Duration expires the download context before the first byte, and a
// WarmUp longer than the run discards every sample there was.
func (c Config) Validate() error {
	switch {
	case c.Streams < 1:
		return fmt.Errorf("streams must be at least 1, got %d", c.Streams)
	case c.Duration <= 0:
		return fmt.Errorf("duration must be positive, got %s", c.Duration)
	case c.Interval <= 0:
		return fmt.Errorf("interval must be positive, got %s", c.Interval)
	case c.Interval > c.Duration:
		return fmt.Errorf("interval (%s) must not exceed duration (%s)", c.Interval, c.Duration)
	case c.WarmUp < 0:
		return fmt.Errorf("warmup must not be negative, got %s", c.WarmUp)
	case c.WarmUp >= c.Duration:
		return fmt.Errorf("warmup (%s) must be shorter than duration (%s)", c.WarmUp, c.Duration)
	case c.MinRun < 0:
		return fmt.Errorf("minrun must not be negative, got %s", c.MinRun)
	}
	return validateURL(c.URL)
}

func validateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q has no host", raw)
	}
	return nil
}
