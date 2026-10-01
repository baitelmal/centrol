package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scirem/centrol/internal/guard"
	"github.com/scirem/centrol/internal/policy"
	"github.com/scirem/centrol/internal/session"
	"github.com/scirem/centrol/internal/terminal"
	"github.com/scirem/centrol/internal/ui"
)

// cmdGuard implements `centrol guard -- <agent command>`. Zero flags
// are required: it snapshots the repo, watches it while the wrapped
// command runs, and lets `centrol undo` roll the run back afterward.
func cmdGuard(args []string) {
	scopePaths, observeFlag, quietFlag, verboseFlag, rest := extractGuardFlags(args)
	agentArgs, err := splitAfterDoubleDash(rest)
	if err != nil {
		fatalf("centrol guard: %v", err)
	}
	if len(agentArgs) == 0 {
		fatalf("centrol guard: usage: centrol guard [--scope <path>]... [--observe] [--quiet|--verbose] -- <agent command>")
	}

	root, err := repoRoot()
	if err != nil {
		fatalf("centrol guard: %v", err)
	}
	g, err := openGovernorAt(root)
	if err != nil {
		fatalf("centrol guard: %v", err)
	}
	emit := emitFunc(g)

	resolver := newResolver(root)
	logLevel := resolveLogLevel(resolver, quietFlag, verboseFlag)
	logger := ui.NewLogger(os.Stderr, logLevel)

	runID := newRunID()
	started := time.Now()
	counters := newRunCounters()
	// Tallying happens on every emission regardless of --quiet/--verbose
	// (order between the two wraps doesn't matter — both simply forward
	// to inner), so the run summary's numbers are never affected by the
	// logging flags.
	emit = counters.wrap(emit)
	// Every governed emission is also traced to stderr at debug level
	// (--verbose) — the "internal state transitions" the spec calls
	// for, without threading a logger through guard/policy/governor.
	emit = debugTraceEmit(logger, emit)

	// clearMarker and the summary/signal machinery are defined before
	// the run marker even exists (and before snapshotting) so a signal
	// or panic in this early window — before terminal.Run's own signal
	// forwarding takes over — still prints a summary and exits cleanly
	// rather than leaving a stale "active run" marker with no
	// explanation. session.ClearCurrentRun on a marker that was never
	// written is a safe no-op.
	clearMarker := func() {
		if err := session.ClearCurrentRun(centrolDir(root)); err != nil {
			fmt.Fprintf(os.Stderr, "centrol guard: warning: could not clear run marker: %v\n", err)
		}
	}
	// end is the one run-scoped "end the run" decision point every exit
	// site below calls through, from here (runID acquisition) onward
	// (Pass 3.9c Final) — see its doc comment in summary.go for the
	// win/lose contract. It is also the only thing that ever calls
	// Governor.Exit for this run.
	end := endRun(g, runID, ledgerPath(root), started, counters, clearMarker)
	// ctx/signalOutcome cover the window up to terminal.Run taking over
	// (Pass 3.9c Final correction): every blocking setup step below runs
	// through runSetup, which blocks THIS goroutine (main's own) on a
	// select against ctx, rather than ever letting the signal watch
	// itself call end/Exit from its own goroutine — see newSignalWatch's
	// and runSetup's doc comments in summary.go for why that distinction
	// matters. Once the wrapped agent is actually running, a real
	// SIGINT/SIGTERM/SIGHUP reaches centrol and the agent's process
	// group simultaneously, and terminal.Run's own forwarding already
	// produces the right summary + exit code via the ordinary
	// end-of-function path below — so this watch steps aside there
	// instead of racing it.
	ctx, signalOutcome, stopSignalWatch := newSignalWatch(g)
	defer func() {
		if r := recover(); r != nil {
			end("panic", ui.ExitRuntimeError, func() {
				fmt.Fprintf(os.Stderr, "centrol guard: panic: %v\n", r)
			})
		}
	}()

	var ignore *guard.IgnoreSet
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		ignore, e = guard.LoadIgnore(root)
		return e
	}); err != nil {
		end("load_ignore_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol guard: loading .centrolignore: %v\n", err)
		})
		return
	}

	snapDir := filepath.Join(snapshotsDir(root), runID)
	logger.Infof("centrol: snapshotting repo before run %s...\n", runID)
	if err := runSetup(ctx, end, signalOutcome, func() error {
		return guard.Snapshot(root, snapDir, ignore)
	}); err != nil {
		// Race (b) (Pass 3.9 Section 5): a SIGTERM/SIGINT that lands here
		// can kill git's own subprocess directly (it shares centrol's
		// process group), and runSetup's own ctx-cancellation branch
		// above usually wins this race first — but if git's subprocess
		// happens to die and report back before that branch fires,
		// signalCauseFromErr still recognizes it and normalizes to the
		// same cause/code newSignalWatch would have reported, so
		// whichever of the two actually calls end() first, the run's
		// recorded outcome agrees.
		if cause, code, ok := signalCauseFromErr(err); ok {
			end(cause, code, nil)
			return
		}
		end("snapshot_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol guard: snapshot failed, refusing to run ungoverned: %v\n", err)
		})
		return
	}

	contract := policy.DefaultContract(policy.KindGuard, runID, root)
	if len(scopePaths) > 0 {
		contract.AllowedPaths = scopePaths
	}
	gc := newGuardedContract(contract)

	// Audit fix: proxy.additional_block_paths was wired into `centrol
	// proxy`'s contract (see cmd_proxy.go) but never resolved here, so a
	// path an operator configured as an additional protected path was
	// silently unenforced under `centrol guard` even though both
	// surfaces share the same Contract/EvaluateFSPath machinery. Same
	// resolve-once-at-setup, canonicalize-once pattern as cmd_proxy.go.
	var additionalBlockPaths []string
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		additionalBlockPaths, _, e = policy.ResolveAdditionalBlockPaths(resolver)
		return e
	}); err != nil {
		end("resolve_additional_block_paths_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol guard: %v\n", err)
		})
		return
	}
	if len(additionalBlockPaths) > 0 {
		contract.ProtectedPaths = append(contract.ProtectedPaths, policy.CanonicalizeBlockPaths(additionalBlockPaths)...)
		gc = newGuardedContract(contract)
	}

	var observeFromConfig bool
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		observeFromConfig, _, e = policy.ResolveObserveMode(resolver)
		return e
	}); err != nil {
		end("resolve_observe_mode_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol guard: %v\n", err)
		})
		return
	}
	observe := observeFlag || observeFromConfig
	if observe {
		logger.Infof("centrol: observe mode — evaluating policy, never prompting, never blocking\n")
	}

	if err := session.WriteCurrentRun(centrolDir(root), session.RunMarker{
		RunID: runID, Kind: "guard", RepoRoot: root, StartedAt: time.Now().UTC(),
		SnapshotDir: snapDir, AllowedPaths: contract.AllowedPaths,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "centrol guard: warning: could not write run marker: %v\n", err)
	}
	// clearMarker itself is defined earlier (before snapshotting) so the
	// signal watch/panic handlers above can call it too. It's still not
	// a defer for the reason noted there: every exit path below ends in
	// Governor.Exit (via end()), which calls os.Exit and so skips
	// deferred functions.

	_ = emit(runID, "guard", "run.start", map[string]interface{}{
		"agent": agentArgs[0], "args": agentArgs[1:], "snapshot": snapDir,
	})

	var debounceMS int
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		debounceMS, _, e = policy.ResolveWatcherDebounceMS(resolver)
		return e
	}); err != nil {
		end("resolve_watcher_debounce_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol guard: %v\n", err)
		})
		return
	}
	var watcher *guard.Watcher
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		watcher, e = guard.NewWatcherWithDebounce(root, runID, ignore, contractAwareEmit(emit, gc, observe), time.Duration(debounceMS)*time.Millisecond)
		return e
	}); err != nil {
		end("start_watcher_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol guard: starting watcher: %v\n", err)
		})
		return
	}
	watcherDone := make(chan struct{})
	go func() { watcher.Run(); close(watcherDone) }()

	stopScope := make(chan struct{})
	go pollScopeAmendments(centrolDir(root), runID, gc, emit, stopScope)

	// From here on, a real terminal-generated SIGINT/SIGTERM/SIGHUP hits
	// this process and the wrapped agent's process group simultaneously;
	// terminal.Run installs its own signal.Notify and forwards to the
	// child, then returns the signal-derived exit code once the child
	// dies (128+sig) rather than centrol itself being killed. So signal
	// responsibility hands off to terminal.Run here, and the Governor's
	// own signal watch (for the narrower window before this point) steps
	// aside.
	stopSignalWatch()
	// checkCancelled AFTER stopSignalWatch, not before: stopSignalWatch
	// (newSignalWatch's own stop, cmd/centrol/summary.go — which settles
	// the underlying WatchSignals watch but deliberately never
	// unregisters it; see that doc comment) blocks until the watch's
	// goroutine has made its final, settled decision about any signal
	// it may have received, so this check sees the complete truth for
	// the entire pre-Run window — including a signal landing in the
	// plain synchronous stretch since the last runSetup call above
	// (watcher/pollScopeAmendments startup, nothing ctx.Done() could
	// otherwise interrupt), AND a signal arriving in the exact instant
	// of this handoff itself. Checking before stopSignalWatch returned
	// used to miss that second case: a signal this watch itself had
	// already consumed, but not yet finished processing, could still
	// call onReceived/cancel() strictly after this check had already
	// run and after cmd_guard had already moved on into terminal.Run —
	// a real SIGINT, swallowed by this watch, with no reader left for
	// the cancellation it eventually produced. See
	// Governor.WatchSignals' doc comment in internal/governor/run.go
	// for the fix (and for why this watch's registration is left
	// active rather than unregistered here).
	checkCancelled(end, signalOutcome)

	exitCode, runErr := terminal.Run(agentArgs[0], agentArgs[1:], nil)

	close(stopScope)
	// Grace period before tearing down the watcher: the agent process
	// has already exited, but the kernel may still have filesystem
	// events queued that fsnotify's own reader goroutine hasn't yet
	// pulled off the fd and pushed onto its Events channel. Closing the
	// watcher immediately can drop those — a real gap this fixes rather
	// than a defensive guess (see guard's own tests for the race this
	// closes). watcher.Close() then unblocks watcher.Run()'s select via
	// its channels closing, and watcherDone confirms it actually drained
	// before we log run.end.
	time.Sleep(400 * time.Millisecond)
	_ = watcher.Close()
	<-watcherDone

	endPayload := map[string]interface{}{"exit_code": exitCode}
	if runErr != nil {
		endPayload["error"] = runErr.Error()
	}
	_ = emit(runID, "guard", "run.end", endPayload)

	// end prints the run summary and clears the marker on every
	// trappable exit from here: normal completion, and the runErr branch
	// below (terminal.Run itself failed to start/manage the child —
	// distinct from the agent's own exit code, which is exitCode and
	// already reported above). Never gated by --quiet: printRunSummary
	// writes straight to os.Stderr, not through logger.
	if runErr != nil {
		end("agent_run_error", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol guard: running agent: %v\n", runErr)
		})
		return
	}
	end("agent_exit", exitCode, func() {
		fmt.Fprintf(os.Stderr, "centrol: run %s finished (exit %d). Roll back with: centrol undo\n", runID, exitCode)
	})
}

// splitAfterDoubleDash returns everything after a literal "--" in args.
// Guard takes no flags of its own in v0.1, so anything before "--" is
// rejected rather than silently ignored.
func splitAfterDoubleDash(args []string) ([]string, error) {
	for i, a := range args {
		if a == "--" {
			return args[i+1:], nil
		}
	}
	return nil, fmt.Errorf(`missing "--" separator before the agent command`)
}

// extractGuardFlags pulls every "--scope <path>" pair (repeatable) and
// the bare "--observe"/"--quiet"/"--verbose" flags out of args,
// returning them plus the remaining args untouched. This only ever
// looks at args BEFORE the "--" separator (splitAfterDoubleDash runs on
// what's left afterward) — anything after "--" belongs to the wrapped
// agent command and must never be touched, even if the agent happens to
// have its own --verbose flag of its own. Omitting --scope keeps the
// honest default (the whole repo); omitting --observe keeps normal
// enforcement; omitting --quiet/--verbose keeps the configured (or
// default "info") log level.
func extractGuardFlags(args []string) (scopePaths []string, observe, quiet, verbose bool, rest []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			// Everything from here on belongs to the wrapped agent
			// command and must pass through completely untouched —
			// even if the agent happens to have its own --observe,
			// --quiet, or --verbose flag.
			rest = append(rest, args[i:]...)
			break
		}
		switch {
		case args[i] == "--scope" && i+1 < len(args):
			scopePaths = append(scopePaths, args[i+1])
			i++
		case strings.HasPrefix(args[i], "--scope="):
			scopePaths = append(scopePaths, strings.TrimPrefix(args[i], "--scope="))
		case args[i] == "--observe":
			observe = true
		case args[i] == "--quiet":
			quiet = true
		case args[i] == "--verbose":
			verbose = true
		default:
			rest = append(rest, args[i])
		}
	}
	return scopePaths, observe, quiet, verbose, rest
}
