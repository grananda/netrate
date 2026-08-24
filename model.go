package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type state int

const (
	stateRunning state = iota
	stateDone
	stateError
)

// appName is what the interface calls itself. Lower case on purpose: it is the
// command the reader typed, not a product splash.
const appName = "netrate"

const (
	// readoutEase smooths the headline number towards each new sample. The
	// engine reports every cfg.Interval — ten times a second by default — and
	// an unsmoothed figure flickers through digits too fast to read.
	readoutEase = 0.35

	// maxHistory caps the series behind the chart. A long -duration would
	// otherwise grow it without bound.
	maxHistory = 8192

	// boxChrome is the columns the border and padding take from the terminal.
	boxChrome = 6
)

var (
	titleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#67e8f9")).Bold(true)
	valueStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f8fafc")).Bold(true)
	unitStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8"))
	phaseStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#f0abfc"))
	statStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8"))
	strongStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#e2e8f0")).Bold(true)
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#4ade80"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#fbbf24"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#fb7185"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b"))
	boxStyle    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#334155")).
			Padding(0, 2)
)

// Messages carried between the engine goroutine and the UI.
//
// Both carry the epoch of the run that produced them. Pressing "r" starts a new
// engine while the previous run's pending channel read is still in flight;
// without the epoch that stale read lands on the fresh model and flags it as
// failed.
type (
	engineMsg struct {
		epoch int
		ev    Event
	}
	closedMsg struct{ epoch int }
)

type model struct {
	cfg   Config
	state state
	phase string

	// epoch identifies this run. It only ever increases, via restart.
	epoch int

	// width is the usable width inside the box, learnt from the terminal.
	width int

	// The engine's lifetime is the model's lifetime, so the context and its
	// event channel are built with the model and never reassigned. Init has
	// a value receiver and could not set them anyway.
	ctx    context.Context
	cancel context.CancelFunc
	events chan Event

	readout float64 // eased headline value, the only reading the view shows
	peak    float64
	warm    bool
	elapsed time.Duration
	bytes   int64
	history []float64

	result Result
	err    error
}

func newModel(cfg Config) model {
	ctx, cancel := context.WithCancel(context.Background())
	return model{
		cfg:    cfg,
		state:  stateRunning,
		phase:  phaseStarting,
		ctx:    ctx,
		cancel: cancel,
		events: make(chan Event, 256),
	}
}

// Init launches the engine and starts draining its events.
func (m model) Init() tea.Cmd {
	go Run(m.ctx, m.cfg, m.events)
	return listen(m.events, m.epoch)
}

// restart tears the current engine down and hands back a fresh model with the
// next epoch, so anything still in flight from this run is ignored.
func (m model) restart() (model, tea.Cmd) {
	m.cancel()
	next := newModel(m.cfg)
	next.epoch = m.epoch + 1
	next.width = m.width
	return next, next.Init()
}

func listen(ch <-chan Event, epoch int) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return closedMsg{epoch: epoch}
		}
		return engineMsg{epoch: epoch, ev: ev}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancel()
			return m, tea.Quit
		case "r":
			if m.state != stateRunning {
				return m.restart()
			}
		}

	case tea.WindowSizeMsg:
		m.width = clampViewWidth(msg.Width - boxChrome)
		return m, nil

	case engineMsg:
		if msg.epoch != m.epoch {
			return m, nil
		}
		m = m.applyEvent(msg.ev)
		return m, listen(m.events, m.epoch)

	case closedMsg:
		if msg.epoch != m.epoch {
			return m, nil
		}
		// The engine closed the channel without a verdict: either it was
		// cancelled, or it died in a way it could not report.
		if m.state == stateRunning {
			m.state = stateError
			m.phase = phaseFailed
			if m.err == nil {
				m.err = errNoUsableSamples
			}
		}
		return m, nil
	}

	return m, nil
}

func (m model) applyEvent(ev Event) model {
	switch ev := ev.(type) {
	case PhaseEvent:
		m.phase = ev.Phase

	case SampleEvent:
		s := ev.Sample
		m.readout += (s.Mbps - m.readout) * readoutEase
		m.elapsed = s.At
		m.bytes = s.Bytes
		m.warm = s.Warm
		if s.Warm && s.Mbps > m.peak {
			m.peak = s.Mbps
		}
		m.history = appendCapped(m.history, s.Mbps, maxHistory)

	case DoneEvent:
		m.result = ev.Result
		m.readout = ev.Result.Median // the verdict, not wherever the easing got to
		m.state = stateDone
		m.phase = phaseComplete

	case ErrEvent:
		m.err = ev.Err
		m.state = stateError
		m.phase = phaseFailed
	}
	return m
}

// appendCapped appends v, dropping the oldest entries once the series would
// outgrow limit.
func appendCapped(series []float64, v float64, limit int) []float64 {
	series = append(series, v)
	if len(series) > limit {
		series = append(series[:0], series[len(series)-limit:]...)
	}
	return series
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m model) View() string {
	width := m.viewWidth()

	var b strings.Builder
	b.WriteString(spread(width, titleStyle.Render(appName), m.statusLine()))
	b.WriteString("\n\n")
	b.WriteString(spread(width, m.headline(), m.headlineDetail()))
	b.WriteString("\n\n")

	for _, row := range renderChart(m.history, m.cfg, m.chartSpan(), width) {
		b.WriteString(row + "\n")
	}

	b.WriteString("\n")
	b.WriteString(m.stats(width))

	if m.state == stateError {
		b.WriteString("\n" + errStyle.Render("✗ "+m.errorText()))
	}

	help := "q quit"
	if m.state != stateRunning {
		help = "r run again  ·  q quit"
	}
	return boxStyle.Render(b.String()) + "\n" + helpStyle.Render("  "+help) + "\n"
}

// chartSpan is what the x axis should cover. A live run is plotted against its
// duration budget so the chart shows progress; a finished one against the time
// it actually took, so a run that settled early fills the plot instead of
// trailing off into empty space.
func (m model) chartSpan() time.Duration {
	if m.state == stateDone && m.result.Duration > 0 {
		return m.result.Duration
	}
	return m.cfg.Duration
}

// viewWidth is the usable width inside the box: whatever the terminal reported,
// or a sensible default until it does.
func (m model) viewWidth() int {
	if m.width == 0 {
		return defaultViewWidth
	}
	return m.width
}

// headline is the number the whole screen is about: live while measuring, the
// median once the run has a verdict.
func (m model) headline() string {
	value, unit := mbpsParts(m.readout)
	return valueStyle.Render(value) + " " + unitStyle.Render(unit)
}

func (m model) headlineDetail() string {
	switch m.state {
	case stateDone:
		return statStyle.Render(fmt.Sprintf("median of %d samples · CV %.1f%%",
			m.result.Samples, m.result.CV))
	case stateError:
		return statStyle.Render("no result")
	default:
		if !m.warm && m.elapsed > 0 {
			return warnStyle.Render("warming up · discarded")
		}
		return statStyle.Render("peak " + formatMbps(m.peak))
	}
}

func (m model) statusLine() string {
	switch m.state {
	case stateDone:
		if m.result.Stable {
			return okStyle.Render("✓ stable")
		}
		return warnStyle.Render("⚠ unstable")
	case stateError:
		return errStyle.Render("✗ failed")
	default:
		return phaseStyle.Render(m.phase)
	}
}

// errorText never dereferences a nil error, so a future state transition that
// forgets to set one degrades into a vague message instead of a panic.
func (m model) errorText() string {
	if m.err == nil {
		return "the measurement failed"
	}
	return m.err.Error()
}

func (m model) stats(width int) string {
	field := func(key, value string) string {
		return statStyle.Render(key+" ") + strongStyle.Render(value)
	}

	if m.state != stateDone {
		return strings.Join([]string{
			spread(width,
				field("elapsed", fmt.Sprintf("%.1fs / %.1fs", m.elapsed.Seconds(), m.cfg.Duration.Seconds())),
				field("streams", fmt.Sprint(m.cfg.Streams)),
			),
			spread(width,
				field("downloaded", formatBytes(m.bytes)),
				// peak is already beside the headline; showing it twice wastes
				// the only other line the running view has.
				field("samples", fmt.Sprint(len(m.history))),
			),
		}, "\n")
	}

	r := m.result
	rows := []string{
		spread(width,
			field("p50", formatMbps(r.Median)),
			field("trimmed", formatMbps(r.Trimmed)),
		),
		spread(width,
			field("min", formatMbps(r.Min)),
			field("max", formatMbps(r.Max)),
		),
		spread(width,
			field("downloaded", formatBytes(r.Bytes)),
			field("duration", fmt.Sprintf("%.1fs", r.Duration.Seconds())),
		),
		spread(width,
			field("streams", fmt.Sprint(r.Streams)),
			field("CV", fmt.Sprintf("%.1f%%", r.CV)),
		),
	}

	// Without this the run just looks short of the duration that was asked for.
	if r.EndedEarly {
		rows = append(rows, statStyle.Render(fmt.Sprintf(
			"settled early · stopped at %.1fs of the %.1fs budget",
			r.Duration.Seconds(), m.cfg.Duration.Seconds())))
	}
	return strings.Join(rows, "\n")
}

// spread pushes two cells to opposite ends of the given width.
func spread(width int, left, right string) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}
