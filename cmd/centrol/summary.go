package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/scirem/centrol/internal/ui"
)

// runCounters tallies one run's governed events as they're emitted, so
// the run summary (section 5) can report totals without a second pass
// over the ledger. "flagged" and "blocked" use exactly the same
// definitions `centrol audit --flagged`/`--blocked` do (policy.violation
// and tool.blocked respectively — see cmd_audit.go), so the numbers in
// the summary and what `centrol audit --flagged` actually finds always
// agree.
type runCounters struct {
	mu                      sync.Mutex
	total, flagged, blocked int
}

func newRunCounters() *runCounters { return &runCounters{} }

// wrap returns an EmitFunc-shaped function that tallies every emission
// through inner before forwarding to it — counting happens regardless
// of whether inner itself errors, since the ledger write already
// happened (or was validly silenced) by the time emit is called at all.
func (c *runCounters) wrap(inner func(run, src, typ string, payload map[string]interface{}) error) func(run, src, typ string, payload map[string]interface{}) error {
	return func(run, src, typ string, payload map[string]interface{}) error {
		c.mu.Lock()
		c.total++
		switch typ {
		case "policy.violation":
			c.flagged++
		case "tool.blocked":
			c.blocked++
		}
		c.mu.Unlock()
		return inner(run, src, typ, payload)
	}
}

func (c *runCounters) snapshot() (total, flagged, blocked int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total, c.flagged, c.blocked
}

// formatRunDuration renders a duration the way the summary block wants
// it: "14m 22s", or just "3s" for anything under a minute — never
// fractional seconds, since nobody needs sub-second precision on how
// long an agent run took.
func formatRunDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := d / time.Minute
	s := (d % time.Minute) / time.Second
	if m == 0 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %ds", m, s)
}

// printRunSummary is the one thing guard and proxy MUST print on every
// trappable exit path (section 5): normal exit, panic, SIGINT, SIGTERM,
// and best-effort on SIGHUP. It is never gated by log_level/--quiet —
// callers pass os.Stderr directly, not a Logger, specifically so
// --quiet cannot suppress it. It recovers from its own panics (section
// 5b) so a bug in summary rendering can never hide the real exit error
// or corrupt the process's exit code; if that happens, it logs the
// failure to stderr and returns, leaving the caller's own exit code
// intact.
func printRunSummary(w io.Writer, runID, ledgerFile string, started time.Time, counters *runCounters) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "centrol: warning: run summary failed to render: %v\n", r)
		}
	}()

	total, flagged, blocked := counters.snapshot()
	elapsed := formatRunDuration(time.Since(started))
	p := ui.NewPainter(w)

	fmt.Fprintf(w, "\nRun complete (%s)\n", elapsed)
	if flagged > 0 || blocked > 0 {
		warn := p.Yellow(ui.Glyph(w, ui.GlyphWarn))
		fmt.Fprintf(w, "  %s %d flagged, %d blocked  (out of %d events)\n", warn, flagged, blocked, total)
	} else {
		fmt.Fprintf(w, "  Events:   %d logged, %d flagged, %d blocked\n", total, flagged, blocked)
	}
	fmt.Fprintf(w, "  Ledger:   %s\n", ledgerFile)

	inspectFlags := ""
	switch {
	case blocked > 0:
		inspectFlags = " --blocked"
	case flagged > 0:
		inspectFlags = " --flagged"
	}
	fmt.Fprintf(w, "  Inspect:  centrol audit --run %s%s\n", runID, inspectFlags)
}

// earlySignalHandler catches a signal that arrives BEFORE the wrapped
// agent's own signal forwarding (internal/terminal.Run's signal.Notify)
// has taken over — e.g. during the snapshot phase of `centrol guard`,
// before terminal.Run is even called. Once the agent is actually
// running, real terminal-generated signals reach both centrol and its
// child directly (they share a process group), so the ordinary
// end-of-function code path already prints the summary and exits with
// the signal-derived code; this handler exists for the narrower window
// before that path is reachable at all, where nothing else would ever
// print a summary or set the right exit code.
//
// Deliberately portable: os.Interrupt and syscall.SIGTERM are both
// defined on every platform centrol builds for (including Windows,
// where SIGHUP has no equivalent and forwardedSignals omits it — see
// internal/terminal/forward_windows.go), so this file needs no
// _unix/_windows split of its own.
func earlySignalHandler(onSignal func(code int)) (stop func()) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-sigCh:
			code := 130 // SIGINT
			if sig == syscall.SIGTERM {
				code = 143
			}
			onSignal(code)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}
