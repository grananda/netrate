package main

import (
	"math"
	"strings"
	"testing"
)

func TestFormatMbps(t *testing.T) {
	tests := []struct {
		v    float64
		want string
	}{
		{0, "0.00 Mbps"},
		{9.456, "9.46 Mbps"},
		{99.99, "99.99 Mbps"},
		{100, "100.0 Mbps"},
		{940.5, "940.5 Mbps"},
		{1000, "1.00 Gbps"},
		{2500, "2.50 Gbps"},
	}
	for _, tc := range tests {
		if got := formatMbps(tc.v); got != tc.want {
			t.Errorf("formatMbps(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

// The UI renders the number and the unit in different styles, so they have to
// come apart cleanly rather than by splitting the formatted string.
func TestMbpsPartsAlwaysHasAUnit(t *testing.T) {
	for _, v := range []float64{0, 50, 100, 1500, math.NaN(), math.Inf(1)} {
		value, unit := mbpsParts(v)
		if value == "" || unit == "" {
			t.Errorf("mbpsParts(%v) = %q, %q: both halves must be present", v, value, unit)
		}
		if strings.Contains(value, " ") {
			t.Errorf("mbpsParts(%v) value %q contains a space", v, value)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{5 * 1024 * 1024 * 1024 * 1024, "5.0 TiB"},
	}
	for _, tc := range tests {
		if got := formatBytes(tc.n); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestClamp01(t *testing.T) {
	tests := []struct {
		v    float64
		want float64
	}{
		{-1, 0}, {0, 0}, {0.5, 0.5}, {1, 1}, {2, 1}, {math.NaN(), 0},
	}
	for _, tc := range tests {
		if got := clamp01(tc.v); got != tc.want {
			t.Errorf("clamp01(%v) = %v, want %v", tc.v, got, tc.want)
		}
	}
}
