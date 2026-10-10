package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/scirem/centrol/internal/governor"
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
	streamErr               bool // set via markStreamError when a pump's underlying I/O read failed (see proxy.ErrStreamRead), as opposed to an ordinary clean stream close
}

func newRunCounters() *runCounters { return &runCounters{} }

// markStreamError records that this run ended (at least in part) because
// a pump's read failed rather than the stream simply closing cleanly —
// printRunSummary surfaces this distinctly so it isn't mistaken for an
// ordinary completed run.
func (c *runCounters) markStreamError() {
	c.mu.Lock()
	c.streamErr = true
	c.mu.Unlock()
}

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

func (c *runCounters) hadStreamError() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamErr
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
	if counters.hadStreamError() {
		warn := p.Yellow(ui.Glyph(w, ui.GlyphWarn))
		fmt.Fprintf(w, "  %s stream error — a pump's underlying read failed; see the ledger's policy.silence entry for details\n", warn)
	}

	inspectFlags := ""
	switch {
	case blocked > 0:
		inspectFlags = " --blocked"
	case flagged > 0:
		inspectFlags = " --flagged"
	}
	fmt.Fprintf(w, "  Inspect:  centrol audit --run %s%s\n", runID, inspectFlags)
}

// endRun returns the single run-scoped "end the run" decision point
// cmd_guard.go/cmd_proxy.go's exit sites call through, from the point
// runID is acquired onward (Pass 3.9c Final: "the Governor owns
// termination" for a run-scoped operation). It is the CLI-side half
// of that contract — the Governor-side half is Governor.Stop/Exit
// (see internal/governor/run.go) — and it is the only place in either
// command that calls Governor.Exit.
//
// Every call attempts g.Stop(cause, code). Unlike the pre-3.9c-Final
// newShutdown this replaces, a losing call does not simply return:
// losing only means a DIFFERENT call already decided the run's
// outcome (another exit site, a signal via WatchSignals below, or a
// panic/context-cancellation decided inside Governor.Run itself) — it
// does not mean termination is already in progress on this call's
// behalf, since that other decision may have come from a goroutine
// that has no way to finish the run-scoped teardown itself (a
// Governor.Run panic recovery, for instance, has no access to
// printRunSummary/clearMarker). So every call, win or lose, ends by
// calling g.Exit(); the finish (summary + marker clear + any
// win-specific message) happens at most once, guarded by its own
// sync.Once, regardless of which call reaches it first.
func endRun(g *governor.Governor, runID, ledgerFile string, started time.Time, counters *runCounters, clearMarker func()) func(cause string, code int, printMsg func()) {
	return endRunOpts(g, runID, ledgerFile, started, counters, clearMarker, true)
}

// endRunOpts is endRun with control over the run summary. With
// summary=false, a call that wins the race to decide the run's outcome
// finishes without the "Run complete" summary: it is for a startup that
// failed before the wrapped command ever ran, where "Run complete" would
// be false. A call that loses the race still prints the summary, since
// someone else (a signal) decided the outcome and the run did get going.
func endRunOpts(g *governor.Governor, runID, ledgerFile string, started time.Time, counters *runCounters, clearMarker func(), summary bool) func(cause string, code int, printMsg func()) {
	var finished sync.Once
	finish := func(printMsg func(), withSummary bool) {
		finished.Do(func() {
			if withSummary {
				printRunSummary(os.Stderr, runID, ledgerFile, started, counters)
			}
			clearMarker()
			if printMsg != nil {
				printMsg()
			}
		})
	}
	return func(cause string, code int, printMsg func()) {
		if g.Stop(cause, code) {
			finish(printMsg, summary)
		} else {
			// Someone else's Stop call already decided the run's
			// outcome; still finish (exactly once, via the shared
			// sync.Once above) so the run is never left without a
			// summary or a cleared marker just because this call lost
			// the race, and still terminate with the winning decision's
			// recorded exit code.
			finish(nil, true)
		}
		g.Exit()
	}
}

// newSignalWatch is the pre-Run setup window's signal-to-context
// bridge (Pass 3.9c Final correction): the Governor's WatchSignals
// callback here never calls end or Exit itself — finishing the run on
// main's own goroutine is left entirely to runSetup below. That is
// the fix for the flagged deviation in Governor.Exit's original doc
// comment: Exit must only ever be called from main's own goroutine,
// and a signal handler calling it directly (even via end(), even in
// agreement with whatever cause/code the rest of the run would have
// recorded) is a second goroutine racing Exit against the first — the
// same class of concurrent-termination hazard Pass 3.9c's Run-phase
// redesign exists to close, just in a different window.
//
// It DOES call g.Stop(cause, code) directly, though — unlike Exit,
// Stop is explicitly designed for exactly this (Governor.Run's own
// internal signal watch does the same thing, from its own goroutine,
// for the same reason): it just records a cause/code, first-call-wins,
// and losing a race to another Stop call is a normal, harmless
// outcome, never a correctness hazard the way two calls to Exit are.
// This matters because this watch's registration is never unregistered
// (see stop's own doc comment below): if the one OS-level delivery of
// a signal lands on THIS watch's channel rather than on whatever
// registers next (terminal.Run's own forwarding for guard; Governor.Run's
// own watch for proxy) — which can happen if that next registration
// hasn't occurred yet — nothing else will ever see that signal again.
// For guard that's merely wasted (terminal.Run's raw forwarding isn't
// the only thing that reacts: the OS also delivers the same signal
// directly to the wrapped agent, independent of any of this). For
// proxy it is not merely wasted: Governor.Run's own signal watch is
// the ONLY thing that ever calls g.Stop with the signal's real cause
// during a run, since Run manages its target directly with no
// separate forwarding layer — if THIS watch intercepts the one
// delivery instead, and only recorded it into its own ctx/ignored it,
// Governor's bookkeeping would never learn a signal happened at all,
// even though the target process itself (sharing the same process
// group) may have died from the raw delivery anyway — producing a
// plain error instead of the signal's proper cause and exit code.
// Reproduced directly: TestShipCriterion9_ProxySummaryPrintsOnSigint
// returned exit 1 ("signal: interrupt" as an ordinary error) instead
// of 130, under a full `-race -count=10` run, before this call was
// added. Calling g.Stop here as well as recording into signalOutcome
// closes that: whichever channel actually receives the delivery, the
// Governor's own cause/code ends up correct either way.
//
// stop settles the signal watch (see Governor.WatchSignals' doc
// comment for what that means) and cancels ctx, so it's safe to defer
// or call explicitly when handing signal responsibility to whatever
// takes over next (terminal.Run's forwarding for guard; Governor.Run's
// own watch for proxy). Deliberately absent from stop: a call to
// WatchSignals' unregister. This watch's registration is left in
// place — still listening at the OS level, just never acted on again
// — for the rest of the process's life, rather than torn down before
// handing off. The alternative (settle, then unregister, then call
// terminal.Run/Governor.Run, exactly mirroring Run's own use of
// WatchSignals) opens a real window where NOTHING is registered for
// these signals: between unregister here and terminal.Run's/
// Governor.Run's own signal.Notify call a moment later, a signal
// whose actual OS-level delivery (not merely its sending) lands in
// that gap reverts to the OS default disposition and kills the
// process outright, bypassing every Go-level handler — reproduced
// directly under heavy scheduler contention. Leaving this
// registration active costs one unread, capacity-1 channel for the
// remainder of a short-lived CLI process's life; that is cheaper than
// the alternative's gap. See WatchSignals' own doc comment
// (internal/governor/run.go) for the full reasoning.
//
// stop's own cancel() call is unconditional — ordinary Go hygiene,
// releasing ctx's resources whether or not a signal ever arrived —
// which matters for callers: ctx.Err() being non-nil after stop()
// returns does NOT by itself mean a signal was received, only that
// stop() has now run. Whether a real signal arrived is
// signalOutcome's question to answer, not ctx's: recordedCause stays
// "" unless onReceived actually ran, and signalCause
// (internal/governor/run.go) never produces an empty cause for a real
// signal, so checkCancelled below checks that instead of ctx.Err().
func newSignalWatch(g *governor.Governor) (ctx context.Context, signalOutcome func() (cause string, code int), stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var recordedCause string
	var recordedCode int
	settleWatch, _ := g.WatchSignals(func(cause string, code int) {
		mu.Lock()
		recordedCause, recordedCode = cause, code
		mu.Unlock()
		g.Stop(cause, code)
		cancel()
	})
	return ctx, func() (string, int) {
			mu.Lock()
			defer mu.Unlock()
			return recordedCause, recordedCode
		}, func() {
			settleWatch()
			cancel()
		}
}

// runSetup is how every pre-Run setup step in cmd_guard.go/cmd_proxy.go
// runs (Pass 3.9c Final correction): step executes in its own
// goroutine, and runSetup blocks the CALLER's goroutine — main's own,
// since runSetup is only ever invoked synchronously from
// cmdGuard/cmdProxy, never from a goroutine of its own — on a select
// between step finishing and ctx being cancelled by a signal (see
// newSignalWatch above). The setup window has no cancellation
// plumbing of its own otherwise (LoadIgnore, Snapshot, the policy
// resolvers, and the allowlist prompt are all plain blocking calls),
// so without this a signal arriving mid-step would have nothing to
// interrupt main with, which is exactly the condition the old
// signal-handler-calls-Exit-directly design existed to work around —
// and exactly what reintroduced a second goroutine able to reach Exit.
//
// If step finishes first, runSetup returns its error normally and the
// caller proceeds exactly as it would have without any of this. If
// ctx is cancelled first, runSetup finishes the run itself — calling
// end (and so Stop, then Exit) right here, on the caller's own
// goroutine — and never returns to the caller, since end's call to
// Governor.Exit does not return. step's own goroutine, if still
// running at that point, is abandoned; its result, if it ever
// arrives, is never read. That is an accepted leak on shutdown, not a
// correctness gap — the process is terminating either way.
func runSetup(ctx context.Context, end func(cause string, code int, printMsg func()), signalOutcome func() (string, int), step func() error) error {
	done := make(chan error, 1)
	go func() { done <- step() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		cause, code := signalOutcome()
		end(cause, code, nil)
		return nil // unreachable: end always calls Governor.Exit, which terminates the process.
	}
}

// checkCancelled finishes the run (via end, exactly like runSetup's
// own ctx-cancellation branch) if a signal already landed by the time
// this is called. It exists for the gaps runSetup's own select cannot
// see: a signal that arrives between two setup steps, where nothing
// is blocked on anything for ctx.Done() to interrupt, is otherwise
// silently absorbed into a cancelled context that nothing then reads
// — cancelled, recorded, and never acted on.
//
// cmd_guard.go/cmd_proxy.go call this immediately AFTER
// stopSignalWatch(), not before: stopSignalWatch (WatchSignals' own
// stop closure, internal/governor/run.go) blocks until its goroutine
// has made its final, settled decision about any signal it may have
// received, so calling this only after it returns is what makes the
// check complete — it covers both an earlier gap (a signal between
// two prior runSetup calls) and a signal landing in the exact instant
// of the handoff to stopSignalWatch itself, which a check placed
// before stopSignalWatch returned could still miss (the signal
// consumed by this watch, but onReceived not yet run).
//
// It checks signalOutcome's recorded cause, not ctx.Err(): stop's own
// cancel() call is unconditional (see newSignalWatch's doc comment),
// so by the time this runs, ctx.Err() is non-nil regardless of
// whether a signal ever actually arrived. recordedCause stays ""
// unless onReceived genuinely ran, which is the only reliable signal
// here.
func checkCancelled(end func(cause string, code int, printMsg func()), signalOutcome func() (string, int)) {
	cause, code := signalOutcome()
	if cause == "" {
		return
	}
	end(cause, code, nil)
}
