package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestFlow drives the real Bubble Tea program headlessly against the real
// network, and checks that the engine's events keep reaching Update past the
// first one. Everything else is covered offline by the other tests; this one
// exists to catch the wiring that only breaks against a real link.
func TestFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("needs network access; run go test without -short")
	}

	cfg := DefaultConfig()
	cfg.Duration = 6 * time.Second
	cfg.WarmUp = time.Second
	cfg.MinRun = 3 * time.Second

	m := newModel(cfg)
	defer m.cancel()

	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithoutRenderer())

	// The gauge deliberately stays up after the run so the reading can be read
	// and re-run, so the test has to press "q" itself. The delay is the run
	// plus enough slack for a slow link to finish the download phase.
	quit := time.AfterFunc(cfg.Duration+5*time.Second, func() {
		p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	})
	defer quit.Stop()

	final, err := p.Run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	last, ok := final.(model)
	if !ok {
		t.Fatalf("final model is %T, want model", final)
	}
	defer last.cancel()

	t.Logf("phase=%q samples=%d peak=%.1f bytes=%d", last.phase, len(last.history), last.peak, last.bytes)

	if last.state != stateDone {
		t.Fatalf("state %v, err=%v (wanted stateDone)", last.state, last.err)
	}
	if len(last.history) < 10 {
		t.Fatalf("only %d samples reached the UI", len(last.history))
	}
	if last.result.Median <= 0 {
		t.Fatalf("invalid median: %v", last.result.Median)
	}
	t.Logf("result: %s (CV %.1f%%, stable=%v)",
		formatMbps(last.result.Median), last.result.CV, last.result.Stable)
}
