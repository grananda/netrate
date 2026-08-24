package main

import (
	"math"
	"sort"
	"time"
)

// Sample is one instantaneous speed reading: one column of the chart.
type Sample struct {
	At    time.Duration // elapsed since the download phase started
	Mbps  float64       // speed over the last Interval, aggregated over all streams
	Bytes int64         // cumulative bytes across all streams
	Warm  bool          // true once past WarmUp, i.e. counts towards the result
}

// Result is the aggregate verdict once the run finishes.
type Result struct {
	Median  float64 // p50 of the readings past the warm-up
	Trimmed float64 // mean after dropping the fastest and slowest 10%
	Min     float64
	Max     float64
	CV      float64 // coefficient of variation, in percent
	Stable  bool    // CV below the stability threshold

	// EndedEarly reports that the reading settled before the duration budget
	// ran out, so Duration is well short of the configured ceiling.
	EndedEarly bool

	Bytes    int64
	Duration time.Duration
	Samples  int
	Streams  int
}

const (
	// stabilityCV is the coefficient of variation, in percent, below which a
	// reading is considered settled.
	stabilityCV = 4.0

	// stabilityWindow is how much recent history has to be that tight before
	// we call the measurement early.
	stabilityWindow = 2 * time.Second

	// minStabilitySamples stops a coarse Interval from declaring victory on
	// two or three readings.
	minStabilitySamples = 5

	// trimFraction is dropped from each end of a sorted series before
	// averaging, so a couple of scheduling hiccups cannot condemn an
	// otherwise steady reading.
	trimFraction = 0.1
)

// settled reports whether the most recent readings are tight enough that
// measuring for longer would not change the answer.
func settled(samples []Sample, cfg Config) bool {
	if cfg.Interval <= 0 {
		return false
	}
	window := int(stabilityWindow / cfg.Interval)
	if window < minStabilitySamples || len(samples) < window {
		return false
	}

	speeds := make([]float64, 0, window)
	for _, s := range samples[len(samples)-window:] {
		if !s.Warm {
			return false // still inside the warm-up: nothing to conclude yet
		}
		speeds = append(speeds, s.Mbps)
	}
	return trimmedCV(speeds) < stabilityCV
}

// stableOverall reports whether the run as a whole sits inside the stability
// threshold. It is the same test summarise applies for Result.Stable, so the
// early exit and the verdict can never disagree.
func stableOverall(samples []Sample) bool {
	speeds := warmSpeeds(samples)
	if len(speeds) == 0 {
		return false
	}
	return trimmedCV(speeds) < stabilityCV
}

// summarise turns the sample series into a verdict, ignoring the warm-up and
// trimming the extremes the way commercial testers do.
func summarise(samples []Sample, bytes int64, cfg Config) Result {
	if len(samples) == 0 {
		return Result{Streams: cfg.Streams, Bytes: bytes}
	}

	speeds := warmSpeeds(samples)
	sorted := append([]float64(nil), speeds...)
	sort.Float64s(sorted)

	res := Result{
		Median:   percentile(sorted, 0.5),
		Trimmed:  trimmedMean(sorted, trimFraction),
		Min:      sorted[0],
		Max:      sorted[len(sorted)-1],
		Bytes:    bytes,
		Duration: samples[len(samples)-1].At,
		Samples:  len(speeds),
		Streams:  cfg.Streams,
	}
	res.CV = trimmedCV(speeds)
	res.Stable = res.CV < stabilityCV
	return res
}

// warmSpeeds returns the readings that count towards the result: everything
// past the warm-up, or the whole series if the run never got that far. Callers
// rely on it being non-empty whenever samples is.
func warmSpeeds(samples []Sample) []float64 {
	speeds := make([]float64, 0, len(samples))
	for _, s := range samples {
		if s.Warm {
			speeds = append(speeds, s.Mbps)
		}
	}
	if len(speeds) > 0 {
		return speeds
	}
	// The run never made it past the warm-up; use what we have rather than
	// reporting nothing.
	for _, s := range samples {
		speeds = append(speeds, s.Mbps)
	}
	return speeds
}

// trimmedCV measures dispersion after dropping the extremes. Input need not be
// sorted; the caller's slice is left untouched.
func trimmedCV(speeds []float64) float64 {
	sorted := append([]float64(nil), speeds...)
	sort.Float64s(sorted)
	_, cv := meanCV(trim(sorted, trimFraction))
	return cv
}

// meanCV returns the mean and the coefficient of variation, in percent.
func meanCV(v []float64) (mean, cv float64) {
	if len(v) == 0 {
		return 0, 0
	}
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	if mean == 0 {
		return 0, 0
	}

	var variance float64
	for _, x := range v {
		variance += (x - mean) * (x - mean)
	}
	variance /= float64(len(v))
	return mean, math.Sqrt(variance) / mean * 100
}

// percentile interpolates between the two neighbouring readings, so p50 of an
// even-length series is the average of the middle pair rather than one of them.
// The series must already be sorted.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := p * float64(len(sorted)-1)
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// trimmedMean averages an already sorted series with its extremes removed.
func trimmedMean(sorted []float64, frac float64) float64 {
	kept := trim(sorted, frac)
	if len(kept) == 0 {
		return 0
	}
	var sum float64
	for _, x := range kept {
		sum += x
	}
	return sum / float64(len(kept))
}

// trim drops the fastest and slowest frac of an already sorted series, keeping
// everything when the series is too short to spare anything.
func trim(sorted []float64, frac float64) []float64 {
	cut := int(float64(len(sorted)) * frac)
	if len(sorted)-2*cut < 1 {
		cut = 0
	}
	return sorted[cut : len(sorted)-cut]
}
