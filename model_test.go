package main

import (
	"errors"
	"math"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// offlineConfig fails Validate, so Run rejects it and returns immediately
// without touching the network. The UI tests drive events by hand anyway.
func offlineConfig() Config {
	cfg := DefaultConfig()
	cfg.URL = "not-a-url"
	return cfg
}

func newTestModel(t *testing.T) model {
	t.Helper()
	m := newModel(offlineConfig())
	t.Cleanup(m.cancel)
	return m
}

func update(t *testing.T, m model, msg any) model {
	t.Helper()
	next, _ := m.Update(msg)
	got, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	return got
}

func TestApplyEvent(t *testing.T) {
	t.Run("a sample updates the live readout", func(t *testing.T) {
		m := newTestModel(t)
		m = update(t, m, engineMsg{epoch: m.epoch, ev: SampleEvent{Sample: Sample{
			At: time.Second, Mbps: 42, Bytes: 999, Warm: true,
		}}})

		if m.readout <= 0 || m.peak != 42 || m.bytes != 999 || m.elapsed != time.Second {
			t.Fatalf("sample not applied: %+v", m)
		}
		if len(m.history) != 1 {
			t.Fatalf("history has %d entries, want 1", len(m.history))
		}
	})

	t.Run("a cold sample does not move the peak", func(t *testing.T) {
		m := newTestModel(t)
		m = update(t, m, engineMsg{epoch: m.epoch, ev: SampleEvent{Sample: Sample{Mbps: 900, Warm: false}}})

		if m.peak != 0 {
			t.Fatalf("peak = %v, want warm-up readings excluded", m.peak)
		}
	})

	t.Run("done switches state", func(t *testing.T) {
		m := newTestModel(t)
		m = update(t, m, engineMsg{epoch: m.epoch, ev: DoneEvent{Result: Result{Median: 100}}})

		if m.state != stateDone || m.phase != phaseComplete {
			t.Fatalf("state = %v phase = %q, want done", m.state, m.phase)
		}
	})

	t.Run("an error switches state", func(t *testing.T) {
		m := newTestModel(t)
		m = update(t, m, engineMsg{epoch: m.epoch, ev: ErrEvent{Err: errors.New("boom")}})

		if m.state != stateError || m.err == nil {
			t.Fatalf("state = %v err = %v, want an error", m.state, m.err)
		}
	})
}

// A channel that closes before any verdict means something went wrong.
func TestClosedChannelFailsARunningModel(t *testing.T) {
	m := newTestModel(t)
	m = update(t, m, closedMsg{epoch: m.epoch})

	if m.state != stateError {
		t.Fatalf("state = %v, want stateError", m.state)
	}
	if m.errorText() == "" {
		t.Fatal("a failed model must have something to show the user")
	}
}

// ...but a channel closing after a verdict is just the engine tidying up.
func TestClosedChannelKeepsAFinishedResult(t *testing.T) {
	m := newTestModel(t)
	m = update(t, m, engineMsg{epoch: m.epoch, ev: DoneEvent{Result: Result{Median: 100}}})
	m = update(t, m, closedMsg{epoch: m.epoch})

	if m.state != stateDone {
		t.Fatalf("state = %v, want the result to survive the channel closing", m.state)
	}
}

// Regression: pressing "r" swaps in a new model while the previous run's frame
// tick and pending channel read are still in flight. Without the epoch those
// stale messages land on the fresh model — the closedMsg flags it as failed,
// and the frameMsg leaves a second ticker running, doubling the frame rate on
// every restart.
func TestStaleMessagesFromAPreviousRunAreIgnored(t *testing.T) {
	m := newTestModel(t)
	m = update(t, m, engineMsg{epoch: m.epoch, ev: DoneEvent{Result: Result{Median: 100}}})

	restarted, _ := m.restart()
	t.Cleanup(restarted.cancel)

	if restarted.epoch == m.epoch {
		t.Fatal("restart did not bump the epoch")
	}
	if restarted.state != stateRunning {
		t.Fatalf("state = %v, want a fresh run", restarted.state)
	}

	t.Run("a stale closed channel does not fail the new run", func(t *testing.T) {
		got := update(t, restarted, closedMsg{epoch: m.epoch})
		if got.state != stateRunning {
			t.Fatalf("state = %v, want the stale close to be ignored", got.state)
		}
	})

	t.Run("a stale sample does not corrupt the new run", func(t *testing.T) {
		got := update(t, restarted, engineMsg{epoch: m.epoch, ev: SampleEvent{Sample: Sample{Mbps: 777, Warm: true}}})
		if got.peak != 0 || len(got.history) != 0 {
			t.Fatalf("stale sample leaked into the new run: peak=%v history=%d", got.peak, len(got.history))
		}
	})

}

// The headline number is eased towards each sample so it does not flicker
// through digits, but a verdict must land on the exact figure.
func TestHeadlineEasesThenSnapsToTheVerdict(t *testing.T) {
	m := newTestModel(t)
	for range 3 {
		m = update(t, m, engineMsg{epoch: m.epoch, ev: SampleEvent{Sample: Sample{Mbps: 100, Warm: true}}})
	}
	if m.readout <= 0 || m.readout >= 100 {
		t.Fatalf("readout = %v, want it easing between 0 and the reading", m.readout)
	}

	m = update(t, m, engineMsg{epoch: m.epoch, ev: DoneEvent{Result: Result{Median: 623.4}}})
	if m.readout != 623.4 {
		t.Fatalf("readout = %v, want it to land exactly on the median", m.readout)
	}
}

// The chart is worth having only if it uses the terminal it is given.
func TestWindowSizeResizesTheView(t *testing.T) {
	m := newTestModel(t)
	if got := m.viewWidth(); got != defaultViewWidth {
		t.Fatalf("viewWidth = %d before the terminal reports, want the default %d", got, defaultViewWidth)
	}

	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if got := m.viewWidth(); got != maxViewWidth {
		t.Fatalf("viewWidth = %d on a 120-column terminal, want it clamped to %d", got, maxViewWidth)
	}

	m = update(t, m, tea.WindowSizeMsg{Width: 20, Height: 40})
	if got := m.viewWidth(); got != minViewWidth {
		t.Fatalf("viewWidth = %d on a 20-column terminal, want it clamped to %d", got, minViewWidth)
	}
}

// A restart keeps the terminal size: the run changes, the window does not.
func TestRestartKeepsTheWindowSize(t *testing.T) {
	m := newTestModel(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 40})
	m = update(t, m, engineMsg{epoch: m.epoch, ev: DoneEvent{Result: Result{Median: 100}}})

	restarted, _ := m.restart()
	t.Cleanup(restarted.cancel)

	if restarted.viewWidth() != m.viewWidth() {
		t.Fatalf("width %d after restart, want the %d it had", restarted.viewWidth(), m.viewWidth())
	}
}

func TestAppendCapped(t *testing.T) {
	var series []float64
	for i := range 10 {
		series = appendCapped(series, float64(i), 4)
	}
	if len(series) != 4 {
		t.Fatalf("len = %d, want the series capped at 4", len(series))
	}
	for i, want := range []float64{6, 7, 8, 9} {
		if series[i] != want {
			t.Fatalf("series = %v, want the 4 newest readings", series)
		}
	}
}

// View runs on every frame; none of its states may panic.
func TestViewRendersEveryState(t *testing.T) {
	states := map[string]func(model) model{
		"running": func(m model) model { return m },
		"sampling": func(m model) model {
			return update(t, m, engineMsg{epoch: m.epoch, ev: SampleEvent{Sample: Sample{Mbps: 120, Warm: true}}})
		},
		"done": func(m model) model {
			return update(t, m, engineMsg{epoch: m.epoch, ev: DoneEvent{Result: Result{Median: 100, Stable: true}}})
		},
		"unstable": func(m model) model {
			return update(t, m, engineMsg{epoch: m.epoch, ev: DoneEvent{Result: Result{Median: 100, CV: 30}}})
		},
		"error": func(m model) model {
			return update(t, m, engineMsg{epoch: m.epoch, ev: ErrEvent{Err: errors.New("boom")}})
		},
		"error without a message": func(m model) model {
			m.state = stateError
			return m
		},
	}

	for name, setup := range states {
		t.Run(name, func(t *testing.T) {
			if out := setup(newTestModel(t)).View(); out == "" {
				t.Fatal("View rendered nothing")
			}
		})
	}
}

// TestViewPreview asserts nothing: run it with -v to eyeball the whole screen.
func TestViewPreview(t *testing.T) {
	live := newModel(DefaultConfig())
	t.Cleanup(live.cancel)

	for i := range 96 {
		v := 640*(1-math.Exp(-float64(i)/12)) + 25*math.Sin(float64(i)/3)
		live.history = append(live.history, v)
		if v > live.peak {
			live.peak = v
		}
	}
	live.readout, live.warm = 627.6, true
	live.elapsed, live.bytes = 9600*time.Millisecond, 407800000
	live.phase = phaseMeasuring

	t.Log("\n" + live.View())

	done := live
	done.state = stateDone
	done.phase = phaseComplete
	done.readout = 623.4
	done.result = Result{
		Median: 623.4, Trimmed: 622.9, Min: 291.2, Max: 900.2,
		CV: 0.4, Stable: true, Bytes: 407800000, Duration: 12 * time.Second,
		Samples: 96, Streams: 6,
	}
	t.Log("\n" + done.View())
}
