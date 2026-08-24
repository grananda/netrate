package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// Exit codes, so the tool composes in a script.
const (
	exitOK          = 0
	exitFailed      = 1 // the measurement ran but produced no verdict
	exitUsage       = 2 // the flags do not describe a runnable measurement
	exitInterrupted = 130
)

func main() {
	cfg := DefaultConfig()

	flag.StringVar(&cfg.URL, "url", cfg.URL, "large, incompressible file to download")
	flag.IntVar(&cfg.Streams, "streams", cfg.Streams, "concurrent TCP connections")
	flag.DurationVar(&cfg.Duration, "duration", cfg.Duration, "maximum length of the download phase")
	flag.DurationVar(&cfg.WarmUp, "warmup", cfg.WarmUp, "leading window discarded from the statistics")
	flag.DurationVar(&cfg.Interval, "interval", cfg.Interval, "sampling period for instantaneous speed")
	// Registered with a zero default on purpose. Given a real one, -h would
	// print "(default 6.3s)" next to a help line saying the default tracks
	// -duration, and only one of the two could be true.
	minRun := flag.Duration("minrun", 0,
		"keep measuring at least this long before a settled reading may stop the run\n(default: 45% of -duration); set it equal to -duration to use the whole budget")
	plain := flag.Bool("plain", false, "no interface: print the result and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	// Answered before anything is validated: asking a binary what it is must
	// work even when the rest of the command line does not.
	if *showVersion {
		fmt.Println(versionString())
		os.Exit(exitOK)
	}

	// -minrun follows -duration unless it was named on the command line. A
	// fixed minimum makes -duration inert: a stable link would end every run
	// at the same moment whether you asked for 14 seconds or for five minutes.
	cfg.MinRun = defaultMinRun(cfg.Duration)
	if flagWasSet("minrun") {
		cfg.MinRun = *minRun
	}

	// Fail on the flags rather than deep inside the engine, where a zero
	// duration or an unreachable URL surfaces as something unrecognisable.
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		flag.Usage()
		os.Exit(exitUsage)
	}

	// The chart needs a real terminal. IDE run consoles and pipes are not
	// one, so fall back instead of dying on an opaque /dev/tty error.
	if *plain || !interactive() {
		if !*plain {
			fmt.Fprintln(os.Stderr, "warning: no interactive terminal, falling back to -plain")
		}
		os.Exit(runPlain(cfg))
	}

	os.Exit(runTUI(cfg))
}

// flagWasSet reports whether a flag was named on the command line, as opposed
// to being left at its default.
func flagWasSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// interactive reports whether both ends are attached to a terminal.
func interactive() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// runTUI drives the full-screen interface. On failure it does not fall back to runPlain: the
// model's engine is already downloading by then, and starting a second
// measurement on top of it would compete for the same link and report nonsense.
func runTUI(cfg Config) int {
	first := newModel(cfg)

	final, err := tea.NewProgram(first).Run()

	// The model that quit is not necessarily the one we started: "r" swaps in
	// a fresh one with its own context. Cancel both so no engine outlives us.
	first.cancel()
	if last, ok := final.(model); ok {
		last.cancel()
		if err == nil && last.state == stateError {
			return exitFailed
		}
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "could not run the interface: %v\n", err)
		fmt.Fprintln(os.Stderr, "try again with -plain")
		return exitFailed
	}
	return exitOK
}

// runPlain is the pipe-friendly path: same engine, no interface.
//
// Everything that is not the final report goes to stderr, so `speed -plain >
// out.txt` captures the numbers and nothing else — no carriage returns, no
// escape codes, no half-drawn progress line.
func runPlain(cfg Config) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	events := make(chan Event, 256)
	go Run(ctx, cfg, events)

	progress := newProgressLine()

	var (
		result Result
		done   bool
	)
	for ev := range events {
		switch ev := ev.(type) {
		case PhaseEvent:
			progress.printf("%s...", ev.Phase)
		case SampleEvent:
			mark := " "
			if ev.Sample.Warm {
				mark = "*"
			}
			progress.printf("%s %5.1fs  %s", mark, ev.Sample.At.Seconds(), formatMbps(ev.Sample.Mbps))
		case DoneEvent:
			result, done = ev.Result, true
		case ErrEvent:
			progress.clear()
			fmt.Fprintf(os.Stderr, "error measuring download: %v\n", ev.Err)
			return exitFailed
		}
	}
	progress.clear()

	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "interrupted")
		return exitInterrupted
	}
	if !done {
		fmt.Fprintln(os.Stderr, "the measurement ended without a result")
		return exitFailed
	}

	printReport(result, cfg)
	return exitOK
}

func printReport(r Result, cfg Config) {
	stability := "stable"
	if !r.Stable {
		stability = "unstable"
	}
	fmt.Printf("Download   %s  (median of %d samples, %d streams)\n",
		formatMbps(r.Median), r.Samples, r.Streams)
	fmt.Printf("Trimmed    %s\n", formatMbps(r.Trimmed))
	fmt.Printf("Range      %s – %s\n", formatMbps(r.Min), formatMbps(r.Max))
	fmt.Printf("CV         %.1f%% (%s)\n", r.CV, stability)
	fmt.Printf("Volume     %s in %.1fs\n", formatBytes(r.Bytes), r.Duration.Seconds())

	// Without this the run just looks short: the reading settled, so measuring
	// the rest of the budget would not have changed the answer.
	if r.EndedEarly {
		fmt.Printf("Stopped    early at %.1fs of the %.1fs budget (the reading settled; -minrun %s to measure longer)\n",
			r.Duration.Seconds(), cfg.Duration.Seconds(), cfg.Duration)
	}
}

// progressLine redraws a single line in place. It goes quiet when stderr is not
// a terminal, because carriage returns in a log file are just noise.
type progressLine struct {
	enabled bool
	width   int // longest line drawn so far, to know how much to erase
}

func newProgressLine() *progressLine {
	return &progressLine{enabled: isTerminal(os.Stderr)}
}

func (p *progressLine) printf(format string, args ...any) {
	if !p.enabled {
		return
	}
	text := fmt.Sprintf(format, args...)
	pad := max(p.width-len(text), 0)
	p.width = max(p.width, len(text))
	fmt.Fprintf(os.Stderr, "\r%s%s", text, spaces(pad))
}

func (p *progressLine) clear() {
	if !p.enabled || p.width == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\r%s\r", spaces(p.width))
	p.width = 0
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%*s", n, "")
}
