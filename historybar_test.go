package main

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// barRecord is a record with just the fields the bar looks at.
func barRecord(median, min, max float64, stable bool) runRecord {
	return runRecord{MedianMbps: median, MinMbps: min, MaxMbps: max, Stable: stable}
}

// AC-1, AC-2, AC-7, AC-12
func TestLayoutBarScale(t *testing.T) {
	tests := []struct {
		name   string
		median float64
		top    float64
		cells  int
		want   barLayout
	}{
		{"AC-12 the greatest median fills the chart", 100, 100, 10, barLayout{solid: 10}},
		{"AC-12 half the greatest fills half", 50, 100, 10, barLayout{solid: 5}},
		{"AC-12 rounds to the nearest eighth, not up", 31.4, 100, 10, barLayout{solid: 3, tip: 1}},
		{"AC-12 rounds to the nearest eighth, not down", 32.4, 100, 10, barLayout{solid: 3, tip: 2}},
		{"AC-12 a positive median never vanishes", 0.01, 100, 10, barLayout{tip: 1}},
		{"AC-12 a median of 0 draws no bar", 0, 100, 10, barLayout{}},
		{"AC-12 NaN draws no bar", math.NaN(), 100, 10, barLayout{}},
		{"AC-12 a negative median draws no bar", -5, 100, 10, barLayout{}},
		{"AC-7 a median above the scale is capped", 150, 100, 10, barLayout{solid: 10}},
		{"AC-7 top 0 does not divide by zero", 50, 0, 10, barLayout{}},
		{"AC-7 negative top", 50, -1, 10, barLayout{}},
		{"AC-7 no cells", 50, 100, 0, barLayout{}},
		{"AC-7 negative cells", 50, 100, -3, barLayout{}},
		{"AC-7 a single record is its own scale", 77, 77, 25, barLayout{solid: 25}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := layoutBar(barRecord(tt.median, tt.median, tt.median, true), tt.top, tt.cells)
			if got != tt.want {
				t.Errorf("layoutBar(median %v, top %v, cells %d) = %+v, want %+v", tt.median, tt.top, tt.cells, got, tt.want)
			}
		})
	}

	t.Run("AC-2 every partial glyph is reachable", func(t *testing.T) {
		for k := 0; k <= 6; k++ {
			m := 51.25 + 1.25*float64(k) // 41..47 eighths of 80
			got := layoutBar(barRecord(m, m, m, true), 100, 10)
			if want := (barLayout{solid: 5, tip: k + 1}); got != want {
				t.Errorf("median %v = %+v, want %+v", m, got, want)
			}
		}
	})

	t.Run("AC-2 close medians do not look identical", func(t *testing.T) {
		a := layoutBar(barRecord(50, 50, 50, true), 100, 10)
		b := layoutBar(barRecord(51.25, 51.25, 51.25, true), 100, 10)
		if a == b {
			t.Errorf("50 and 51.25 both lay out as %+v", a)
		}
	})

	t.Run("AC-1 length is proportional to the median", func(t *testing.T) {
		a := layoutBar(barRecord(25, 25, 25, true), 100, 40)
		b := layoutBar(barRecord(50, 50, 50, true), 100, 40)
		if a.solid*2 != b.solid {
			t.Errorf("25 -> %d cells, 50 -> %d cells, want double", a.solid, b.solid)
		}
	})
}

// AC-11
func TestLayoutBarRange(t *testing.T) {
	tests := []struct {
		name     string
		median   float64
		min, max float64
		want     barLayout
	}{
		{"min = median = max is a plain bar", 50, 50, 50, barLayout{solid: 5}},
		{"min below the median", 50, 20, 50, barLayout{solid: 2, spread: 3}},
		{"a different min renders differently", 50, 30, 50, barLayout{solid: 3, spread: 2}},
		{"max above the median", 50, 50, 80, barLayout{solid: 5, reach: 3}},
		{"a different max renders differently", 50, 50, 70, barLayout{solid: 5, reach: 2}},
		{"max past the edge is cut", 50, 50, 1000, barLayout{solid: 5, reach: 5}},
		{"missing range is a plain bar", 50, 0, 0, barLayout{solid: 5}},
		{"min above the median is ignored", 50, 70, 50, barLayout{solid: 5}},
		{"the range reaches past a partial tip", 55, 55, 80, barLayout{solid: 5, tip: 4, reach: 2}},
		{"min below the median on the fastest row", 100, 50, 300, barLayout{solid: 5, spread: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := layoutBar(barRecord(tt.median, tt.min, tt.max, true), 100, 10)
			if got != tt.want {
				t.Errorf("= %+v, want %+v", got, tt.want)
			}
			if got.width() > 10 {
				t.Errorf("width %d exceeds the 10 cells", got.width())
			}
			plain := layoutBar(barRecord(tt.median, tt.median, tt.median, true), 100, 10)
			body := func(b barLayout) int {
				n := b.solid + b.spread
				if b.tip > 0 {
					n++
				}
				return n
			}
			if body(got) != body(plain) || got.tip != plain.tip {
				t.Errorf("the range changed the bar: %+v vs plain %+v", got, plain)
			}
		})
	}

	t.Run("two records with the same median and different min or max differ", func(t *testing.T) {
		a := layoutBar(barRecord(50, 45, 55, true), 100, 20)
		b := layoutBar(barRecord(50, 10, 90, true), 100, 20)
		if a == b {
			t.Errorf("both lay out as %+v", a)
		}
	})
}

// AC-2, AC-11, AC-12
func TestRenderBarGlyphs(t *testing.T) {
	got := stripANSI(renderBar(barRecord(55, 20, 80, true), 100, 10))
	if want := "▓▓▒▒▒▌──  "; got != want {
		t.Errorf("renderBar = %q, want %q", got, want)
	}
	if got := stripANSI(renderBar(barRecord(100, 100, 100, true), 100, 10)); got != strings.Repeat("▓", 10) {
		t.Errorf("AC-12 fastest = %q, want ten ▓", got)
	}
	if got := stripANSI(renderBar(barRecord(100, 0, 100, true), 100, 10)); got != strings.Repeat("▒", 10) {
		t.Errorf("AC-11 a stalled run = %q, want ten ▒ (it stays visible)", got)
	}
	if got := stripANSI(renderBar(barRecord(50, 50, 50, true), 100, 10)); got != "▓▓▓▓▓     " {
		t.Errorf("AC-12 half = %q, want five ▓ and five spaces", got)
	}
	if got := stripANSI(renderBar(barRecord(0, 0, 0, true), 100, 10)); got != strings.Repeat(" ", 10) {
		t.Errorf("AC-12 median 0 = %q, want ten spaces", got)
	}
	if got := stripANSI(renderBar(barRecord(0.01, 0.01, 0.01, true), 100, 10)); !strings.HasPrefix(got, "▏") {
		t.Errorf("AC-12 tiny median = %q, want it to start with ▏", got)
	}
	for _, cells := range []int{8, 25, 53} {
		for _, rec := range []runRecord{
			barRecord(100, 40, 100, true), barRecord(37, 10, 5000, false),
			barRecord(0, 0, 0, true), barRecord(99.9, 99.9, 99.9, true),
		} {
			if w := lipgloss.Width(renderBar(rec, 100, cells)); w != cells {
				t.Errorf("cells %d, median %v: width %d", cells, rec.MedianMbps, w)
			}
		}
	}
}

// AC-4, AC-14, AC-11
func TestRenderBarUnstable(t *testing.T) {
	if got := stripANSI(renderBar(barRecord(55, 20, 80, false), 100, 10)); got != "▒▒░░░▌──  " {
		t.Errorf("AC-14 unstable bar = %q, want %q", got, "▒▒░░░▌──  ")
	}
	if got := stripANSI(renderBar(barRecord(100, 100, 100, false), 100, 10)); got != strings.Repeat("▒", 10) {
		t.Errorf("AC-14 unstable fastest = %q, want ten ▒", got)
	}

	prefix := func(s string) int {
		n := 0
		for _, r := range s {
			if r != ' ' && r != '─' {
				n++
			}
		}
		return n
	}
	for _, median := range []float64{30, 55, 100} {
		t.Run(fmt.Sprintf("median %v differs from stable by glyphs", median), func(t *testing.T) {
			stable := stripANSI(renderBar(barRecord(median, 20, 80, true), 100, 10))
			unstable := stripANSI(renderBar(barRecord(median, 20, 80, false), 100, 10))
			if stable == unstable {
				t.Errorf("AC-14 stable and unstable are both %q", stable)
			}
			if prefix(stable) != prefix(unstable) {
				t.Errorf("AC-4 the length changed with stability: %q vs %q", stable, unstable)
			}
		})
	}

	t.Run("a bar under one cell differs only by style (documented limit)", func(t *testing.T) {
		want := "▏" + strings.Repeat(" ", 9)
		for _, stable := range []bool{true, false} {
			if got := stripANSI(renderBar(barRecord(0.01, 0.01, 0.01, stable), 100, 10)); got != want {
				t.Errorf("stable=%v: %q, want %q", stable, got, want)
			}
		}
	})
}

// AC-19, AC-4, AC-14
func TestBarStylesAreWhite(t *testing.T) {
	white, grey := lipgloss.Color("#f8fafc"), lipgloss.Color("#94a3b8")
	for _, m := range []float64{0.5, 25, 75, 400, 2000} {
		for _, stable := range []bool{true, false} {
			t.Run(fmt.Sprintf("median %v stable %v", m, stable), func(t *testing.T) {
				bar, reach := barStyles(barRecord(m, m/2, m*2, stable))
				if bar.GetForeground() != white {
					t.Errorf("AC-19 bar foreground = %v, want #f8fafc whatever the speed", bar.GetForeground())
				}
				if reach.GetForeground() != grey {
					t.Errorf("AC-19 reach foreground = %v, want #94a3b8", reach.GetForeground())
				}
				if bar.GetFaint() == stable || reach.GetFaint() == stable {
					t.Errorf("AC-14 faint bar=%v reach=%v, want both %v", bar.GetFaint(), reach.GetFaint(), !stable)
				}
			})
		}
	}
}

// sgrSegment is a run of text and the style in force while it was written.
type sgrSegment struct {
	text  string
	fg    string
	faint bool
}

// parseSGR splits s into styled segments by reading its \x1b[...m sequences.
func parseSGR(t *testing.T, s string) []sgrSegment {
	t.Helper()
	var out []sgrSegment
	var cur sgrSegment
	flush := func(text string) {
		if text != "" {
			c := cur
			c.text = text
			out = append(out, c)
		}
	}
	for len(s) > 0 {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			flush(s)
			break
		}
		flush(s[:i])
		s = s[i+2:]
		j := strings.IndexByte(s, 'm')
		if j < 0 {
			t.Fatalf("unterminated escape sequence in %q", s)
		}
		params := strings.Split(s[:j], ";")
		s = s[j+1:]
		for k := 0; k < len(params); k++ {
			switch params[k] {
			case "", "0":
				cur = sgrSegment{}
			case "2":
				cur.faint = true
			case "38":
				if k+4 < len(params) && params[k+1] == "2" {
					cur.fg = strings.Join(params[k:k+5], ";")
					k += 4
				}
			}
		}
	}
	return out
}

// AC-19, AC-14, AC-11 (closes CR-1: the style wiring is checked in the output)
func TestRenderBarStyledOutput(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0) // true colour
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	// Derive the expected foregrounds the way the renderer does: termenv
	// converts hex with uint8(f*255), which truncates (0x94 -> 147, not 148),
	// so hard-coded RGB values would not match. Probe the same colours.
	probe := func(hex string) string {
		segs := parseSGR(t, lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render("x"))
		if len(segs) != 1 || segs[0].fg == "" {
			t.Fatalf("cannot derive the foreground of %s from %+v", hex, segs)
		}
		return segs[0].fg
	}
	white, grey := probe("#f8fafc"), probe("#94a3b8")
	if white == grey {
		t.Fatalf("probe colours are identical: %q", white)
	}

	check := func(t *testing.T, rec runRecord, top float64, cells int, wantFaint bool) {
		t.Helper()
		out := renderBar(rec, top, cells)
		if !strings.Contains(out, "\x1b[") {
			t.Fatalf("no escape sequences in %q: the colour profile was not forced", out)
		}
		segs := parseSGR(t, out)
		seen := 0
		for _, sg := range segs {
			if strings.Trim(sg.text, " ") == "" {
				continue
			}
			seen++
			want := white
			if strings.Contains(sg.text, "─") {
				want = grey
				if strings.Trim(sg.text, "─ ") != "" {
					t.Errorf("segment %q mixes the range with the bar", sg.text)
				}
			}
			if sg.fg != want {
				t.Errorf("AC-19 segment %q has foreground %q, want %q", sg.text, sg.fg, want)
			}
			if sg.faint != wantFaint {
				t.Errorf("AC-14 segment %q faint=%v, want %v", sg.text, sg.faint, wantFaint)
			}
		}
		if seen == 0 {
			t.Errorf("no visible segment in %q", out)
		}
	}

	t.Run("AC-19 AC-11 bar white and range grey", func(t *testing.T) {
		out := renderBar(barRecord(55, 20, 80, true), 100, 10)
		check(t, barRecord(55, 20, 80, true), 100, 10, false)
		if !strings.Contains(out, grey) {
			t.Errorf("no grey sequence for the range in %q", out)
		}
	})
	t.Run("AC-19 the colour does not depend on the speed", func(t *testing.T) {
		check(t, barRecord(0.5, 0.5, 0.5, true), 0.5, 10, false)
		check(t, barRecord(2000, 2000, 2000, true), 2000, 10, false)
	})
	t.Run("AC-14 unstable is faint everywhere", func(t *testing.T) {
		check(t, barRecord(55, 20, 80, false), 100, 10, true)
	})
	t.Run("AC-14 stable is never faint", func(t *testing.T) {
		check(t, barRecord(100, 100, 100, true), 100, 10, false)
	})
}

func TestHistoryScale(t *testing.T) {
	rec := func(m float64) runRecord { return barRecord(m, m, m, true) }
	tests := []struct {
		name string
		runs []runRecord
		want float64
	}{
		{"AC-12 nil", nil, 0},
		{"AC-12 all zero", []runRecord{rec(0), rec(0)}, 0},
		{"AC-17 the greatest median", []runRecord{rec(0), rec(50), rec(400), rec(120)}, 400},
		{"AC-7 single record", []runRecord{rec(37.5)}, 37.5},
		{"AC-7 all equal", []runRecord{rec(80), rec(80), rec(80)}, 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := historyScale(tt.runs); got != tt.want {
				t.Errorf("historyScale = %v, want %v", got, tt.want)
			}
		})
	}
}
