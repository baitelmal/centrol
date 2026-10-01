package governor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/scirem/centrol/internal/proxy"
)

// RunTransport is the capability Governor.Run needs from whatever
// fronts the real MCP server. It is a consumer-defined interface
// (Pass 3.9 Section 1): defining it here, rather than importing
// internal/proxy/transport for its concrete Target interface, is what
// lets the Governor drive transport.StdioTarget and transport.HTTPTarget
// without this package ever importing internal/proxy/transport.
// internal/proxy and internal/proxy/transport do not import
// internal/governor (verified, Pass 3.9 pre-implementation report, item
// 4), so this package's own import of internal/proxy below — needed
// for the *proxy.Interceptor type Governor.Run drives — is a sound
// downward dependency, not a cycle.
type RunTransport interface {
	Start(ctx context.Context) error
	Send(frame []byte) error
	Receive() (<-chan []byte, error)
	Stop() error
}

// targetErrReporter is an optional capability a RunTransport may
// implement to report why its Receive() channel closed: nil for a
// clean end, or the underlying read failure otherwise (see
// transport.StdioTarget.Err). Not part of RunTransport itself, since
// not every transport has a meaningful distinction here; Governor.Run
// checks for it via a type assertion. Moved verbatim from
// internal/proxy/run.go (Pass 3.9c) — same name, same debug string
// below, deliberately preserved for TestRunTargetDrivesMinimalTarget-
// WithoutOptionalCapabilities (now internal/governor/run_test.go).
type targetErrReporter interface {
	Err() error
}

// targetWaiter is an optional capability a RunTransport may implement
// to block until it has fully finished shutting down, after Stop has
// signaled it to do so — e.g. transport.StdioTarget.Wait, which calls
// cmd.Wait() to reap the subprocess. Moved verbatim from
// internal/proxy/run.go; see targetErrReporter's doc comment.
type targetWaiter interface {
	Wait() error
}

// Phase is a run's lifecycle phase, owned exclusively by the Governor's
// run/change hand (Stop/MarkStopped below). Phase is for diagnostics
// and for Stop's own first-writer-wins guard — Governor.Run's pump
// orchestration does not consult it.
type Phase int

const (
	PhaseRunning Phase = iota
	PhaseStopping
	PhaseStopped
)

func (p Phase) String() string {
	switch p {
	case PhaseRunning:
		return "running"
	case PhaseStopping:
		return "stopping"
	case PhaseStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// runState is the run/change hand's lifecycle state (Pass 3.9 Section
// 2): one struct, one mutex, one owner (the Governor that embeds it).
// Zero value is PhaseRunning, which is the correct starting phase for
// a freshly constructed Governor — no explicit initialization needed
// in New().
type runState struct {
	mu       sync.Mutex
	phase    Phase
	cause    string
	exitCode int
}

// Stop is the run/change hand's sole lifecycle-ending decision point:
// the one place "the run ends" gets decided, for both ordinary error
// exits and the SIGINT/SIGTERM/race-(b) paths. Transitions are
// first-writer-wins by explicit phase check (Pass 3.9 Section 2): the
// first call to observe phase == running flips it to stopping, records
// cause/exitCode, and returns true (it won — its cause/exitCode are
// the run's recorded outcome). Every later call, concurrent or not,
// same cause or different, sees phase already past running and returns
// false without re-deciding anything. No sync.Once needed; the phase
// check is the guard.
//
// Deviation from the Pass 3.9 spec's literal Stop(cause string)
// signature, flagged and approved ahead of 3.9c (pre-implementation
// report, item pending confirmation): extended to
// Stop(cause string, exitCode int) bool. The bool is required so a
// caller can tell whether it won — race (b)'s two sites can normalize
// to the identical cause string ("sigterm") and still need to know
// which one's exitCode is authoritative. exitCode must be
// caller-supplied because the Governor cannot statically know the
// wrapped agent's own dynamic exit code (e.g. terminal.Run's 128+sig,
// or the agent's own process exit status).
func (g *Governor) Stop(cause string, exitCode int) bool {
	g.run.mu.Lock()
	defer g.run.mu.Unlock()
	if g.run.phase != PhaseRunning {
		return false
	}
	g.run.phase = PhaseStopping
	g.run.cause = cause
	g.run.exitCode = exitCode
	return true
}

// MarkStopped completes the transition Stop began, once the HOW-tier
// teardown Stop triggered (pump drain, target.Stop/Wait) has actually
// finished. Separate from Stop: Stop only records the *decision* that
// the run ends and who made it; the teardown it triggers still has to
// run to completion afterward. Not currently called by any CLI path in
// this pass (both cmd_guard.go and cmd_proxy.go call Governor.Exit
// immediately after every exit site's Stop attempt, win or lose, via
// the shared endRun closure in summary.go, so there is no further
// process lifetime in which "stopped" would be observed) — kept
// because Phase()/Cause() are documented diagnostics surfaces and a
// caller that does NOT exit immediately (a future embedding of the
// Governor, or a test) needs a way to reach PhaseStopped.
func (g *Governor) MarkStopped() {
	g.run.mu.Lock()
	defer g.run.mu.Unlock()
	if g.run.phase == PhaseStopping {
		g.run.phase = PhaseStopped
	}
}

// Phase reports the run's current lifecycle phase. Diagnostics only.
func (g *Governor) Phase() Phase {
	g.run.mu.Lock()
	defer g.run.mu.Unlock()
	return g.run.phase
}

// Cause reports why Stop was called (empty until the first winning
// call). Diagnostics only.
func (g *Governor) Cause() string {
	g.run.mu.Lock()
	defer g.run.mu.Unlock()
	return g.run.cause
}

// ExitCode reports the exit code the winning Stop call recorded.
// Diagnostics only.
func (g *Governor) ExitCode() int {
	g.run.mu.Lock()
	defer g.run.mu.Unlock()
	return g.run.exitCode
}

// Exit is the Governor's sole process-termination call (Pass 3.9c
// Final, "The Governor owns termination"): it terminates the process
// with the exit code the winning Stop call recorded. Every run-scoped
// operation's termination — ordinary completion, a signal, a pump
// panic, context cancellation — funnels through Stop first (deciding
// the outcome) and Exit second (acting on it); no other call in a
// run-scoped path may call os.Exit. A non-run command's own declared
// exit authority (fatalf/fatalHint; see cmd/centrol/helpers.go) is
// deliberately out of scope — those commands have no run lifecycle
// for the Governor to arbitrate.
//
// Exit is always called from main's own goroutine. During Run, that
// holds because Run itself owns its signal/panic/context-cancellation
// routing and only ever calls Stop, returning control to the caller
// (main's goroutine) before Exit is ever reached. Before Run — the
// pre-Run setup window in cmd_guard.go/cmd_proxy.go, which has no
// cancellation plumbing of its own (LoadIgnore, Snapshot, the policy
// resolvers, and the allowlist prompt are all plain blocking calls) —
// it holds because that window's own signal watch (see
// cmd/centrol's newSignalWatch) never calls Stop or Exit itself: it
// only cancels a context, which main's own goroutine observes via a
// select (cmd/centrol's runSetup) and acts on from there. A signal
// handler calling Exit directly, even in full agreement with whatever
// the rest of the run would have decided, is a second goroutine able
// to reach Exit — the same class of concurrent-termination hazard
// this whole pass exists to close, just in a different window; an
// earlier draft of this pass had exactly that gap, flagged and
// corrected before this comment's final form.
func (g *Governor) Exit() {
	os.Exit(g.ExitCode())
}

// signalCause maps a signal this Governor's run lifecycle recognizes
// to the cause string and conventional exit code Stop should record
// for it (Pass 3.9c Final, Section 2 "Signals"). Any signal outside
// this set is not registered in the first place (see WatchSignals /
// Run's own signal.Notify below), so the OS default handles it
// around the Governor, per spec.
func signalCause(sig os.Signal) (cause string, code int) {
	switch sig {
	case syscall.SIGTERM:
		return "sigterm", 143
	case syscall.SIGINT:
		return "sigint", 130
	case syscall.SIGHUP:
		return "sighup", 129
	case syscall.SIGQUIT:
		return "sigquit", 131
	default:
		return "signal", 1
	}
}

// signalFromWaitErr reports the signal a target subprocess was killed
// by, if err is (or wraps) an *exec.ExitError whose underlying
// syscall.WaitStatus says the process was signaled. Used by Run's
// Wait() handling above to recognize that outcome directly, rather
// than relying solely on this process's own, separately and
// asynchronously delivered, copy of the same signal — see that call
// site's doc comment for the race this closes. ok is false for any
// other error (a non-signal exit, a non-exec-based targetWaiter, or
// nil), in which case the caller must not treat sig as meaningful.
func signalFromWaitErr(err error) (sig os.Signal, ok bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil, false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return nil, false
	}
	return ws.Signal(), true
}

// WatchSignals is the Governor-owned replacement for the old CLI-level
// earlySignalHandler (Pass 3.9c Final, Section 2 "Signals" / Section 1
// "the Governor registers signal handlers"): it registers SIGTERM,
// SIGINT, SIGHUP, and SIGQUIT via signal.Notify and reports each one's
// conventional cause/exit code to onReceived exactly once. It
// deliberately does not call Stop itself: recording the decision is
// left to the caller's own single termination path (cmd/centrol's
// endRun), so a signal-triggered termination and every other kind
// (an ordinary resolve error, a panic, context cancellation) all
// race through the exact same Stop call rather than two different
// call sites each deciding independently — which is the condition
// that produced the pre-3.9c shutdown races in the first place.
//
// It returns two independent operations, deliberately not one:
//
//   - settle blocks until any signal already in flight has finished
//     being processed (onReceived called, if it ever was going to be)
//     and this watch will never call onReceived again. A caller must
//     call this before inspecting whatever onReceived recorded;
//     without that ordering, it can observe a half-finished decision
//     (see its own inline doc for the race this closes).
//   - unregister actually calls signal.Stop, removing this watch's
//     channel from the registered recipients for these signals. A
//     caller about to hand these same signals to someone else's
//     registration (terminal.Run's own forwarding; a later
//     WatchSignals call) must call settle first, THEN confirm that
//     someone else's registration is already active, and ONLY THEN
//     call unregister — never the reverse, and never with a gap
//     between the handoff and this call. Go's os/signal delivers a
//     given signal instance to every channel registered for it at
//     the moment the OS actually delivers it — not at the moment it
//     was merely sent — so overlap between two registrations is
//     always safe (both get it; one of them not reading it again is
//     harmless), while a window with NEITHER registered is not: a
//     signal whose real OS-level delivery lands in that window
//     reverts to the OS default disposition, which for SIGINT/SIGTERM
//     terminates the process outright, bypassing every Go-level
//     handler entirely. Reproduced directly against cmd_guard.go's
//     pre-Run watch, under heavy scheduler contention: the signal was
//     sent well before stopSignalWatch, this watch's settle found
//     nothing to report (the OS had not actually delivered it to this
//     process yet), unregister ran, and only THEN — in the resulting
//     gap, before terminal.Run's own signal.Notify — did the OS
//     finally deliver it, killing the process raw (exit status
//     reported "terminated by signal", not any code this run ever
//     chose). A caller with nothing registered to hand off to simply
//     calls both, in order; a caller handing off to another
//     registration calls ONLY settle and leaves unregister uncalled,
//     relying on the overlap instead — see cmd/centrol's
//     newSignalWatch for that case.
//
// signalsRegistered is a test seam, a no-op in production: it fires
// the instant signal.Notify above has returned, i.e. the instant this
// process is actually listening for the signals WatchSignals claims to
// watch. A test that sends this process a REAL OS signal (SIGINT,
// SIGTERM) to exercise that handling has exactly one safe moment to do
// it — any earlier, and the signal falls through to the OS default
// disposition, which for SIGINT/SIGTERM kills the entire test binary,
// not just that one test. Start()-returned or a fixed sleep are both
// proxies for "registration has happened," not the fact itself, and
// under heavy scheduler contention (a full `-race -count=N` run) the
// gap between either proxy and the real registration is not reliably
// negligible — see TestGovernorRunStopsOnSIGINT, which reproduced
// exactly this failure (the whole test binary killed by its own
// self-sent SIGINT) under that load before this hook existed.
var signalsRegistered = func() {}

func (g *Governor) WatchSignals(onReceived func(cause string, code int)) (settle func(), unregister func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	signalsRegistered()
	done := make(chan struct{})
	// settled closes once the goroutine below has made its one, final
	// decision — delivered a signal, or determined there was none to
	// deliver. settle (below) blocks on it, which is the actual fix
	// (see settle's own doc comment): without that block, onReceived
	// could still fire asynchronously, strictly after settle() had
	// already returned control to a caller who, by then, has moved on
	// to something that no longer reads this watch's ctx at all — a
	// real signal, consumed by this watch, whose effect lands nowhere.
	settled := make(chan struct{})
	deliver := func(sig os.Signal) {
		if onReceived != nil {
			cause, code := signalCause(sig)
			onReceived(cause, code)
		}
	}
	go func() {
		defer close(settled)
		select {
		case sig := <-sigCh:
			deliver(sig)
		case <-done:
			// A signal that was genuinely delivered into sigCh (by the
			// runtime's own signal-forwarding goroutine, independent of
			// this one) and done closing by stop() below can both be
			// ready here at the same moment — and select picks
			// uniformly at random among simultaneously-ready cases, not
			// in source order (the exact mechanism the "final gap" bug
			// this pass fixed turned on; see checkCancelled's doc
			// comment in cmd/centrol/summary.go for the sibling
			// instance). Without this, that random pick can silently
			// drop a signal that had already arrived, right as the
			// caller's run-scoped window hands off to whatever takes
			// over next.
			// sigCh is buffered (capacity 1), so a signal already
			// delivered is still sitting there to read, non-blocking,
			// even after done's case was the one select chose.
			select {
			case sig := <-sigCh:
				deliver(sig)
			default:
			}
		}
	}()
	var settleOnce sync.Once
	settle = func() {
		settleOnce.Do(func() {
			close(done)
			// Block until the goroutine above has made its final call.
			// Without this, settle() could return — and a caller might
			// act on whatever onReceived had (or hadn't yet) recorded —
			// before a signal that arrived in the exact instant of this
			// call finished being processed: onReceived (and so the
			// caller's cancel()) would then fire asynchronously, after
			// the caller has already moved on to code that never looks
			// at this watch's ctx again. Reproduced directly: a real
			// SIGINT sent to cmd_guard.go's pre-Run window, right as its
			// signal watch was being torn down, was correctly picked up
			// by this watch's own sigCh — deliver and onReceived both
			// ran — but only after cmd_guard.go had already checked for
			// a cancellation (found nothing yet) and moved on, so the
			// cancellation that did eventually happen had no reader
			// left. Blocking here until settled closes that: by the
			// time settle() returns, onReceived has already been called
			// if it is ever going to be.
			<-settled
		})
	}
	unregister = func() {
		signal.Stop(sigCh)
	}
	return settle, unregister
}

// Run drives one MCP session end-to-end: starts target, pumps
// clientIn -> target and target -> clientOut through in, and blocks
// until the target has fully shut down or an unrecoverable pipe error
// occurs. This is internal/proxy/run.go's former RunTarget, relocated
// here per Pass 3.9 Shape A (run.go deleted entirely; see the commit
// message for why this shape was chosen over Shape B).
//
// The one authority change from the original RunTarget (Pass 3.9
// Section 3): pumps no longer decide or trigger shutdown themselves.
// The client pump no longer has its own `defer stop()` — it only
// signals clientPumpDone when its loop ends, same channel-contract
// shape ("one value, exactly once") as its other two channels. This
// function's own body is the sole place that calls targetStop(): once
// via a small goroutine reacting to clientPumpDone (the common case —
// client stopped sending, mirroring the old defer's trigger), and once
// as the fallback after draining targetToClientDone (the target ended
// on its own before the client pump got there). Exactly one of the two
// actually performs the stop (stopOnce) — only the trigger moved out
// of the pump body, not the at-most-once guarantee.
//
// targetStop() (call-scoped, local to this one invocation, no
// run-ending semantics of its own) is deliberately NOT the same
// mechanism as Governor.Stop/MarkStopped above (run-scoped, process-
// wide, the CLI's actual exit-code decision). An early design draft
// conflated the two — letting the pump's own ordinary completion "win"
// the run-ending race with a meaningless placeholder exit code before
// the CLI's real decision downstream ever got a turn — and was caught
// and corrected before any code was written.
//
// Pass 3.9c Final adds three more routes into that same Stop
// decision, closing the gap this doc comment used to flag ("Unlike
// centrol guard, Governor.Run has no signal handling of its own"):
//
//   - Signals: Run registers its own WatchSignals before target.Start
//     is even called, and holds that registration for its full
//     duration (SIGTERM, SIGINT, SIGHUP, SIGQUIT) — unlike the CLI-level
//     watch cmd_guard.go/cmd_proxy.go use for their pre-Run setup
//     windows. On receipt, Run calls Stop itself and triggers
//     targetStop(), so Run unwinds and returns promptly instead of
//     leaving the caller's own signal watcher to decide while main is
//     blocked inside this call. Registering before Start matters: see
//     the inline comment at the registration site for the gap this
//     closes (TestShipCriterion9_ProxySummaryPrintsOnSigint).
//   - Panics: every goroutine Run spawns has a top-level deferred
//     recover that calls Stop("panic", 2) and, for the two pump
//     goroutines, still satisfies that goroutine's completion-channel
//     contract (clientPumpDone / targetToClientDone) so Run's own
//     blocking receives can never hang on a goroutine that panicked
//     instead of finishing normally.
//   - Context cancellation: ctx.Done() is watched alongside signals;
//     on cancellation, Run calls Stop("context_cancelled", 2) and
//     targetStop().
//
// None of the three call Exit — only Stop, plus the same targetStop()
// mechanism ordinary completion already uses to unwind the pumps.
// Exit stays the caller's job, after Run has returned (see Exit's doc
// comment for the one documented exception to that).
func (g *Governor) Run(ctx context.Context, target RunTransport, in *proxy.Interceptor, clientIn io.Reader, clientOut, diagOut io.Writer) error {
	clientToTargetErr := make(chan error, 1)
	targetToClientDone := make(chan error, 1) // the goroutine that writes to clientOut — Run must not return until it has actually finished
	clientPumpDone := make(chan struct{}, 1)  // "no more input is coming" — reported upward, not acted on, by the client pump itself
	runDone := make(chan struct{})            // closed when Run itself is returning, so the signal/context watcher below never outlives this call
	defer close(runDone)
	var clientOutMu sync.Mutex // WriteFrame calls interleave from both pump directions; guard the shared writer so frames never partially interleave on the wire.
	var diagOutMu sync.Mutex   // the capability-degrade debug note below can fire alongside the two pump goroutines; guard diagOut the same way clientOutMu guards clientOut.

	// targetStop is the call-scoped shutdown *signal* (see targetWaiter's
	// doc comment for the signal/wait split) and must fire exactly once:
	// twice would mean a second DELETE for HTTPTarget, or a second
	// stdin-close race for StdioTarget. Declared before target.Start is
	// even called (see the registration below) — both StdioTarget.Stop
	// and .Wait are nil-safe when called before Start ever ran, so a
	// signal landing while Start is still spawning the subprocess, or
	// Start itself failing, can both safely call targetStop() through
	// this same closure.
	var stopOnce sync.Once
	var stopErr error
	targetStop := func() error {
		stopOnce.Do(func() { stopErr = target.Stop() })
		return stopErr
	}

	// Run's own signal/context watch (Pass 3.9c Final): unlike the
	// CLI-level WatchSignals cmd_guard.go/cmd_proxy.go use for their
	// pre-Run setup windows, this one calls Stop directly — Run has no
	// caller-supplied decision point to defer to, and it must trigger
	// targetStop() itself to unwind promptly, rather than just
	// recording a decision nothing then acts on.
	//
	// Registered HERE, before target.Start(ctx) below, not after it.
	// The CLI's own pre-Run watch (cmd/centrol's newSignalWatch) settles
	// and steps aside right before calling g.Run, but deliberately never
	// unregisters (see WatchSignals' doc comment) — so its channel is
	// still registered at the OS level, but its one-shot goroutine has
	// already made its final decision and exited; nothing will ever
	// read that channel again. That is harmless on its own (the signal
	// is still "caught" by the OS, so it cannot revert to default
	// disposition and kill the process), but it means a signal
	// delivered in the gap before some OTHER registration is actively
	// listening is invisible to the Governor's own Stop bookkeeping,
	// even though the OS's raw delivery (to the whole process group)
	// can still kill the target subprocess directly. target.Start can
	// take real, non-trivial time (it spawns a subprocess), so starting
	// it before this registration leaves exactly that gap open.
	// Reproduced directly: TestShipCriterion9_ProxySummaryPrintsOnSigint
	// got exit 1 ("signal: interrupt", an ordinary error surfaced from
	// Wait() because the raw signal killed the target directly) instead
	// of 130 — Stop was never called with the signal's real cause,
	// because nothing Governor-side was listening yet when it arrived,
	// so the CLI's own post-Run error path won the Stop race instead
	// with a plain "agent_run_error". Registering before Start closes
	// that gap: by the time Start can even begin, this watch is already
	// listening on the Governor's behalf.
	settleSignals, unregisterSignals := g.WatchSignals(func(cause string, code int) {
		g.Stop(cause, code)
		_ = targetStop()
	})
	// Run is this signal watch's last registration in its process —
	// nothing after Run returns still needs these signals handled on
	// Run's behalf, so there is no "someone else's registration" to
	// overlap with and no reason to withhold unregister the way
	// cmd/centrol's newSignalWatch must (see WatchSignals' doc comment
	// for why that distinction matters). settle first, then unregister,
	// same order either caller uses. Deferred immediately so it still
	// runs even if target.Start below fails and Run returns early.
	defer func() { settleSignals(); unregisterSignals() }()
	go func() {
		select {
		case <-ctx.Done():
			g.Stop("context_cancelled", 2)
			_ = targetStop()
		case <-runDone:
		}
	}()

	if err := target.Start(ctx); err != nil {
		return err
	}

	// Interceptor itself never touches clientOut (its job is evaluation
	// and logging, not I/O) — so Run supplies the one callback that needs
	// to, bound to the same mutex the two pump goroutines below already
	// share. This must be set before either goroutine starts: a
	// tools/call forwarded in the first few lines of the client pump can
	// already be arming its timeout by the time this function returns
	// from the caller's perspective, and OnTimeout must be ready by then.
	in.OnTimeout = func(id json.RawMessage, tool string, elapsed time.Duration) {
		resp, err := proxy.TimeoutErrorResponse(id, tool)
		if err != nil {
			return
		}
		clientOutMu.Lock()
		_ = proxy.WriteFrame(clientOut, resp)
		clientOutMu.Unlock()
	}

	// client -> target
	go func() {
		// Top-level recover (Pass 3.9c Final, Section 2 "Panics"): Go
		// does not propagate a panic across goroutines, so without this
		// a bug here would crash the whole process outside the
		// Governor's knowledge. Deferred first (runs last, after the
		// clientPumpDone send below) so it still catches a panic that
		// happens inside that same deferred send — recover only needs to
		// be called from some deferred function in the panicking frame,
		// not the first one.
		defer func() {
			if r := recover(); r != nil {
				g.Stop("panic", 2)
			}
		}()
		// Reports upward that this pump's own loop has ended, for
		// whatever reason — "no more input is coming." It does NOT call
		// targetStop() itself (Pass 3.9 Section 3): that decision belongs
		// to this function's own body (see the clientPumpDone-watching
		// goroutine below), not to the pump.
		defer func() { clientPumpDone <- struct{}{} }()
		clientLines, clientReadErr := proxy.ReadFrames(clientIn)
		for line := range clientLines {
			forward, fwdLine, blockResp, err := in.HandleClientRequest(line)
			if err != nil {
				clientToTargetErr <- fmt.Errorf("proxy: handling client request: %w", err)
				return
			}
			if forward {
				if err := target.Send(fwdLine); err != nil {
					clientToTargetErr <- fmt.Errorf("proxy: writing to target: %w", err)
					return
				}
				continue
			}
			if blockResp != nil {
				clientOutMu.Lock()
				err := proxy.WriteFrame(clientOut, blockResp)
				clientOutMu.Unlock()
				if err != nil {
					clientToTargetErr <- fmt.Errorf("proxy: writing block response to client: %w", err)
					return
				}
			}
		}
		// The channel is closed either because clientIn was exhausted
		// cleanly (normal EOF — e.g. the client hung up) or because the
		// scanner itself failed (oversized frame, read error). Only the
		// latter is a real failure: it means bytes on the wire were lost
		// without ever being evaluated or forwarded, which is exactly
		// what Protocol Silence exists to make visible rather than let
		// pass as an ordinary clean close.
		if rerr := clientReadErr(); rerr != nil {
			_ = in.Emit(in.RunID, "proxy", "policy.silence", map[string]interface{}{
				"reason": "stream read error", "stream": "clientIn", "error": rerr.Error(),
			})
			clientToTargetErr <- fmt.Errorf("proxy: reading from client: %w", rerr)
			return
		}
		clientToTargetErr <- nil
	}()

	// target -> client
	go func() {
		// Top-level recover (Pass 3.9c Final, Section 2 "Panics"): also
		// sends on targetToClientDone, since — unlike the client pump
		// above — nothing else in this goroutine unconditionally reports
		// completion; without this, a panic here would leave Run's
		// blocking `<-targetToClientDone` receive waiting forever.
		defer func() {
			if r := recover(); r != nil {
				g.Stop("panic", 2)
				targetToClientDone <- fmt.Errorf("proxy: panic in target->client pump: %v", r)
			}
		}()
		targetLines, err := target.Receive()
		if err != nil {
			targetToClientDone <- fmt.Errorf("proxy: receiving from target: %w", err)
			return
		}
		for line := range targetLines {
			fwdLine, err := in.HandleTargetResponse(line)
			if err != nil {
				targetToClientDone <- fmt.Errorf("proxy: handling target response: %w", err)
				return
			}
			if fwdLine == nil {
				// A late response to a call that already timed out (see
				// Interceptor.resolvePending): the client already has its
				// structured mcp_call_timeout error for this id, so this
				// frame is deliberately dropped, not forwarded.
				continue
			}
			clientOutMu.Lock()
			werr := proxy.WriteFrame(clientOut, fwdLine)
			clientOutMu.Unlock()
			if werr != nil {
				targetToClientDone <- fmt.Errorf("proxy: writing to client: %w", werr)
				return
			}
		}
		// Same distinction as the client -> target pump above: a clean
		// close here is the target exiting normally (the expected, common
		// case handled below via targetStop()); a non-nil error means the
		// target's own read stream failed mid-read. Not every RunTransport
		// distinguishes the two (see targetErrReporter), so this is an
		// optional check.
		if er, ok := target.(targetErrReporter); ok {
			if rerr := er.Err(); rerr != nil {
				_ = in.Emit(in.RunID, "proxy", "policy.silence", map[string]interface{}{
					"reason": "stream read error", "stream": "targetStdout", "error": rerr.Error(),
				})
				targetToClientDone <- fmt.Errorf("proxy: reading from target: %w", rerr)
				return
			}
		} else {
			diagOutMu.Lock()
			fmt.Fprintf(diagOut, "centrol: debug: target does not implement targetErrReporter — cannot distinguish a clean stream end from a read failure for this transport\n")
			diagOutMu.Unlock()
		}
		targetToClientDone <- nil
	}()

	// The sole trigger for the common case: the client pump ending
	// ("no more input is coming") is the earliest and most common point
	// at which shutdown can begin, matching the old client pump's own
	// defer — only now the decision to act on it lives in this function's
	// body, not inside the pump. A no-op (stopOnce) if the fallback below
	// already fired first.
	go func() {
		// Top-level recover (Pass 3.9c Final, Section 2 "Panics"). The
		// fallback targetStop() call after draining targetToClientDone
		// below still covers shutdown if this goroutine panics before
		// reaching its own targetStop() call.
		defer func() {
			if r := recover(); r != nil {
				g.Stop("panic", 2)
			}
		}()
		<-clientPumpDone
		_ = targetStop()
	}()

	// The target -> client goroutine's loop ends when its Receive channel
	// closes: for StdioTarget, that follows the process exiting, which
	// (if it reads until EOF) follows targetStop() above closing stdin;
	// for HTTPTarget, Stop() closes the channel directly. Run must not
	// return (and the caller must not treat clientOut as final) until
	// this goroutine actually has — it is the only other writer to
	// clientOut.
	drainErr := <-targetToClientDone

	// Fallback signal: guarantees targetStop() has fired even if the
	// client pump never reached EOF — e.g. the target ended on its own,
	// by clean exit or by error, while the client pump was still blocked
	// reading clientIn. A no-op (stopOnce) if the clientPumpDone-watching
	// goroutine above already fired.
	_ = targetStop()

	// Now that targetStop has signaled shutdown, wait for it to actually
	// finish, if this target has anything left to wait for beyond what
	// Stop itself already did (StdioTarget: reap the subprocess via
	// cmd.Wait()). Not every RunTransport has such a phase — a
	// request/response transport like HTTPTarget has nothing left to wait
	// for once its receive channel is closed — so this is an optional
	// check, same pattern as targetErrReporter above.
	var waitErr error
	if w, ok := target.(targetWaiter); ok {
		waitErr = w.Wait()
		// A signal sent to this process's own process group (a
		// foreground Ctrl+C, or a test's syscall.Kill(-pid, sig)) lands
		// on the target subprocess directly, via the OS, independent of
		// and concurrent with the Governor's own WatchSignals delivery
		// above. The two are racing: the kernel killing the child and
		// its stdout pipe closing can easily finish — and waitErr come
		// back — before the Go runtime's own signal-forwarding goroutine
		// has even delivered the signal to this process's sigCh, let
		// alone before onReceived has run and called Stop. No amount of
		// registering WatchSignals earlier closes that gap: it is not a
		// Go-code ordering problem, it is two different OS-level
		// deliveries (to two different processes in the same group)
		// with no ordering guarantee between them. Reproduced directly
		// under heavy scheduler contention even after WatchSignals was
		// moved ahead of target.Start (TestShipCriterion9_ProxySummary-
		// PrintsOnSigint still got exit 1, "signal: interrupt", instead
		// of 130).
		//
		// waitErr itself is definitive proof of what happened to the
		// target, synchronously, in-band — no need to wait on the
		// racy, asynchronous path at all. If it reports the target was
		// killed by a signal, that signal's conventional cause/code
		// (signalCause, same mapping WatchSignals uses) is recorded via
		// Stop right here, before this function ever returns. Stop is
		// first-call-wins and safe for concurrent callers (see its own
		// doc comment), so this never fights the WatchSignals path —
		// either this wins (the common case, since it is earlier and
		// synchronous) or WatchSignals already had, in which case this
		// call is simply a no-op.
		if sig, ok := signalFromWaitErr(waitErr); ok {
			cause, code := signalCause(sig)
			g.Stop(cause, code)
		}
	} else {
		diagOutMu.Lock()
		fmt.Fprintf(diagOut, "centrol: debug: target does not implement targetWaiter — nothing left to wait for once Stop has signaled shutdown, by design for this transport\n")
		diagOutMu.Unlock()
	}

	if drainErr != nil {
		return drainErr
	}

	// Symmetric to the drain above, but for mcp_call_timeout_seconds:
	// every timer armTimeout started must be accounted for — fired, or
	// canceled by a real response — before Run treats clientOut as final.
	in.WaitPendingTimeouts()

	// The client -> target direction has no such deadline in real usage
	// (a live MCP client's stdin may stay open long after the target it
	// was talking to exits), so this check stays non-blocking: pick up an
	// error if the goroutine already finished, but never wait on it here.
	select {
	case pumpErr := <-clientToTargetErr:
		if pumpErr != nil {
			return pumpErr
		}
	default:
	}

	if waitErr != nil {
		return waitErr
	}
	return stopErr
}
