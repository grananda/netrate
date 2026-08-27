package main

import (
	"math"
	"testing"
	"time"
)

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{7}, 0.5, 7},
		{"odd length hits a real reading", []float64{1, 2, 3}, 0.5, 2},
		{"even length interpolates", []float64{1, 2, 3, 4}, 0.5, 2.5},
		{"first", []float64{1, 2, 3, 4}, 0, 1},
		{"last", []float64{1, 2, 3, 4}, 1, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			approx(t, percentile(tc.sorted, tc.p), tc.want)
		})
	}
}

func TestTrim(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		want   []float64
	}{
		{"empty", nil, []float64{}},
		{"too short to spare anything", []float64{1, 2, 3}, []float64{1, 2, 3}},
		{"drops one from each end", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, []float64{2, 3, 4, 5, 6, 7, 8, 9}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := trim(tc.sorted, trimFraction)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				approx(t, got[i], tc.want[i])
			}
		})
	}
}

// The whole point of trimming is that one wild reading must not move the
// answer. If this ever fails, the statistics have stopped being robust.
func TestTrimmedMeanIgnoresAnOutlier(t *testing.T) {
	clean := []float64{100, 100, 100, 100, 100, 100, 100, 100, 100, 100}
	spiked := []float64{0, 100, 100, 100, 100, 100, 100, 100, 100, 9000}

	approx(t, trimmedMean(clean, trimFraction), 100)
	approx(t, trimmedMean(spiked, trimFraction), 100)
}

func TestMeanCV(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		mean, cv := meanCV(nil)
		approx(t, mean, 0)
		approx(t, cv, 0)
	})
	t.Run("a flat series has no dispersion", func(t *testing.T) {
		mean, cv := meanCV([]float64{50, 50, 50})
		approx(t, mean, 50)
		approx(t, cv, 0)
	})
	t.Run("all zeroes does not divide by zero", func(t *testing.T) {
		mean, cv := meanCV([]float64{0, 0, 0})
		approx(t, mean, 0)
		approx(t, cv, 0)
	})
	t.Run("known spread", func(t *testing.T) {
		// mean 100, population sd 10 -> CV 10%.
		_, cv := meanCV([]float64{90, 110})
		approx(t, cv, 10)
	})
}

func TestTrimmedCVDoesNotMutateItsInput(t *testing.T) {
	speeds := []float64{9, 1, 5, 3, 7}
	trimmedCV(speeds)
	for i, want := range []float64{9, 1, 5, 3, 7} {
		approx(t, speeds[i], want)
	}
}

// series builds a sample run: `cold` readings inside the warm-up followed by
// the given warm ones.
func series(cold int, warm ...float64) []Sample {
	cfg := DefaultConfig()
	samples := make([]Sample, 0, cold+len(warm))
	at := time.Duration(0)
	for range cold {
		at += cfg.Interval
		samples = append(samples, Sample{At: at, Mbps: 1, Warm: false})
	}
	for _, v := range warm {
		at += cfg.Interval
		samples = append(samples, Sample{At: at, Mbps: v, Warm: true, Bytes: 1})
	}
	return samples
}

func TestSummarise(t *testing.T) {
	cfg := DefaultConfig()

	t.Run("no samples returns a zero result instead of panicking", func(t *testing.T) {
		got := summarise(nil, 0, cfg)
		if got.Samples != 0 || got.Median != 0 {
			t.Fatalf("got %+v, want a zero result", got)
		}
		if got.Streams != cfg.Streams {
			t.Fatalf("streams = %d, want %d", got.Streams, cfg.Streams)
		}
	})

	t.Run("the warm-up is excluded from the statistics", func(t *testing.T) {
		// The cold readings sit at 1 Mbps; if they leaked in, the median
		// would collapse well below 100.
		got := summarise(series(20, 100, 100, 100, 100, 100), 1234, cfg)
		approx(t, got.Median, 100)
		approx(t, got.Min, 100)
		if got.Samples != 5 {
			t.Fatalf("samples = %d, want 5 warm readings", got.Samples)
		}
		if got.Bytes != 1234 {
			t.Fatalf("bytes = %d, want 1234", got.Bytes)
		}
	})

	t.Run("a run that never warmed up falls back to what it has", func(t *testing.T) {
		got := summarise(series(4), 10, cfg)
		if got.Samples != 4 {
			t.Fatalf("samples = %d, want the 4 cold readings", got.Samples)
		}
		approx(t, got.Median, 1)
	})

	t.Run("a flat series is stable", func(t *testing.T) {
		got := summarise(series(2, 100, 100, 100, 100, 100, 100), 0, cfg)
		if !got.Stable {
			t.Fatalf("CV %.2f%% should be under the %.1f%% threshold", got.CV, stabilityCV)
		}
	})

	t.Run("a swinging series is unstable", func(t *testing.T) {
		got := summarise(series(2, 10, 200, 15, 190, 20, 180, 25, 170), 0, cfg)
		if got.Stable {
			t.Fatalf("CV %.2f%% should be over the %.1f%% threshold", got.CV, stabilityCV)
		}
	})
}

func TestSettled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Interval = 100 * time.Millisecond // a 2s window is 20 samples at this rate

	flat := make([]float64, 25)
	for i := range flat {
		flat[i] = 100
	}

	t.Run("not enough history yet", func(t *testing.T) {
		if settled(series(0, flat[:10]...), cfg) {
			t.Fatal("10 readings should not be enough to call it")
		}
	})

	t.Run("a steady warm window settles", func(t *testing.T) {
		if !settled(series(0, flat...), cfg) {
			t.Fatal("25 flat warm readings should settle")
		}
	})

	t.Run("the warm-up never settles", func(t *testing.T) {
		cold := make([]Sample, 25)
		for i := range cold {
			cold[i] = Sample{Mbps: 100, Warm: false}
		}
		if settled(cold, cfg) {
			t.Fatal("readings inside the warm-up must not settle the run")
		}
	})

	t.Run("a noisy window does not settle", func(t *testing.T) {
		noisy := make([]float64, 25)
		for i := range noisy {
			noisy[i] = 100
			if i%2 == 0 {
				noisy[i] = 20
			}
		}
		if settled(series(0, noisy...), cfg) {
			t.Fatal("a series swinging 20..100 must not settle")
		}
	})

	t.Run("a zero interval cannot divide by zero", func(t *testing.T) {
		broken := cfg
		broken.Interval = 0
		if settled(series(0, flat...), broken) {
			t.Fatal("a zero interval should refuse to settle, not panic")
		}
	})
}

// The early exit and the final verdict must never contradict each other: a run
// is not allowed to stop saying "settled" and then report "unstable".
func TestStableOverall(t *testing.T) {
	cfg := DefaultConfig()

	t.Run("no samples", func(t *testing.T) {
		if stableOverall(nil) {
			t.Fatal("an empty run must not count as stable")
		}
	})

	t.Run("a flat run is stable", func(t *testing.T) {
		samples := series(2, 100, 100, 100, 100, 100, 100, 100, 100)
		if !stableOverall(samples) {
			t.Fatal("a flat series should be stable")
		}
		if got := summarise(samples, 0, cfg); got.Stable != stableOverall(samples) {
			t.Fatal("stableOverall disagrees with the verdict summarise reports")
		}
	})

	t.Run("a run that swung early is not", func(t *testing.T) {
		// A quiet tail after a noisy start: settled() sees only the tail, so
		// this is exactly the case the two tests together have to catch.
		samples := series(2, 100, 900, 150, 850, 600, 600, 600, 600, 600, 600)
		if stableOverall(samples) {
			t.Fatal("a run ranging 100..900 must not count as stable")
		}
		if got := summarise(samples, 0, cfg); got.Stable {
			t.Fatal("summarise and stableOverall disagree")
		}
	})
}
