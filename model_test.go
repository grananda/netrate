package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	m.store = historyStore{path: filepath.Join(t.TempDir(), "netrate", "history.json")}
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

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	keyCtrlC = tea.KeyMsg{Type: tea.KeyCtrlC}
)

// press feeds a key and also hands back the command it produced.
func press(t *testing.T, m model, key tea.KeyMsg) (model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(key)
	got, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	return got, cmd
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func done(m model, r Result) engineMsg { return engineMsg{epoch: m.epoch, ev: DoneEvent{Result: r}} }

var dateRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}`)

const dateLayout = "2006-01-02 15:04"

// recordLines are the lines of the view that hold a history row.
func recordLines(view string) []string {
	var out []string
	for _, l := range strings.Split(view, "\n") {
		if dateRE.MatchString(l) {
			out = append(out, l)
		}
	}
	return out
}

// seedDays seeds n records, one per day in the past, the median of record i
// being 100+i*10 (so record 0 is the oldest and the highest index the newest).
func seedDays(t *testing.T, s historyStore, n int) []runRecord {
	t.Helper()
	var recs []runRecord
	for i := 0; i < n; i++ {
		rec := testRecord(time.Now().Add(-time.Duration(n-i)*24*time.Hour), float64(100+i*10))
		rec.CVPercent = float64(i) + 0.5
		rec.Stable = i%2 == 0
		recs = append(recs, rec)
	}
	seedRecords(t, s, recs...)
	return recs
}

func lineCount(view, sub string) int {
	n := 0
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// AC-1
func TestCompletedRunIsSaved(t *testing.T) {
	cases := map[string]Result{
		"stable":   {Median: 321.5, Trimmed: 320, Min: 200, Max: 400, CV: 1.2, Stable: true, Bytes: 5000, Duration: 4 * time.Second, Samples: 20, Streams: 3},
		"unstable": {Median: 150, CV: 30, Duration: time.Second},
		"early":    {Median: 700, CV: 0.4, Stable: true, EndedEarly: true, Duration: 2 * time.Second},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			m := newTestModel(t)
			if fileExists(m.store.path) {
				t.Fatal("history file exists before the run")
			}
			before := time.Now().Truncate(time.Second)
			m = update(t, m, done(m, r))
			after := time.Now()

			if m.state != stateDone {
				t.Fatalf("state = %v, want stateDone", m.state)
			}
			doc := readDoc(t, m.store.path)
			if len(doc.Measurements) != 1 {
				t.Fatalf("file has %d records, want 1", len(doc.Measurements))
			}
			rec := doc.Measurements[0]
			if rec.MedianMbps != r.Median || rec.CVPercent != r.CV || rec.Stable != r.Stable || rec.EndedEarly != r.EndedEarly {
				t.Fatalf("record %+v does not match result %+v", rec, r)
			}
			if rec.At.Before(before) || rec.At.After(after) {
				t.Fatalf("at = %v, want between %v and %v", rec.At, before, after)
			}
		})
	}
}

// AC-2
func TestEachCompletedRunAddsARecord(t *testing.T) {
	m := newTestModel(t)
	seedDays(t, m.store, 2)

	m = update(t, m, done(m, Result{Median: 555}))
	if got := medians(readDoc(t, m.store.path).Measurements); !slices.Equal(got, []float64{100, 110, 555}) {
		t.Fatalf("file has %v, want the new record last", got)
	}

	m, _ = press(t, m, keyRunes("r"))
	t.Cleanup(m.cancel)
	if m.state != stateRunning {
		t.Fatalf("state = %v after r, want a new run", m.state)
	}
	m = update(t, m, done(m, Result{Median: 666}))
	if got := medians(readDoc(t, m.store.path).Measurements); !slices.Equal(got, []float64{100, 110, 555, 666}) {
		t.Fatalf("file has %v, want the repeated run added", got)
	}
}

// AC-3
func TestFailedOrInterruptedRunIsNotSaved(t *testing.T) {
	cases := map[string]func(*testing.T, model) model{
		"error event": func(t *testing.T, m model) model {
			return update(t, m, engineMsg{epoch: m.epoch, ev: ErrEvent{Err: errors.New("boom")}})
		},
		"closed channel while running": func(t *testing.T, m model) model {
			return update(t, m, closedMsg{epoch: m.epoch})
		},
		"q":      func(t *testing.T, m model) model { m, _ = press(t, m, keyRunes("q")); return m },
		"esc":    func(t *testing.T, m model) model { m, _ = press(t, m, keyEsc); return m },
		"ctrl+c": func(t *testing.T, m model) model { m, _ = press(t, m, keyCtrlC); return m },
	}
	for name, act := range cases {
		t.Run(name, func(t *testing.T) {
			m := act(t, newTestModel(t))
			if fileExists(m.store.path) {
				t.Fatal("a history file was written for a run that did not complete")
			}
		})
	}
}

// AC-4
func TestHistoryShowsRecordsFromAPreviousRun(t *testing.T) {
	prev := newTestModel(t)
	recs := seedDays(t, prev.store, 2)
	// Also go through save, as a previous process would have.
	if _, err := prev.store.save(testRecord(time.Now().Add(-time.Hour), 888)); err != nil {
		t.Fatal(err)
	}
	recs = append(recs, testRecord(time.Now().Add(-time.Hour), 888))

	m := newTestModel(t)
	m.store = prev.store
	m, _ = press(t, m, keyRunes("h"))

	view := m.View()
	for _, rec := range recs {
		if !strings.Contains(view, rec.At.Local().Format(dateLayout)) {
			t.Errorf("view lacks the date of %v:\n%s", rec.At, view)
		}
		if !strings.Contains(view, formatMbps(rec.MedianMbps)) {
			t.Errorf("view lacks the median %s:\n%s", formatMbps(rec.MedianMbps), view)
		}
	}
}

// AC-8
func TestHistoryViewInEveryState(t *testing.T) {
	states := map[string]func(*testing.T, model) model{
		"running": func(t *testing.T, m model) model { return m },
		"done":    func(t *testing.T, m model) model { return update(t, m, done(m, Result{Median: 5})) },
		"error": func(t *testing.T, m model) model {
			return update(t, m, engineMsg{epoch: m.epoch, ev: ErrEvent{Err: errors.New("boom")}})
		},
	}
	rowRE := regexp.MustCompile(`CV \d+\.\d% [✓⚠]`)
	durRE := regexp.MustCompile(`\d+\.\ds`)

	for name, setup := range states {
		t.Run(name, func(t *testing.T) {
			m := newTestModel(t)
			recs := seedDays(t, m.store, 3)
			m = setup(t, m)
			if name == "done" {
				// The run just saved itself; reset to exactly the seeded records.
				seedRecords(t, m.store, recs...)
			}
			m, _ = press(t, m, keyRunes("h"))
			if !m.historyOpen {
				t.Fatal("historyOpen is false after h")
			}

			lines := recordLines(m.View())
			if len(lines) != 3 {
				t.Fatalf("got %d record lines, want 3:\n%s", len(lines), m.View())
			}
			// Newest first.
			for i, line := range lines {
				rec := recs[len(recs)-1-i]
				if !strings.Contains(line, rec.At.Local().Format(dateLayout)) {
					t.Errorf("line %d = %q, want the date of record %v", i, line, rec.At)
				}
				if !strings.Contains(line, formatMbps(rec.MedianMbps)) {
					t.Errorf("line %d = %q lacks the median", i, line)
				}
				if !rowRE.MatchString(line) || !durRE.MatchString(line) {
					t.Errorf("line %d = %q lacks CV with its mark or the duration", i, line)
				}
				wantMark := "⚠"
				if rec.Stable {
					wantMark = "✓"
				}
				if !strings.Contains(line, wantMark) {
					t.Errorf("line %d = %q lacks %s", i, line, wantMark)
				}
			}
		})
	}
}

// AC-9
func TestHistoryViewWhenEmpty(t *testing.T) {
	for name, prepare := range map[string]func(*testing.T, historyStore){
		"missing file": func(*testing.T, historyStore) {},
		"empty list":   func(t *testing.T, s historyStore) { seedRaw(t, s, `{"version":1,"measurements":[]}`) },
	} {
		t.Run(name, func(t *testing.T) {
			m := newTestModel(t)
			prepare(t, m.store)
			m, _ = press(t, m, keyRunes("h"))
			view := m.View()
			if !strings.Contains(view, "no measurements saved yet") {
				t.Errorf("view lacks the empty message:\n%s", view)
			}
			if strings.Contains(view, "could not be read") {
				t.Errorf("view reports an error for an empty history:\n%s", view)
			}
		})
	}
}

// AC-10
func TestHistoryViewUpdatesWhenRunFinishes(t *testing.T) {
	m := newTestModel(t)
	seedRecords(t, m.store, testRecord(time.Now().Add(-48*time.Hour), 111))

	m, _ = press(t, m, keyRunes("h"))
	m = update(t, m, engineMsg{epoch: m.epoch, ev: SampleEvent{Sample: Sample{Mbps: 50, Warm: true}}})
	if m.ctx.Err() != nil {
		t.Fatal("opening the history cancelled the measurement")
	}
	m = update(t, m, done(m, Result{Median: 777}))

	if !m.historyOpen {
		t.Fatal("the view closed itself when the run finished")
	}
	if m.state != stateDone {
		t.Fatalf("state = %v, want stateDone", m.state)
	}
	lines := recordLines(m.View())
	if len(lines) != 2 {
		t.Fatalf("got %d record lines, want 2:\n%s", len(lines), m.View())
	}
	if !strings.Contains(lines[0], formatMbps(777)) {
		t.Fatalf("first record line = %q, want the new run (%s)", lines[0], formatMbps(777))
	}
}

// AC-11
func TestHistoryViewFitsEveryWidth(t *testing.T) {
	for w := minViewWidth + boxChrome; w <= maxViewWidth+boxChrome; w++ {
		m := newTestModel(t)
		var recs []runRecord
		for i := 0; i < 10; i++ {
			rec := testRecord(time.Now().Add(-time.Duration(10-i)*24*time.Hour), 1234.5)
			rec.CVPercent, rec.DurationSeconds, rec.Stable = 123.4, 300.0, i%2 == 0
			recs = append(recs, rec)
		}
		seedRecords(t, m.store, recs...)
		m = update(t, m, tea.WindowSizeMsg{Width: w, Height: 40})
		m, _ = press(t, m, keyRunes("h"))

		view := m.View()
		limit := m.viewWidth() + boxChrome
		for _, l := range strings.Split(view, "\n") {
			if got := lipgloss.Width(l); got > limit {
				t.Fatalf("width %d: line is %d columns, limit %d: %q", w, got, limit, l)
			}
		}
		lines := recordLines(view)
		if len(lines) != 10 {
			t.Fatalf("width %d: %d record lines, want 10", w, len(lines))
		}
		for _, l := range lines {
			if !strings.Contains(l, formatMbps(1234.5)) {
				t.Fatalf("width %d: line %q lacks the median", w, l)
			}
			switch m.viewWidth() {
			case minViewWidth:
				if strings.Contains(l, "300.0s") {
					t.Fatalf("width %d: minimum width still shows the duration: %q", w, l)
				}
			case maxViewWidth:
				if !strings.Contains(l, "300.0s") || !strings.Contains(l, "CV 123.4%") {
					t.Fatalf("width %d: maximum width lacks a column: %q", w, l)
				}
			}
		}
	}
}

// AC-12
func TestHistoryViewGoesBack(t *testing.T) {
	for name, key := range map[string]tea.KeyMsg{"h": keyRunes("h"), "esc": keyEsc} {
		t.Run(name+" while running", func(t *testing.T) {
			m := newTestModel(t)
			epoch := m.epoch
			m, _ = press(t, m, keyRunes("h"))
			m = update(t, m, engineMsg{epoch: m.epoch, ev: SampleEvent{Sample: Sample{Mbps: 10, Bytes: 4242, Warm: true}}})
			m, cmd := press(t, m, key)

			if m.historyOpen {
				t.Fatal("history still open")
			}
			if cmd != nil {
				t.Fatalf("closing the view returned a command (quit?)")
			}
			if m.epoch != epoch || m.state != stateRunning || m.ctx.Err() != nil {
				t.Fatalf("run disturbed: epoch=%d state=%v ctx=%v", m.epoch, m.state, m.ctx.Err())
			}
			if m.bytes != 4242 {
				t.Fatalf("bytes = %d, the sample received while open was lost", m.bytes)
			}
		})
		t.Run(name+" when done", func(t *testing.T) {
			m := newTestModel(t)
			m = update(t, m, done(m, Result{Median: 321}))
			m, _ = press(t, m, keyRunes("h"))
			m, cmd := press(t, m, key)
			if m.historyOpen || cmd != nil || m.state != stateDone || m.result.Median != 321 {
				t.Fatalf("open=%v cmd=%v state=%v result=%+v", m.historyOpen, cmd != nil, m.state, m.result)
			}
		})
	}

	t.Run("esc outside the view still quits", func(t *testing.T) {
		m := newTestModel(t)
		_, cmd := press(t, m, keyEsc)
		if !isQuit(cmd) {
			t.Fatal("esc on the measurement screen did not quit")
		}
	})
}

// AC-13
func TestHistoryViewQuitAndRunAgainKeys(t *testing.T) {
	for name, key := range map[string]tea.KeyMsg{"q": keyRunes("q"), "ctrl+c": keyCtrlC} {
		t.Run(name, func(t *testing.T) {
			m := newTestModel(t)
			m, _ = press(t, m, keyRunes("h"))
			m, cmd := press(t, m, key)
			if !isQuit(cmd) {
				t.Fatal("did not quit")
			}
			if m.ctx.Err() == nil {
				t.Fatal("the engine was not cancelled")
			}
		})
	}

	t.Run("r does nothing", func(t *testing.T) {
		m := newTestModel(t)
		m = update(t, m, done(m, Result{Median: 9}))
		m, _ = press(t, m, keyRunes("h"))
		epoch := m.epoch
		m, cmd := press(t, m, keyRunes("r"))
		if m.epoch != epoch || !m.historyOpen || m.state != stateDone || cmd != nil {
			t.Fatalf("epoch=%d open=%v state=%v cmd=%v", m.epoch, m.historyOpen, m.state, cmd != nil)
		}
	})
}

// AC-14
func TestCorruptHistoryFileDoesNotBreakTheRun(t *testing.T) {
	m := newTestModel(t)
	seedRaw(t, m.store, "not json")

	m, _ = press(t, m, keyRunes("h"))
	view := m.View()
	if !strings.Contains(view, "history file could not be read") {
		t.Errorf("view lacks the notice:\n%s", view)
	}
	if n := len(recordLines(view)); n != 0 {
		t.Errorf("%d record lines shown for a corrupt file", n)
	}

	m = update(t, m, done(m, Result{Median: 250}))
	if m.state != stateDone || m.saveErr != nil {
		t.Fatalf("state=%v saveErr=%v, want a normal finish", m.state, m.saveErr)
	}
	doc := readDoc(t, m.store.path)
	if len(doc.Measurements) != 1 || doc.Measurements[0].MedianMbps != 250 {
		t.Fatalf("file = %+v, want only the new measurement", doc)
	}
}

// AC-15
func TestUnsavableHistoryWarns(t *testing.T) {
	stores := map[string]func(*testing.T) historyStore{
		"regular file as parent": blockedStore,
		"no config directory": func(*testing.T) historyStore {
			return historyStore{err: errors.New("no user config directory")}
		},
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) {
			m := newTestModel(t)
			m.store = mk(t)
			m = update(t, m, tea.WindowSizeMsg{Width: minViewWidth + boxChrome, Height: 40})
			m = update(t, m, done(m, Result{Median: 400, Stable: true}))

			if m.state != stateDone || m.err != nil {
				t.Fatalf("state=%v err=%v, want a normal finish", m.state, m.err)
			}
			view := m.View()
			if !strings.Contains(view, "p50") {
				t.Errorf("the result is no longer shown:\n%s", view)
			}
			if n := lineCount(view, "history not saved"); n != 1 {
				t.Errorf("%d lines mention history not saved, want exactly 1:\n%s", n, view)
			}
			limit := m.viewWidth() + boxChrome
			for _, l := range strings.Split(view, "\n") {
				if lipgloss.Width(l) > limit {
					t.Errorf("line wider than %d: %q", limit, l)
				}
			}
		})
	}
}

// AC-18
func TestHelpLine(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		v := newTestModel(t).View()
		if !strings.Contains(v, "h history  ·  q quit") || strings.Contains(v, "r run again") {
			t.Fatalf("running help wrong:\n%s", v)
		}
	})
	t.Run("done", func(t *testing.T) {
		m := newTestModel(t)
		v := update(t, m, done(m, Result{Median: 1})).View()
		if !strings.Contains(v, "r run again  ·  h history  ·  q quit") {
			t.Fatalf("done help wrong:\n%s", v)
		}
	})
	t.Run("error", func(t *testing.T) {
		m := newTestModel(t)
		v := update(t, m, engineMsg{epoch: m.epoch, ev: ErrEvent{Err: errors.New("boom")}}).View()
		if !strings.Contains(v, "r run again  ·  h history  ·  q quit") {
			t.Fatalf("error help wrong:\n%s", v)
		}
	})
	t.Run("history view", func(t *testing.T) {
		m, _ := press(t, newTestModel(t), keyRunes("h"))
		if v := m.View(); !strings.Contains(v, "h/esc back  ·  q quit") {
			t.Fatalf("history help wrong:\n%s", v)
		}
	})
}

// warningLine renders a finished run against the store at the narrowest width
// and returns the single history-not-saved line.
func warningLine(t *testing.T, store historyStore) string {
	t.Helper()
	m := newTestModel(t)
	m.store = store
	m = update(t, m, tea.WindowSizeMsg{Width: minViewWidth + boxChrome, Height: 40})
	m = update(t, m, done(m, Result{Median: 400, Stable: true}))
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, "history not saved") {
			return l
		}
	}
	t.Fatalf("no warning line:\n%s", m.View())
	return ""
}

// AC-15
func TestSaveWarningKeepsItsReason(t *testing.T) {
	t.Run("default store without a config directory", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "")
		store := defaultHistoryStore()
		if !errors.Is(store.location(), errNoConfigDir) {
			t.Skipf("this platform still resolves a config directory: %+v", store)
		}
		if l := warningLine(t, store); !strings.Contains(l, "no config directory") {
			t.Errorf("warning lost its reason: %q", l)
		}
	})
	t.Run("rename onto a directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "netrate", "history.json")
		if err := os.MkdirAll(filepath.Join(path, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		l := warningLine(t, historyStore{path: path})
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "⚠ history not saved:")) == "" ||
			!strings.Contains(l, "history not saved: ") {
			t.Errorf("warning has no reason: %q", l)
		}
	})
}

// AC-15: the common OS reasons fit the narrowest view in full.
func TestSaveWarningShowsOSReasonsInFull(t *testing.T) {
	const longPath = "/some/long/config/dir/netrate/history.json"
	wrap := func(errno syscall.Errno) error {
		return fmt.Errorf("save history: %w", &fs.PathError{Op: "open", Path: longPath, Err: errno})
	}
	tests := []struct {
		name  string
		err   error
		posix bool // the errno is a POSIX one, not meaningful on Windows
	}{
		{"read-only file system", wrap(syscall.EROFS), true},
		{"no space left", wrap(syscall.ENOSPC), true},
		{"operation not permitted", wrap(syscall.EPERM), false},
		{"access denied", wrap(syscall.EACCES), false},
		{"missing directory", wrap(syscall.ENOENT), false},
		{"not exist sentinel", fmt.Errorf("save history: %w", &fs.PathError{Op: "open", Path: longPath, Err: fs.ErrNotExist}), false},
		{"no config dir", fmt.Errorf("save history: %w", errNoConfigDir), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.posix && runtime.GOOS == "windows" {
				t.Skip("POSIX errno")
			}
			m := newTestModel(t)
			m = update(t, m, tea.WindowSizeMsg{Width: minViewWidth + boxChrome, Height: 40})
			m = update(t, m, done(m, Result{Median: 400, Stable: true}))
			m.saveErr = tt.err
			var lines []string
			for _, l := range strings.Split(m.View(), "\n") {
				if strings.Contains(l, "history not saved") {
					lines = append(lines, l)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("want exactly one warning line, got %d: %q", len(lines), lines)
			}
			line := lines[0]
			reason := saveFailureReason(tt.err)
			if reason == "" {
				t.Fatal("reason is empty")
			}
			if !strings.Contains(line, "history not saved") {
				t.Errorf("warning does not say the history was not saved: %q", line)
			}
			if !strings.Contains(line, reason) {
				t.Errorf("reason %q cut or missing: %q", reason, line)
			}
			if strings.Contains(line, longPath) || strings.Contains(line, "/") {
				t.Errorf("warning leaks the path: %q", line)
			}
			if w := lipgloss.Width(line); w > minViewWidth+boxChrome {
				t.Errorf("warning is %d columns, limit %d", w, minViewWidth+boxChrome)
			}
		})
	}
}

// chartMedians are spread over every colour of the ramp and over every unit.
var chartMedians = []float64{0.5, 9.99, 55, 99.99, 140, 600, 870, 1234.5, 2500, 12345}

// checkHistoryChart checks the open history view of m, which is expected to
// hold the 10 records recs (oldest first).
func checkHistoryChart(t *testing.T, m model, recs []runRecord, label string) (bars int) {
	t.Helper()
	view := m.View()
	limit := m.viewWidth() + boxChrome
	for _, l := range strings.Split(view, "\n") {
		if got := lipgloss.Width(l); got > limit {
			t.Fatalf("%s: line is %d columns, limit %d: %q", label, got, limit, l)
		}
	}
	lines := recordLines(view)
	if len(lines) != len(recs) {
		t.Fatalf("%s: %d record lines, want %d", label, len(lines), len(recs))
	}
	for i, l := range lines {
		rec := recs[len(recs)-1-i]
		plain := stripANSI(l)
		if !strings.Contains(plain, rec.At.Local().Format(dateLayout)) {
			t.Fatalf("%s: line %d lacks its date: %q", label, i, plain)
		}
		if !strings.Contains(plain, formatMbps(rec.MedianMbps)) {
			t.Fatalf("%s: line %d lacks its median: %q", label, i, plain)
		}
		if hasBarGlyphs(plain) {
			bars++
		}
	}
	if bars != 0 && bars != len(lines) {
		t.Fatalf("%s: %d of %d lines have a bar, want all or none", label, bars, len(lines))
	}
	return bars
}

// AC-5, AC-6, AC-13, AC-15
func TestHistoryChartFitsEveryWidth(t *testing.T) {
	for w := minViewWidth + boxChrome; w <= maxViewWidth+boxChrome; w++ {
		m := newTestModel(t)
		var recs []runRecord
		for i, median := range chartMedians {
			rec := testRecord(time.Now().Add(-time.Duration(10-i)*24*time.Hour), median)
			rec.CVPercent, rec.DurationSeconds, rec.Stable = 123.4, 300.0, i%2 == 0
			if i == 5 {
				rec.MaxMbps = median * 5
			}
			recs = append(recs, rec)
		}
		seedRecords(t, m.store, recs...)
		m = update(t, m, tea.WindowSizeMsg{Width: w, Height: 40})
		m, _ = press(t, m, keyRunes("h"))

		bars := checkHistoryChart(t, m, recs, fmt.Sprintf("width %d", w))
		switch m.viewWidth() {
		case maxViewWidth:
			if bars == 0 {
				t.Fatalf("width %d: no chart at the maximum width", w)
			}
		case minViewWidth:
			if bars != 0 {
				t.Fatalf("width %d: chart at the minimum width", w)
			}
		}

		// AC-15: resize while the view is open.
		other := (minViewWidth + boxChrome + maxViewWidth + boxChrome) - w
		m = update(t, m, tea.WindowSizeMsg{Width: other, Height: 40})
		if !m.historyOpen {
			t.Fatalf("width %d: the resize closed the history", w)
		}
		checkHistoryChart(t, m, recs, fmt.Sprintf("width %d resized to %d", w, other))
	}
}

// AC-17, AC-8
func TestHistoryChartRescalesOnNewRun(t *testing.T) {
	m := newTestModel(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 106, Height: 40})
	recs := seedDays(t, m.store, 10)
	m, _ = press(t, m, keyRunes("h"))
	m = update(t, m, done(m, Result{Median: 500, Min: 500, Max: 500, CV: 1, Stable: true, Duration: 5500 * time.Millisecond}))

	if !m.historyOpen {
		t.Fatal("the view closed itself when the run finished")
	}
	lines := recordLines(m.View())
	if len(lines) != 10 {
		t.Fatalf("got %d record lines, want 10:\n%s", len(lines), m.View())
	}
	first := stripANSI(lines[0])
	if !strings.Contains(first, formatMbps(500)) {
		t.Fatalf("first line = %q, want the new run", first)
	}
	// 100 columns: date 16, median 10, "CV x.x% m" 9, duration 4, 3 gaps of 2.
	const n = 100 - (16 + 10 + 9 + 4 + 6) - 2
	if !strings.Contains(first, strings.Repeat("▓", n)) {
		t.Errorf("the new fastest run does not fill the chart with ▓ (%d cells): %q", n, first)
	}
	rec190 := recs[9]
	var line190 string
	for _, l := range lines {
		if strings.Contains(l, rec190.At.Local().Format(dateLayout)) {
			line190 = stripANSI(l)
		}
	}
	if line190 == "" {
		t.Fatalf("the 190 run is not in the view:\n%s", m.View())
	}
	if want := stripANSI(renderBar(rec190, 500, n)); !strings.Contains(line190, want) {
		t.Errorf("the 190 line = %q, want the bar rescaled to 500: %q", line190, want)
	}
	for _, l := range lines {
		if strings.Contains(l, recs[0].At.Local().Format(dateLayout)) {
			t.Errorf("the oldest run is still shown: %q", l)
		}
	}
}

// AC-18
func TestHistoryViewWithoutRecordsHasNoChart(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T, historyStore)
		want    string
	}{
		{"missing file", func(*testing.T, historyStore) {}, "no measurements saved yet"},
		{"empty list", func(t *testing.T, s historyStore) { seedRaw(t, s, `{"version":1,"measurements":[]}`) }, "no measurements saved yet"},
		{"not json", func(t *testing.T, s historyStore) { seedRaw(t, s, "not json") }, "history file could not be read"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t)
			tt.prepare(t, m.store)
			m = update(t, m, tea.WindowSizeMsg{Width: 106, Height: 40})
			m, _ = press(t, m, keyRunes("h"))
			view := stripANSI(m.View())
			if !strings.Contains(view, tt.want) {
				t.Errorf("view lacks %q:\n%s", tt.want, view)
			}
			if hasBarGlyphs(view) {
				t.Errorf("view has bar glyphs:\n%s", view)
			}
		})
	}
}

// blankLine reports whether a view line holds nothing but the box border.
func blankLine(l string) bool {
	return strings.TrimSpace(strings.ReplaceAll(stripANSI(l), "│", "")) == ""
}

func viewLines(view string) []string { return strings.Split(view, "\n") }

// recordIndexes are the positions of the record lines in the view.
func recordIndexes(view string) []int {
	var out []int
	for i, l := range viewLines(view) {
		if dateRE.MatchString(l) {
			out = append(out, i)
		}
	}
	return out
}

// requireSpacing checks that the 10 (or n) record lines are separated by
// exactly one blank line (spaced) or by nothing (compact).
func requireSpacing(t *testing.T, view string, spaced bool, label string) {
	t.Helper()
	idx := recordIndexes(view)
	lines := viewLines(view)
	for i := 1; i < len(idx); i++ {
		gap := idx[i] - idx[i-1]
		if !spaced {
			if gap != 1 {
				t.Errorf("%s: record lines %d and %d are %d lines apart, want consecutive", label, i-1, i, gap)
			}
			continue
		}
		if gap != 2 || !blankLine(lines[idx[i-1]+1]) {
			t.Errorf("%s: record lines %d and %d are %d lines apart (blank between: %v), want exactly one blank line", label, i-1, i, gap, gap == 2)
		}
	}
}

// AC-20, AC-8, AC-15
func TestHistoryViewSpacesChartRows(t *testing.T) {
	open := func(t *testing.T, days, width, height int) (model, []runRecord) {
		t.Helper()
		m := newTestModel(t)
		recs := seedDays(t, m.store, days)
		m = update(t, m, tea.WindowSizeMsg{Width: width, Height: height})
		m, _ = press(t, m, keyRunes("h"))
		return m, recs
	}
	lineTotal := func(view string) int { return strings.Count(view, "\n") + 1 }

	t.Run("AC-20 a tall terminal separates the rows with one blank line", func(t *testing.T) {
		m, recs := open(t, 10, 106, 40)
		view := m.View()
		idx := recordIndexes(view)
		if len(idx) != 10 {
			t.Fatalf("%d record lines, want 10:\n%s", len(idx), view)
		}
		lines := viewLines(view)
		for i, ix := range idx { // AC-8: newest first
			rec := recs[len(recs)-1-i]
			if !strings.Contains(stripANSI(lines[ix]), rec.At.Local().Format(dateLayout)) {
				t.Errorf("record line %d = %q, want the run of %s", i, lines[ix], rec.At.Local().Format(dateLayout))
			}
		}
		requireSpacing(t, view, true, "height 40")
		if n := lineTotal(view); n > 40 {
			t.Errorf("the screen has %d lines, over the height 40", n)
		}
	})

	t.Run("AC-20 a resize re-decides the spacing", func(t *testing.T) {
		m, _ := open(t, 10, 106, 40)
		n := lineTotal(m.View())
		m = update(t, m, tea.WindowSizeMsg{Width: 106, Height: n})
		requireSpacing(t, m.View(), true, fmt.Sprintf("height %d (just fits)", n))
		if got := lineTotal(m.View()); got != n {
			t.Errorf("spaced screen is %d lines at height %d, was %d", got, n, n)
		}
		m = update(t, m, tea.WindowSizeMsg{Width: 106, Height: n - 1})
		view := m.View()
		requireSpacing(t, view, false, fmt.Sprintf("height %d (one short)", n-1))
		if got := lineTotal(view); got > n-1 {
			t.Errorf("compact screen is %d lines, over the height %d", got, n-1)
		}
		m = update(t, m, tea.WindowSizeMsg{Width: 106, Height: 40})
		requireSpacing(t, m.View(), true, "height back to 40")
	})

	t.Run("AC-20 a short terminal keeps 10 records compact, 5 spaced", func(t *testing.T) {
		m, _ := open(t, 10, 106, 24)
		view := m.View()
		if got := len(recordIndexes(view)); got != 10 {
			t.Fatalf("%d record lines, want 10", got)
		}
		requireSpacing(t, view, false, "10 records at height 24")
		if n := lineTotal(view); n > 24 {
			t.Errorf("compact screen is %d lines, over the height 24", n)
		}

		m, _ = open(t, 5, 106, 24)
		view = m.View()
		if got := len(recordIndexes(view)); got != 5 {
			t.Fatalf("%d record lines, want 5", got)
		}
		requireSpacing(t, view, true, "5 records at height 24")
		if n := lineTotal(view); n > 24 {
			t.Errorf("spaced screen is %d lines, over the height 24", n)
		}
	})

	t.Run("AC-20 an unknown height counts as fitting", func(t *testing.T) {
		m, _ := open(t, 10, 106, 0)
		requireSpacing(t, m.View(), true, "height 0")
	})

	t.Run("AC-20 AC-15 text-only rows are never spaced", func(t *testing.T) {
		m, recs := open(t, 10, 60, 40)
		if got := m.viewWidth(); got != 54 {
			t.Fatalf("viewWidth = %d, want 54", got)
		}
		view := m.View()
		requireSpacing(t, view, false, "width 60")
		if hasBarGlyphs(stripANSI(view)) {
			t.Errorf("a bar is drawn without room for the chart:\n%s", view)
		}
		want := plainRows(historyRows(recs, 54))
		lines := viewLines(view)
		idx := recordIndexes(view)
		if len(idx) != len(want) {
			t.Fatalf("%d record lines, want %d", len(idx), len(want))
		}
		for i, ix := range idx {
			if got := stripANSI(lines[ix]); !strings.Contains(got, strings.TrimSpace(want[i])) {
				t.Errorf("record line %d = %q, want today's text row %q", i, got, want[i])
			}
		}
	})

	t.Run("AC-20 AC-8 a finished run keeps the spacing, r does nothing", func(t *testing.T) {
		m, _ := open(t, 10, 106, 40)
		m = update(t, m, done(m, Result{Median: 500, Min: 500, Max: 500, CV: 1, Stable: true, Duration: 5500 * time.Millisecond}))
		if !m.historyOpen {
			t.Fatal("the view closed itself when the run finished")
		}
		view := m.View()
		idx := recordIndexes(view)
		if len(idx) != 10 {
			t.Fatalf("%d record lines, want 10:\n%s", len(idx), view)
		}
		if first := stripANSI(viewLines(view)[idx[0]]); !strings.Contains(first, formatMbps(500)) {
			t.Errorf("first record line = %q, want the new run", first)
		}
		requireSpacing(t, view, true, "after the run")
		if n := lineTotal(view); n > 40 {
			t.Errorf("the screen has %d lines, over the height 40", n)
		}

		epoch := m.epoch
		m, cmd := press(t, m, keyRunes("r"))
		if m.epoch != epoch || !m.historyOpen || cmd != nil {
			t.Fatalf("r changed something: epoch=%d open=%v cmd=%v", m.epoch, m.historyOpen, cmd != nil)
		}
		requireSpacing(t, m.View(), true, "after r")
	})
}
