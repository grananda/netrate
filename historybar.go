package main

import (
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// minHistoryBarCells is the narrowest bar chart worth drawing. Below it the
// chart is omitted and the history is the plain text list.
const minHistoryBarCells = 8

// barEighths are the horizontal eighths of a cell a bar can end on.
var barEighths = []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉', '█'}

// barLayout is a history bar in cells: solid whole cells before the run's min,
// spread whole cells from min to median, tip eighths (0..7) of the partial
// block that ends the bar, and reach thin cells after the bar up to max.
type barLayout struct{ solid, spread, tip, reach int }

// width is the cells the layout occupies.
func (b barLayout) width() int {
	w := b.solid + b.spread + b.reach
	if b.tip > 0 {
		w++
	}
	return w
}

// historyScale is the greatest median above 0, or 0 when there is none.
func historyScale(runs []runRecord) float64 {
	top := 0.0
	for _, r := range runs {
		if r.MedianMbps > top {
			top = r.MedianMbps
		}
	}
	return top
}

// layoutBar scales rec against top over cells cells. The median is drawn in
// eighths; min and max are placed at whole-cell resolution on the same scale
// and never change the length of the bar.
func layoutBar(rec runRecord, top float64, cells int) barLayout {
	if top <= 0 || cells <= 0 || !(rec.MedianMbps > 0) {
		return barLayout{}
	}
	med8 := max(int(math.Round(clamp01(rec.MedianMbps/top)*float64(cells*8))), 1)
	full, tip := med8/8, med8%8

	minCell, maxCell := full, 0
	if rec.MaxMbps > 0 {
		lo := math.Min(rec.MinMbps, rec.MedianMbps)
		minCell = min(int(math.Round(clamp01(lo/top)*float64(cells))), full)
		maxCell = int(math.Round(clamp01(rec.MaxMbps/top) * float64(cells)))
	}
	end := full
	if tip > 0 {
		end++
	}
	return barLayout{
		solid:  minCell,
		spread: full - minCell,
		tip:    tip,
		reach:  max(maxCell-end, 0),
	}
}

// barStyle is the headline value's white, the same for every speed: the bar's
// texture comes from shade glyphs, not from colour.
var barStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8fafc"))

// barTexture are the whole-cell glyphs of a bar: body below the run's min and
// spread from min to median. An unstable run is one shade lighter, so it
// differs from a stable one without colour.
func barTexture(rec runRecord) (body, spread rune) {
	if rec.Stable {
		return '▓', '▒'
	}
	return '▒', '░'
}

// barStyles are the bar's style and the style of its line up to max. An
// unstable run is drawn faint, the same condition as its warning mark.
func barStyles(rec runRecord) (bar, reach lipgloss.Style) {
	bar, reach = barStyle, statStyle
	if !rec.Stable {
		bar, reach = bar.Faint(true), reach.Faint(true)
	}
	return bar, reach
}

// renderBar draws rec's bar padded with spaces to exactly cells cells.
func renderBar(rec runRecord, top float64, cells int) string {
	l := layoutBar(rec, top, cells)
	bar, reach := barStyles(rec)
	body, spread := barTexture(rec)

	var b strings.Builder
	if l.solid > 0 {
		b.WriteString(bar.Render(strings.Repeat(string(body), l.solid)))
	}
	if l.spread > 0 {
		b.WriteString(bar.Render(strings.Repeat(string(spread), l.spread)))
	}
	if l.tip > 0 {
		b.WriteString(bar.Render(string(barEighths[l.tip])))
	}
	if l.reach > 0 {
		b.WriteString(reach.Render(strings.Repeat("─", l.reach)))
	}
	if pad := cells - l.width(); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}
