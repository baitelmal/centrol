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
	stopEarlySignal := earlySignalHandler(func(code int) {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
		os.Exit(code)
	})
	defer func() {
		if r := recover(); r != nil {
			printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
			clearMarker()
			fmt.Fprintf(os.Stderr, "centrol guard: panic: %v\n", r)
			os.Exit(ui.ExitRuntimeError)
		}
	}()

	ignore, err := guard.LoadIgnore(root)
	if err != nil {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
		fatalf("centrol guard: loading .centrolignore: %v", err)
	}

	snapDir := filepath.Join(snapshotsDir(root), runID)
	logger.Infof("centrol: snapshotting repo before run %s...\n", runID)
	if err := guard.Snapshot(root, snapDir, ignore); err != nil {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
		fatalf("centrol guard: snapshot failed, refusing to run ungoverned: %v", err)
	}

	contract := policy.DefaultContract(policy.KindGuard, runID, root)
	if len(scopePaths) > 0 {
		contract.AllowedPaths = scopePaths
	}
	gc := newGuardedContract(contract)

	observeFromConfig, _, cerr := policy.ResolveObserveMode(resolver)
	if cerr != nil {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
		fatalf("centrol guard: %v", cerr)
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
	// early-signal/panic handlers above can call it too. It's still not
	// a defer for the reason noted there: every exit path below calls
	// os.Exit (directly, or via fatalf), which skips deferred functions.

	_ = emit(runID, "guard", "run.start", map[string]interface{}{
		"agent": agentArgs[0], "args": agentArgs[1:], "snapshot": snapDir,
	})

	debounceMS, _, err := policy.ResolveWatcherDebounceMS(resolver)
	if err != nil {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
		fatalf("centrol guard: %v", err)
	}
	watcher, err := guard.NewWatcherWithDebounce(root, runID, ignore, contractAwareEmit(emit, gc, observe), time.Duration(debounceMS)*time.Millisecond)
	if err != nil {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
		fatalf("centrol guard: starting watcher: %v", err)
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
	// responsibility hands off to terminal.Run here, and the early
	// handler (for the narrower window before this point) steps aside.
	stopEarlySignal()

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
	clearMarker()

	// The run summary prints on every trappable exit from here: normal
	// completion, and the runErr path below (terminal.Run itself failed
	// to start/manage the child — distinct from the agent's own exit
	// code, which is exitCode and already reported above). Never gated
	// by --quiet: printRunSummary writes straight to os.Stderr, not
	// through logger.
	printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)

	if runErr != nil {
		fatalf("centrol guard: running agent: %v", runErr)
	}
	fmt.Fprintf(os.Stderr, "centrol: run %s finished (exit %d). Roll back with: centrol undo\n", runID, exitCode)
	os.Exit(exitCode)
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
