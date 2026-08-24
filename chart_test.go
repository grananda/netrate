package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// stripANSI drops the styling so a rendered row can be inspected as text.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestSpeedToFrac(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		want float64
	}{
		{"below the ramp", -5, 0},
		{"bottom anchor", 0, 0},
		{"first anchor", 10, 1.0 / 6},
		{"last anchor", 1000, 1},
		{"past the ramp", 5000, 1},
		{"NaN parks at zero", math.NaN(), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := speedToFrac(tc.v); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("speedToFrac(%v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// The colour ramp is non-linear on purpose, but it must never go backwards.
func TestSpeedToFracIsMonotonic(t *testing.T) {
	prev := -1.0
	for v := 0.0; v <= 1200; v += 0.5 {
		got := speedToFrac(v)
		if got < prev {
			t.Fatalf("speedToFrac(%v) = %v dropped below the previous %v", v, got, prev)
		}
		if got < 0 || got > 1 {
			t.Fatalf("speedToFrac(%v) = %v is outside 0..1", v, got)
		}
		prev = got
	}
}

func TestNiceStep(t *testing.T) {
	tests := []struct {
		raw  float64
		want float64
	}{
		{0, 1}, {-5, 1},
		{0.7, 1}, {1, 1}, {1.5, 2}, {2.4, 2.5}, {2.9, 3}, {3.5, 4}, {4.5, 5}, {7, 10},
		{156.75, 200}, {225, 250}, {300, 300},
	}
	for _, tc := range tests {
		if got := niceStep(tc.raw); got != tc.want {
			t.Errorf("niceStep(%v) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// The y axis has to clear the data and still divide into readable gridlines.
func TestNiceTop(t *testing.T) {
	tests := []struct {
		name string
		peak float64
	}{
		{"slow link", 8.4}, {"typical", 95}, {"fast", 627}, {"gigabit", 940}, {"multi-gig", 2400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			top := niceTop([]float64{tc.peak, math.NaN(), tc.peak / 2})

			if top < tc.peak {
				t.Fatalf("top %v does not clear the peak %v", top, tc.peak)
			}
			if top > tc.peak*2.5 {
				t.Fatalf("top %v leaves too much headroom over %v", top, tc.peak)
			}
			// Every gridline must land on the same round step.
			step := top / gridLines
			if math.Abs(step*gridLines-top) > 1e-9 {
				t.Fatalf("top %v does not divide into %d gridlines", top, gridLines)
			}
		})
	}
}

func TestNiceTopOfAnEmptyChart(t *testing.T) {
	if got := niceTop([]float64{math.NaN(), math.NaN()}); got <= 0 {
		t.Fatalf("niceTop = %v, want a positive scale even with no data", got)
	}
}

func TestBucketise(t *testing.T) {
	cfg := DefaultConfig() // 100ms interval, 14s duration

	t.Run("an empty history leaves every column unmeasured", func(t *testing.T) {
		for i, v := range bucketise(nil, cfg.Interval, cfg.Duration, 10) {
			if !math.IsNaN(v) {
				t.Fatalf("column %d = %v, want NaN", i, v)
			}
		}
	})

	t.Run("samples average into their column", func(t *testing.T) {
		// 14s over 7 columns is 2s per column, i.e. 20 samples each.
		history := make([]float64, 40)
		for i := range history {
			history[i] = 100
			if i >= 20 {
				history[i] = 200
			}
		}
		got := bucketise(history, cfg.Interval, cfg.Duration, 7)
		if math.Abs(got[0]-100) > 1e-9 {
			t.Errorf("column 0 = %v, want the 100 Mbps average", got[0])
		}
		if math.Abs(got[1]-200) > 1e-9 {
			t.Errorf("column 1 = %v, want the 200 Mbps average", got[1])
		}
		if !math.IsNaN(got[6]) {
			t.Errorf("column 6 = %v, want NaN: the run has not got there yet", got[6])
		}
	})

	t.Run("a chart wider than the sample count has no holes", func(t *testing.T) {
		// 90 columns for 70 readings: without interpolation every third
		// column lands between samples and the area reads as a comb.
		history := make([]float64, 70)
		for i := range history {
			history[i] = 600
		}
		short := cfg
		short.Duration = 7 * time.Second

		columns := bucketise(history, short.Interval, short.Duration, 90)
		measured := 0
		for _, v := range columns {
			if !math.IsNaN(v) {
				measured++
			}
		}
		for i, v := range columns[:measured] {
			if math.IsNaN(v) {
				t.Fatalf("column %d is empty inside the measured range", i)
			}
			if math.Abs(v-600) > 1e-6 {
				t.Fatalf("column %d = %v, want the interpolated 600", i, v)
			}
		}
		if measured < 80 {
			t.Fatalf("only %d of 90 columns carry data for a full-length run", measured)
		}
	})

	t.Run("nothing is invented after the last sample", func(t *testing.T) {
		columns := bucketise([]float64{100, 100}, cfg.Interval, cfg.Duration, 40)
		trailing := 0
		for _, v := range columns {
			if math.IsNaN(v) {
				trailing++
			}
		}
		if trailing < 30 {
			t.Fatalf("only %d columns left empty: the chart is drawing a future it has not measured", trailing)
		}
	})

	t.Run("a history longer than the run stays in range", func(t *testing.T) {
		history := make([]float64, 1000)
		for i := range history {
			history[i] = 500
		}
		got := bucketise(history, cfg.Interval, cfg.Duration, 8)
		if len(got) != 8 {
			t.Fatalf("got %d columns, want 8", len(got))
		}
	})
}

func TestWarmUpColumns(t *testing.T) {
	cfg := DefaultConfig() // 2.5s of warm-up out of 14s
	got := warmUpColumns(cfg.WarmUp, cfg.Duration, 40)
	if want := 7; got != want {
		t.Errorf("warmUpColumns = %d, want %d", got, want)
	}

	none := cfg
	none.WarmUp = 0
	if got := warmUpColumns(none.WarmUp, none.Duration, 40); got != 0 {
		t.Errorf("warmUpColumns = %d with no warm-up, want 0", got)
	}
}

// The chart is redrawn on every sample; none of its shapes may panic or spill.
func TestRenderChartShape(t *testing.T) {
	cfg := DefaultConfig()

	histories := map[string][]float64{
		"empty":     nil,
		"one point": {100},
		"zeroes":    {0, 0, 0, 0},
		"ramping":   rampSeries(140),
		"huge":      {12000, 9000},
		"negative":  {-5, 10},
	}

	for name, history := range histories {
		for _, width := range []int{minViewWidth, defaultViewWidth, maxViewWidth} {
			t.Run(name, func(t *testing.T) {
				rows := renderChart(history, cfg, cfg.Duration, width)
				if len(rows) != chartRows+2 {
					t.Fatalf("got %d rows, want %d plot rows plus an axis and its labels",
						len(rows), chartRows)
				}
				for i, row := range rows {
					if got := len([]rune(stripANSI(row))); got > width {
						t.Fatalf("row %d is %d wide, over the %d available", i, got, width)
					}
				}
			})
		}
	}
}

func rampSeries(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = float64(i) * 5
	}
	return s
}

// The area is filled from the bottom up: a column can never float above a gap.
func TestAreaIsFilledFromTheBottom(t *testing.T) {
	cfg := DefaultConfig()
	rows := renderChart(rampSeries(140), cfg, cfg.Duration, defaultViewWidth)

	plot := make([][]rune, chartRows)
	for i := range plot {
		plot[i] = []rune(stripANSI(rows[i]))
	}

	for col := yAxisWidth + 1; col < defaultViewWidth; col++ {
		// Scanning downwards: once a column has started drawing, every cell
		// below it must be filled too, or the area is floating.
		filling := false
		for row := range chartRows {
			blank := col >= len(plot[row]) || plot[row][col] == ' ' || plot[row][col] == '·'
			if !blank {
				filling = true
			} else if filling {
				t.Fatalf("column %d is blank at row %d but filled above it: the area is floating",
					col, row)
			}
		}
	}
}

func TestTimeTicks(t *testing.T) {
	t.Run("a zero duration has no ticks", func(t *testing.T) {
		if got := timeTicks(0, 40); got != nil {
			t.Fatalf("got %v, want no ticks", got)
		}
	})

	t.Run("ticks are inside the plot and start at zero", func(t *testing.T) {
		const plotWidth = 40
		ticks := timeTicks(14*time.Second, plotWidth)
		if len(ticks) < 2 {
			t.Fatalf("got %d ticks, want the axis labelled", len(ticks))
		}
		if ticks[0].label != "0s" || ticks[0].col != 0 {
			t.Errorf("first tick is %+v, want 0s at column 0", ticks[0])
		}
		for _, tick := range ticks {
			if tick.col < 0 || tick.col >= plotWidth {
				t.Errorf("tick %+v falls outside the plot", tick)
			}
		}
	})

	t.Run("a long run uses minutes", func(t *testing.T) {
		ticks := timeTicks(10*time.Minute, 40)
		if !strings.HasSuffix(ticks[len(ticks)-1].label, "m") {
			t.Errorf("last tick is %q, want it in minutes", ticks[len(ticks)-1].label)
		}
	})
}

// TestChartPreview asserts nothing: run it with -v to eyeball the chart.
func TestChartPreview(t *testing.T) {
	cfg := DefaultConfig()

	history := make([]float64, 0, 120)
	for i := range 120 {
		v := 640 * (1 - math.Exp(-float64(i)/12)) // TCP slow start, then a plateau
		v += 25 * math.Sin(float64(i)/3)
		history = append(history, v)
	}

	for _, n := range []int{12, 45, 120} {
		t.Logf("---- %d samples ----", n)
		for _, row := range renderChart(history[:n], cfg, cfg.Duration, defaultViewWidth) {
			t.Logf("|%s|", stripANSI(row))
		}
	}
}

// A run that settles early must not leave most of the plot empty: the axis
// shrinks to the time the run actually took.
func TestChartSpanShrinksToTheRunThatHappened(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Duration = 30 * time.Second

	history := make([]float64, 60) // 6 seconds at 100ms
	for i := range history {
		history[i] = 600
	}

	full := measuredColumns(renderChart(history, cfg, cfg.Duration, defaultViewWidth))
	short := measuredColumns(renderChart(history, cfg, 6*time.Second, defaultViewWidth))

	if short <= full*2 {
		t.Fatalf("the 6s axis fills %d columns and the 30s axis %d: shrinking the span did not fill the plot",
			short, full)
	}
}

// measuredColumns counts how many columns of the bottom plot row carry data.
func measuredColumns(rows []string) int {
	bottom := []rune(stripANSI(rows[chartRows-1]))
	n := 0
	for _, r := range bottom[min(yAxisWidth+1, len(bottom)):] {
		if r != ' ' && r != '·' {
			n++
		}
	}
	return n
}

// Regression: the midpoint offset used to be written as
// time.Duration(float64(i)+0.5)*interval. time.Duration takes whole
// nanoseconds, so the 0.5 rounded to zero and every sample was timestamped at
// the start of its window instead of the middle of it.
//
// The two only differ when a column is not a whole number of intervals wide,
// which is why the earlier tests all passed.
func TestBucketisePlacesSamplesAtTheirMidpoint(t *testing.T) {
	const (
		interval  = 100 * time.Millisecond
		span      = 1500 * time.Millisecond
		plotWidth = 10 // 150ms per column: one and a half samples wide
	)

	history := make([]float64, 10)
	for i := range history {
		history[i] = float64(i)
	}

	columns := bucketise(history, interval, span, plotWidth)

	// Sample 1 sits at 150ms, exactly on the boundary of column 1, and sample 2
	// at 250ms is inside it: the column is their average. Timestamping at the
	// window start would put sample 1 in column 0 and leave column 1 holding
	// sample 2 alone.
	if got, want := columns[1], 1.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("column 1 = %v, want %v (samples 1 and 2)", got, want)
	}
	if got, want := columns[0], 0.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("column 0 = %v, want %v (sample 0 alone)", got, want)
	}
}

// Regression: a five-character label shifted its whole row one column right,
// so the top of the plot no longer lined up with the rest of the chart.
func TestTickLabelsFitTheAxis(t *testing.T) {
	for _, peak := range []float64{0.4, 8, 95, 627, 940, 2400, 12000, 40000, 250000} {
		top := niceTop([]float64{peak})
		step := top / gridLines

		for line := 1; line <= gridLines; line++ {
			label := formatTick(step*float64(line), step)
			if len([]rune(label)) > yAxisWidth {
				t.Errorf("peak %v: label %q is %d wide, the axis has %d columns",
					peak, label, len([]rune(label)), yAxisWidth)
			}
		}
	}
}

// Every row of the chart has to be exactly as wide as every other, or the plot
// shears sideways.
func TestChartRowsLineUp(t *testing.T) {
	cfg := DefaultConfig()
	history := []float64{12000, 11000, 12500} // large enough to need "12G" labels

	rows := renderChart(history, cfg, cfg.Duration, defaultViewWidth)
	for i, row := range rows[:chartRows] {
		plain := []rune(stripANSI(row))
		if len(plain) <= yAxisWidth {
			continue // trimmed back to the axis: nothing drawn on this row
		}
		if plain[yAxisWidth] != '┤' && plain[yAxisWidth] != '│' {
			t.Errorf("row %d has %q at the axis column, want the axis: %q",
				i, string(plain[yAxisWidth]), string(plain))
		}
	}
}

// Regression: truncating to whole minutes labelled 60s and 90s identically, so
// a two-minute run came out as "0s 30s 1m 1m 2m".
func TestTimeTickLabelsAreUnique(t *testing.T) {
	for _, d := range []time.Duration{
		5 * time.Second, 14 * time.Second, 45 * time.Second,
		80 * time.Second, 2 * time.Minute, 5 * time.Minute, time.Hour,
	} {
		seen := map[string]bool{}
		for _, tick := range timeTicks(d, 60) {
			if seen[tick.label] {
				t.Errorf("duration %s labels two ticks %q", d, tick.label)
			}
			seen[tick.label] = true
		}
	}
}

// Regression: a label wider than the plot drove the start index negative and
// panicked the copy into the label row.
func TestChartSurvivesAPlotNarrowerThanItsLabels(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Duration = 3 * time.Minute // labels like "1m30"

	for width := yAxisWidth + 2; width <= minViewWidth; width++ {
		rows := renderChart([]float64{100, 200}, cfg, cfg.Duration, width)
		if len(rows) != chartRows+2 {
			t.Fatalf("width %d: got %d rows, want %d", width, len(rows), chartRows+2)
		}
	}
}
