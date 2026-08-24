package main

import (
	"fmt"
	"math"
)

// formatMbps renders a speed with the unit that keeps it readable.
func formatMbps(v float64) string {
	value, unit := mbpsParts(v)
	return value + " " + unit
}

// mbpsParts is formatMbps with the number and the unit kept apart, so the UI
// can style them differently without having to split the string back up.
func mbpsParts(v float64) (value, unit string) {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return "—", "Mbps"
	case v >= 1000:
		return fmt.Sprintf("%.2f", v/1000), "Gbps"
	case v >= 100:
		return fmt.Sprintf("%.1f", v), "Mbps"
	default:
		return fmt.Sprintf("%.2f", v), "Mbps"
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// clamp01 pins v to [0,1]. NaN clamps to 0 rather than propagating into the
// chart geometry, where it would silently blank a column.
func clamp01(v float64) float64 {
	switch {
	case math.IsNaN(v), v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
