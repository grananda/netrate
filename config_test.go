package main

import (
	"testing"
	"time"
)

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults", func(*Config) {}, false},
		{"a single stream is fine", func(c *Config) { c.Streams = 1 }, false},
		{"no warm-up is fine", func(c *Config) { c.WarmUp = 0 }, false},

		{"zero streams", func(c *Config) { c.Streams = 0 }, true},
		{"negative streams", func(c *Config) { c.Streams = -3 }, true},
		{"zero duration", func(c *Config) { c.Duration = 0 }, true},
		{"negative duration", func(c *Config) { c.Duration = -time.Second }, true},
		{"zero interval would panic the ticker", func(c *Config) { c.Interval = 0 }, true},
		{"interval longer than the run", func(c *Config) { c.Interval = 2 * c.Duration }, true},
		{"negative warm-up", func(c *Config) { c.WarmUp = -time.Second }, true},
		{"warm-up swallows the whole run", func(c *Config) { c.WarmUp = c.Duration }, true},

		{"empty url", func(c *Config) { c.URL = "" }, true},
		{"missing scheme", func(c *Config) { c.URL = "proof.ovh.net/files/10Gb.dat" }, true},
		{"unsupported scheme", func(c *Config) { c.URL = "ftp://example.com/f.dat" }, true},
		{"no host", func(c *Config) { c.URL = "https:///f.dat" }, true},
		{"plain http is allowed", func(c *Config) { c.URL = "http://example.com/f.dat" }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("%+v validated, want an error", cfg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%+v rejected: %v", cfg, err)
			}
		})
	}
}

// Regression: MinRun used to be a fixed six seconds, which made -duration very
// nearly inert — a stable link ended the run at six seconds whether you asked
// for fourteen seconds or for five minutes.
func TestDefaultMinRunTracksTheDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		want     time.Duration
	}{
		{14 * time.Second, 6300 * time.Millisecond},
		{30 * time.Second, 13500 * time.Millisecond},
		{5 * time.Minute, 135 * time.Second},
	}
	for _, tc := range tests {
		if got := defaultMinRun(tc.duration); got != tc.want {
			t.Errorf("defaultMinRun(%s) = %s, want %s", tc.duration, got, tc.want)
		}
	}

	// A longer budget must always mean a longer floor, or the flag is inert.
	prev := time.Duration(0)
	for d := time.Second; d <= 10*time.Minute; d += 7 * time.Second {
		got := defaultMinRun(d)
		if got <= prev {
			t.Fatalf("defaultMinRun(%s) = %s did not grow past %s", d, got, prev)
		}
		if got >= d {
			t.Fatalf("defaultMinRun(%s) = %s leaves no room to finish", d, got)
		}
		prev = got
	}
}

func TestNegativeMinRunIsRejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinRun = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("a negative minrun validated, want an error")
	}
}
