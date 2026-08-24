package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

const (
	chartRows  = 8 // rows of the plot area
	yAxisWidth = 4 // columns reserved for the y-axis labels
	gridEvery  = 2 // label every other row
	gridLines  = chartRows / gridEvery
	gridDotted = 3 // draw a gridline dot every N columns

	// The view is fixed unless the terminal tells us its size, and clamped
	// either way: a chart narrower than this cannot show a shape, and one
	// wider stops being readable as a single glance.
	defaultViewWidth = 46
	minViewWidth     = 32
	maxViewWidth     = 100
)

// levels are the eighths of a cell an area column can be filled to. Working in
// eighths is what gives the chart eight times the vertical resolution the row
// count suggests, and why it can stay solid instead of dithered.
var levels = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

var (
	axisStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#4b5563"))
	gridStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#334155"))
	tickStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280"))
	warmUpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#4b5563"))
)

// ---------------------------------------------------------------------------
// Colour
// ---------------------------------------------------------------------------

// Colour is keyed to speed on the same non-linear anchors the old dial used: a
// real link spends most of its life in the bottom decade, so an evenly spaced
// 0..1000 ramp would give every ordinary reading the same shade of blue.
var colourAnchors = []float64{0, 10, 50, 100, 250, 500, 1000}

// gradientStops colour the area by speed. Hex is deliberate: lipgloss
// down-converts on terminals that need it, but a truecolour terminal gets a
// genuinely smooth ramp instead of a handful of visible bands.
var gradientStops = []struct {
	at  float64
	rgb [3]float64
}{
	{0.00, [3]float64{0x1d, 0x4e, 0xd8}}, // blue
	{0.28, [3]float64{0x06, 0x91, 0xb2}}, // cyan
	{0.52, [3]float64{0x05, 0x96, 0x69}}, // emerald
	{0.76, [3]float64{0x65, 0xa3, 0x0d}}, // lime
	{1.00, [3]float64{0xea, 0xb3, 0x08}}, // amber
}

const rampSteps = 64

var rampStyles = buildRamp()

func buildRamp() []lipgloss.Style {
	styles := make([]lipgloss.Style, rampSteps)
	for i := range styles {
		styles[i] = styleOf(gradientAt(float64(i) / float64(rampSteps-1)))
	}
	return styles
}

func styleOf(r, g, b float64) lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", int(r), int(g), int(b))))
}

func gradientAt(t float64) (r, g, b float64) {
	t = clamp01(t)
	for i := 0; i < len(gradientStops)-1; i++ {
		lo, hi := gradientStops[i], gradientStops[i+1]
		if t > hi.at {
			continue
		}
		k := 0.0
		if span := hi.at - lo.at; span > 0 {
			k = (t - lo.at) / span
		}
		return lo.rgb[0] + (hi.rgb[0]-lo.rgb[0])*k,
			lo.rgb[1] + (hi.rgb[1]-lo.rgb[1])*k,
			lo.rgb[2] + (hi.rgb[2]-lo.rgb[2])*k
	}
	last := gradientStops[len(gradientStops)-1].rgb
	return last[0], last[1], last[2]
}

// speedColour maps Mbps onto the gradient.
func speedColour(v float64) lipgloss.Style {
	return rampStyles[int(clamp01(speedToFrac(v))*float64(rampSteps-1))]
}

// speedToFrac maps Mbps onto 0..1 along the non-linear anchors.
func speedToFrac(v float64) float64 {
	last := len(colourAnchors) - 1
	switch {
	case math.IsNaN(v), v <= colourAnchors[0]:
		return 0
	case v >= colourAnchors[last]:
		return 1
	}
	for i := range last {
		lo, hi := colourAnchors[i], colourAnchors[i+1]
		if v <= hi {
			return (float64(i) + (v-lo)/(hi-lo)) / float64(last)
		}
	}
	return 1
}

// ---------------------------------------------------------------------------
// Chart
// ---------------------------------------------------------------------------

// renderChart draws the sample history as a filled area: speed up the y axis,
// elapsed time along the x. This is the shape the engine actually measures and
// a single needle throws away — TCP slow start on the left, the plateau in the
// middle, and how much that plateau wobbles.
//
// span is what the x axis covers. While a run is live that is the duration
// budget, so the chart fills left to right instead of rescaling under the
// reader's eyes; once it has finished it is the time the run actually took, so
// a run that settled early does not leave two thirds of the plot empty.
func renderChart(history []float64, cfg Config, span time.Duration, width int) []string {
	plotWidth := max(width-yAxisWidth-1, 1)
	columns := bucketise(history, cfg.Interval, span, plotWidth)
	top := niceTop(columns)
	warmCols := warmUpColumns(cfg.WarmUp, span, plotWidth)

	rows := make([]string, 0, chartRows+2)
	for row := range chartRows {
		rowTop := top * float64(chartRows-row) / chartRows
		rowFloor := top * float64(chartRows-row-1) / chartRows

		var b strings.Builder
		b.WriteString(yAxisCell(row, rowTop, top/gridLines))
		for col, v := range columns {
			cell := areaCell(v, rowFloor, rowTop-rowFloor, col < warmCols)
			if cell == " " {
				cell = gridCell(row, col)
			}
			b.WriteString(cell)
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}

	ticks := timeTicks(span, plotWidth)
	return append(rows, axisRow(plotWidth, warmCols, ticks), timeLabelRow(plotWidth, ticks))
}

// bucketise folds the sample history down to one value per column, averaging
// where several samples share a column. Averaging rather than taking the peak
// keeps the plateau flat instead of turning every column into a spike.
func bucketise(history []float64, interval, span time.Duration, plotWidth int) []float64 {
	columns := make([]float64, plotWidth)
	counts := make([]int, plotWidth)
	for i := range columns {
		columns[i] = math.NaN() // not measured yet
	}

	perColumn := span / time.Duration(plotWidth)
	if perColumn <= 0 || interval <= 0 {
		return columns
	}

	for i, v := range history {
		// A sample is the average over its own window, so it belongs at the
		// middle of that window. Timestamping it at either edge pushes the
		// sample that lands on a column boundary into the wrong column.
		//
		// The halving has to happen in Duration arithmetic: time.Duration takes
		// whole nanoseconds, so time.Duration(float64(i)+0.5) rounds the offset
		// straight back down to zero.
		at := interval*time.Duration(i) + interval/2
		col := min(int(at/perColumn), plotWidth-1)
		if counts[col] == 0 {
			columns[col] = 0
		}
		columns[col] += v
		counts[col]++
	}
	for i, n := range counts {
		if n > 0 {
			columns[i] /= float64(n)
		}
	}
	fillGaps(columns)
	return columns
}

// fillGaps interpolates the columns no sample landed in. On a terminal wider
// than the run has samples — 90 columns for 70 readings — every third column
// would otherwise come out empty and the area would read as a comb.
//
// Only gaps between two measurements are filled. Everything after the last one
// stays empty: the run has not got there yet, and drawing it would invent data.
func fillGaps(columns []float64) {
	prev := -1
	for i, v := range columns {
		if math.IsNaN(v) {
			continue
		}
		if prev >= 0 && i-prev > 1 {
			step := (v - columns[prev]) / float64(i-prev)
			for gap := prev + 1; gap < i; gap++ {
				columns[gap] = columns[prev] + step*float64(gap-prev)
			}
		}
		prev = i
	}
}

// warmUpColumns is how many columns fall inside the discarded warm-up window.
func warmUpColumns(warmUp, span time.Duration, plotWidth int) int {
	if span <= 0 || warmUp <= 0 {
		return 0
	}
	return min(int(float64(plotWidth)*float64(warmUp)/float64(span)), plotWidth)
}

// areaCell fills one cell of one column, from the bottom up.
func areaCell(v, floor, rowSpan float64, warmUp bool) string {
	if math.IsNaN(v) || rowSpan <= 0 {
		return " "
	}
	level := int(math.Round(clamp01((v-floor)/rowSpan) * float64(len(levels)-1)))
	if level == 0 {
		return " "
	}
	if warmUp {
		// Drawn muted: these readings are shown for context but thrown away
		// before any statistic is computed.
		return warmUpStyle.Render(string(levels[level]))
	}
	return speedColour(v).Render(string(levels[level]))
}

// gridCell draws a faint dotted gridline across the labelled rows, so the part
// of the plot the reading has not reached still reads as a chart rather than as
// blank space.
func gridCell(row, col int) string {
	if (chartRows-row)%gridEvery != 0 || col%gridDotted != 0 {
		return " "
	}
	return gridStyle.Render("·")
}

func yAxisCell(row int, value, step float64) string {
	if (chartRows-row)%gridEvery != 0 {
		return strings.Repeat(" ", yAxisWidth) + axisStyle.Render("│")
	}
	label := fmt.Sprintf("%*s", yAxisWidth, formatTick(value, step))
	return tickStyle.Render(label) + axisStyle.Render("┤")
}

func axisRow(plotWidth, warmCols int, ticks []timeTick) string {
	line := make([]rune, plotWidth)
	for i := range line {
		line[i] = '─'
	}
	for _, t := range ticks {
		if t.col > 0 {
			line[t.col] = '┬' // column zero is already marked by the origin
		}
	}

	// The warm-up stretch is drawn muted and dashed, so the part of the run
	// that does not count is obvious without a legend.
	warm := ""
	if warmCols > 0 {
		warm = warmUpStyle.Render(strings.ReplaceAll(string(line[:warmCols]), "─", "╌"))
	}
	return tickStyle.Render(fmt.Sprintf("%*s", yAxisWidth, "0")) +
		axisStyle.Render("┼") + warm + axisStyle.Render(string(line[warmCols:]))
}

func timeLabelRow(plotWidth int, ticks []timeTick) string {
	row := []rune(strings.Repeat(" ", plotWidth))
	nextFree := 0
	for _, t := range ticks {
		text := []rune(t.label)
		if len(text) > plotWidth {
			continue // narrower than its own label; nothing sensible to draw
		}
		start := min(max(t.col, nextFree), plotWidth-len(text))
		if start < nextFree {
			continue // no room; the notch on the axis has to speak for it
		}
		copy(row[start:], text)
		nextFree = start + len(text) + 1
	}
	return strings.Repeat(" ", yAxisWidth+1) + tickStyle.Render(strings.TrimRight(string(row), " "))
}

// ---------------------------------------------------------------------------
// Axis scales
// ---------------------------------------------------------------------------

// niceTop picks the top of the y axis: the smallest round number that clears
// the data and divides evenly into the gridlines, so the labels read 0/250/
// 500/750/1000 rather than 0/227/454/681/908.
func niceTop(columns []float64) float64 {
	peak := 0.0
	for _, v := range columns {
		if !math.IsNaN(v) && v > peak {
			peak = v
		}
	}
	if peak <= 0 {
		return 10 // an empty chart still needs a scale to draw
	}

	step := niceStep(peak / gridLines)
	for step*gridLines < peak {
		step = niceStep(step * 1.2)
	}
	return step * gridLines
}

// niceStep rounds up to the next number people read without effort.
func niceStep(raw float64) float64 {
	if raw <= 0 {
		return 1
	}
	magnitude := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, n := range []float64{1, 2, 2.5, 3, 4, 5} {
		if raw <= n*magnitude {
			return n * magnitude
		}
	}
	return 10 * magnitude
}

// formatTick must never exceed yAxisWidth: a wider label shifts its whole row
// one column right and the plot stops lining up with the rest of the chart.
func formatTick(value, step float64) string {
	switch {
	case value >= 10000:
		return fmt.Sprintf("%.0fG", value/1000) // "12G": "12.0G" would not fit
	case value >= 1000:
		return fmt.Sprintf("%.1fG", value/1000)
	case step < 1:
		return fmt.Sprintf("%.2f", value)
	case step < 10:
		return fmt.Sprintf("%.1f", value)
	default:
		return fmt.Sprintf("%.0f", value)
	}
}

type timeTick struct {
	col   int
	label string
}

// timeTicks spaces the x axis on round intervals that fit the run.
func timeTicks(duration time.Duration, plotWidth int) []timeTick {
	if duration <= 0 || plotWidth < 2 {
		return nil
	}

	step := niceTimeStep(duration / gridLines)
	var ticks []timeTick
	for at := time.Duration(0); at <= duration; at += step {
		col := int(float64(plotWidth-1) * float64(at) / float64(duration))
		ticks = append(ticks, timeTick{col: col, label: formatSeconds(at)})
	}
	return ticks
}

func niceTimeStep(raw time.Duration) time.Duration {
	for _, step := range []time.Duration{
		time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second,
		15 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute,
	} {
		if raw <= step {
			return step
		}
	}
	return 10 * time.Minute
}

func formatSeconds(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		// Truncating to whole minutes labels 60s and 90s identically, so a
		// two-minute run came out as "0s 30s 1m 1m 2m".
		return fmt.Sprintf("%dm%02d", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// clampViewWidth keeps the layout inside the range it was designed for.
func clampViewWidth(w int) int {
	return min(max(w, minViewWidth), maxViewWidth)
}
