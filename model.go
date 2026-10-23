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

	// height is the terminal height from the last WindowSizeMsg, 0 until known.
	height int

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

	// store is where completed runs are kept. The zero value does no IO.
	store historyStore
	// historyOpen shows the history in place of the measurement. It is separate
	// from state, which drives the engine, "r" and the exit code.
	historyOpen bool
	// pastRuns are the stored measurements, oldest first, as last loaded or saved.
	pastRuns []runRecord
	// pastRunsErr is set when the last load of the history failed.
	pastRunsErr error
	// saveErr is set when this run's record could not be saved.
	saveErr error
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
	next.height = m.height
	next.store = m.store
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
		case "q", "ctrl+c":
			m.cancel()
			return m, tea.Quit
		case "esc":
			// Inside the history, esc goes back; anywhere else it quits.
			if m.historyOpen {
				m.historyOpen = false
				return m, nil
			}
			m.cancel()
			return m, tea.Quit
		case "h":
			if m.historyOpen {
				m.historyOpen = false
			} else {
				m = m.openHistory()
			}
			return m, nil
		case "r":
			if m.historyOpen {
				return m, nil
			}
			if m.state != stateRunning {
				return m.restart()
			}
		}

	case tea.WindowSizeMsg:
		m.width = clampViewWidth(msg.Width - boxChrome)
		m.height = msg.Height
		return m, nil

	case engineMsg:
		if msg.epoch != m.epoch {
			return m, nil
		}
		m = m.applyEvent(msg.ev)
		// Saved here, synchronously: the file is tiny and written once per run,
		// and a pending Cmd could be dropped by an immediate quit.
		if ev, ok := msg.ev.(DoneEvent); ok {
			m = m.recordRun(ev.Result, time.Now())
		}
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

// recordRun saves a completed run. A failure is kept for the view to warn about
// and never changes the run's outcome.
func (m model) recordRun(r Result, at time.Time) model {
	runs, err := m.store.save(newRunRecord(r, m.cfg.URL, at))
	if err != nil {
		m.saveErr = err
		return m
	}
	m.pastRuns, m.pastRunsErr = runs, nil
	return m
}

// openHistory re-reads the file on every open, which picks up runs saved by
// other instances.
func (m model) openHistory() model {
	m.pastRuns, m.pastRunsErr = m.store.load()
	m.historyOpen = true
	return m
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

	if m.historyOpen {
		// Rows get a blank line between them when the chart is shown and the
		// whole screen still fits the terminal. The line count is Bubble Tea's
		// (its renderer drops the top lines of a taller view); an unknown
		// height counts as fitting.
		spaced := m.pastRunsErr == nil && historyChartCells(m.pastRuns, width) > 0
		out := m.historyScreen(width, spaced)
		if spaced && m.height > 0 && strings.Count(out, "\n")+1 > m.height {
			out = m.historyScreen(width, false)
		}
		return out
	}

	var b strings.Builder
	b.WriteString(spread(width, titleStyle.Render(appName), m.statusLine()))
	b.WriteString("\n\n")
	headline, detail := m.headline(), m.headlineDetail()
	if m.state == stateDone && lipgloss.Width(headline)+1+lipgloss.Width(detail) > width {
		// On the narrowest box the full sentence overflows it; keep the part
		// that matters rather than let the border break.
		detail = statStyle.Render(fmt.Sprintf("CV %.1f%%", m.result.CV))
	}
	b.WriteString(spread(width, headline, detail))
	b.WriteString("\n\n")

	for _, row := range renderChart(m.history, m.cfg, m.chartSpan(), width) {
		b.WriteString(row + "\n")
	}

	b.WriteString("\n")
	b.WriteString(m.stats(width))

	if m.state == stateError {
		b.WriteString("\n" + errStyle.Render("✗ "+m.errorText()))
	}

	help := "h history  ·  q quit"
	if m.state != stateRunning {
		help = "r run again  ·  h history  ·  q quit"
	}
	out := boxStyle.Render(b.String()) + "\n"
	if m.saveErr != nil {
		// Below the box, where the border and padding do not eat the line.
		out += warnStyle.MaxWidth(width+boxChrome).Render(saveWarning(saveFailureReason(m.saveErr), width+boxChrome)) + "\n"
	}
	return out + helpStyle.Render("  "+help) + "\n"
}

// historyView is the box content of the history: the same header as the
// measurement, so live progress stays visible, then one row per stored run.
func (m model) historyView(width int, spaced bool) string {
	var b strings.Builder
	b.WriteString(spread(width, titleStyle.Render(appName), m.statusLine()))
	b.WriteString("\n\n" + strongStyle.Render("history") + "\n\n")

	switch {
	case m.pastRunsErr != nil:
		b.WriteString(warnStyle.MaxWidth(width).Render("⚠ history file could not be read"))
	case len(m.pastRuns) == 0:
		b.WriteString(statStyle.Render("no measurements saved yet"))
	default:
		sep := "\n"
		if spaced {
			sep = "\n\n"
		}
		b.WriteString(strings.Join(historyRows(m.pastRuns, width), sep))
	}
	return b.String()
}

// historyScreen is the whole history screen: the box and its help line.
func (m model) historyScreen(width int, spaced bool) string {
	return boxStyle.Render(m.historyView(width, spaced)) + "\n" +
		helpStyle.Render("  h/esc back  ·  q quit") + "\n"
}

// historyGap separates the columns of a history row.
const historyGap = 2

// historyRows lays the runs out newest first. Every row keeps its date, median,
// CV and duration; the bar chart takes only the width left over and is dropped
// first, below minHistoryBarCells, where the rows are the plain text list that
// drops columns in priority order (date, median, CV, duration) for as long as
// they fit width, so a narrow terminal loses detail rather than alignment.
func historyRows(runs []runRecord, width int) []string {
	cols, widths := historyCells(runs)

	cells := historyChartCells(runs, width)
	if cells == 0 {
		return historyTextRows(cols, widths, width)
	}
	top := historyScale(runs)

	pad := func(c, i int) string { return strings.Repeat(" ", widths[c]-lipgloss.Width(cols[c][i])) }
	gap := strings.Repeat(" ", historyGap)
	rows := make([]string, len(cols[0]))
	for i := range rows {
		rec := runs[len(runs)-1-i]
		rows[i] = cols[0][i] + pad(0, i) + gap +
			renderBar(rec, top, cells) + gap +
			pad(1, i) + cols[1][i] + gap +
			cols[2][i] + pad(2, i) + gap +
			pad(3, i) + cols[3][i]
	}
	return rows
}

// historyChartCells is the width of the bar chart at width, or 0 when the chart
// is omitted: no run has a median above 0, or the space left after the text
// columns is under minHistoryBarCells.
func historyChartCells(runs []runRecord, width int) int {
	if historyScale(runs) <= 0 {
		return 0
	}
	_, widths := historyCells(runs)
	textTotal := widths[0] + widths[1] + widths[2] + widths[3] + 3*historyGap
	cells := width - textTotal - historyGap
	if cells < minHistoryBarCells {
		return 0
	}
	return cells
}

// historyCells renders the four text columns of every run, newest first, and
// measures each.
func historyCells(runs []runRecord) (cols [4][]string, widths [4]int) {
	for i := len(runs) - 1; i >= 0; i-- {
		rec := runs[i]
		mark := warnStyle.Render("⚠")
		if rec.Stable {
			mark = okStyle.Render("✓")
		}
		cols[0] = append(cols[0], rec.At.Local().Format("2006-01-02 15:04"))
		cols[1] = append(cols[1], formatMbps(rec.MedianMbps))
		cols[2] = append(cols[2], fmt.Sprintf("CV %.1f%% ", rec.CVPercent)+mark)
		cols[3] = append(cols[3], fmt.Sprintf("%.1fs", rec.DurationSeconds))
	}
	for c := range cols {
		for _, cell := range cols[c] {
			widths[c] = max(widths[c], lipgloss.Width(cell))
		}
	}
	return cols, widths
}

// historyTextRows joins the columns that fit width.
func historyTextRows(cols [4][]string, widths [4]int, width int) []string {
	// Date and median always stay: together they are under the minimum width.
	shown, total := 2, widths[0]+historyGap+widths[1]
	for shown < len(cols) && total+historyGap+widths[shown] <= width {
		total += historyGap + widths[shown]
		shown++
	}

	rows := make([]string, len(cols[0]))
	for i := range rows {
		var line strings.Builder
		for c := 0; c < shown; c++ {
			cell := cols[c][i]
			pad := strings.Repeat(" ", widths[c]-lipgloss.Width(cell))
			switch c {
			case 0, 2:
				line.WriteString(cell + pad)
			default:
				line.WriteString(pad + cell)
			}
			if c < shown-1 {
				line.WriteString(strings.Repeat(" ", historyGap))
			}
		}
		rows[i] = line.String()
	}
	return rows
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

// saveWarning builds the one-line "history not saved" warning. It keeps the
// words "history not saved" and drops the icon before it would cut the reason.
func saveWarning(reason string, width int) string {
	msg := "history not saved: " + reason
	if lipgloss.Width("⚠ "+msg) <= width {
		return "⚠ " + msg
	}
	return msg
}
