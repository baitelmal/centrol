package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// hardExcludedAbs returns whether absPath falls under repoRoot/.centrol or
// repoRoot/.git. This check is UNCONDITIONAL and runs before .centrolignore
// or contract rules are evaluated; it cannot be overridden by user
// config. Without it, the ledger's own writes would re-trigger the
// watcher, which would write to the ledger again — an infinite loop
// that fills disk in seconds.
func hardExcludedAbs(repoRoot, absPath string) bool {
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return false
	}
	relSlash := filepath.ToSlash(rel)
	if relSlash == ".centrol" || strings.HasPrefix(relSlash, ".centrol/") {
		return true
	}
	if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
		return true
	}
	return false
}

// resolveNoEscape resolves symlinks in absPath (fsnotify follows
// symlinks by default) and reports whether the real location stays
// within repoRoot. Every path MUST be resolved to its real location
// before policy evaluation; a symlink pointing outside the repo root is
// refused, never followed.
func resolveNoEscape(repoRoot, absPath string) (real string, insideRoot bool, err error) {
	real, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		// The path may have just been deleted (fsnotify Remove events
		// race with EvalSymlinks); fall back to the lexical path so a
		// delete event still gets evaluated against the tree it lived in.
		real = filepath.Clean(absPath)
	}
	rootReal, rootErr := filepath.EvalSymlinks(repoRoot)
	if rootErr != nil {
		rootReal = repoRoot
	}
	rel, relErr := filepath.Rel(rootReal, real)
	if relErr != nil {
		return real, false, relErr
	}
	if rel == ".." || strings.HasPrefix(filepath.ToSlash(rel), "../") {
		return real, false, nil
	}
	return real, true, nil
}

// EmitFunc forwards one governed signal. Guard is decoupled from the
// concrete *governor.Governor type so tests can substitute a recorder.
type EmitFunc func(run, src, typ string, payload map[string]interface{}) error

// Watcher observes a repo root for filesystem changes during a run and
// forwards them through EmitFunc as fs.write / fs.create / fs.delete
// events. Policy comparison against the session contract happens at the
// call site that owns the contract (cmd/centrol wiring), not here, so this
// package stays focused on observation + the two mandatory safety rules.
type Watcher struct {
	repoRoot string
	runID    string
	ignore   *IgnoreSet
	fsw      *fsnotify.Watcher
	emit     EmitFunc
	debounce time.Duration // guard.watcher_debounce_ms, resolved by the caller; 0 disables coalescing entirely

	debounceMu sync.Mutex
	pending    map[string]*pendingFSEvent // keyed by repo-relative path; one in-flight debounce window per path
	debounceWG sync.WaitGroup             // outstanding debounce timers — Close waits for these to flush before returning

	// emitFailureMu/emitFailureStreak track CONSECUTIVE emit() failures
	// across every call site in this file (emitDebounced's immediate
	// and debounced paths, flushAllPending, scanExisting, handle's own
	// policy.violation emits) — audit fix (4b). Every emit() call used
	// to go out as `_ = w.emit(...)`, discarding its error entirely: a
	// guarded run whose ledger writes started silently failing (a full
	// disk, a permissions change mid-run) produced no record of ANY
	// filesystem activity for the rest of the run, with nothing printed
	// and nothing logged — the run would look clean when it wasn't
	// observing anything at all. Reset to 0 by any successful emit.
	emitFailureMu     sync.Mutex
	emitFailureStreak int
}

// emitFailureEscalateAt is how many consecutive emit() failures (see
// checkedEmit) trigger an escalation to a policy.silence ledger entry,
// rather than just a stderr warning. A single failure is often
// transient; three in a row across whatever this watcher is observing
// is treated as the ledger itself being unavailable for the rest of
// the run, which the run's own audit trail must reflect.
const emitFailureEscalateAt = 3

// checkedEmit wraps every call this file makes through EmitFunc.
// Unlike the discarded-error calls it replaces, a failure here always
// prints a warning to stderr (so a human watching the run sees it
// immediately, even though the Watcher has no logger of its own to
// gate output by --quiet/--verbose) and, once emitFailureEscalateAt
// consecutive failures have been observed across any combination of
// call sites, makes one best-effort attempt to record a policy.silence
// entry carrying the underlying error — so the run's own ledger, if it
// has recovered enough to accept writes again, shows that observation
// was degraded for part of the run, rather than silently resuming as
// if nothing had happened. checkedEmit never returns an error and
// never terminates the run: a broken ledger is not a reason to stop
// observing or to kill the wrapped agent.
func (w *Watcher) checkedEmit(typ string, payload map[string]interface{}) {
	err := w.emit(w.runID, "guard", typ, payload)
	if err == nil {
		w.emitFailureMu.Lock()
		w.emitFailureStreak = 0
		w.emitFailureMu.Unlock()
		return
	}

	fmt.Fprintf(os.Stderr, "centrol guard: warning: failed to log %s event: %v\n", typ, err)

	w.emitFailureMu.Lock()
	w.emitFailureStreak++
	streak := w.emitFailureStreak
	if streak >= emitFailureEscalateAt {
		w.emitFailureStreak = 0
	}
	w.emitFailureMu.Unlock()

	if streak >= emitFailureEscalateAt {
		// Best-effort: if the ledger is still unavailable, this fails
		// too and is silently dropped — there is nothing further to do
		// beyond the stderr warning already printed above for every
		// failure in the streak, including this one.
		_ = w.emit(w.runID, "guard", "policy.silence", map[string]interface{}{
			"reason":      "repeated watcher emit failures",
			"failed_type": typ,
			"error":       err.Error(),
		})
	}
}

// pendingFSEvent is the most recent event observed for one path during
// its current debounce window. Each new event for the same path
// overwrites typ/payload here and restarts the window (trailing-edge
// debounce): the emitted entry always carries the LAST event's shape,
// per spec ("the last event in a window determines the entry's
// payload").
type pendingFSEvent struct {
	typ     string
	payload map[string]interface{}
	timer   *time.Timer
}

// NewWatcher creates a Watcher rooted at repoRoot with debouncing
// disabled (every event emits immediately) and registers repoRoot plus
// every subdirectory not hard- or ignore-excluded. Existing callers
// (and tests) that have no opinion on watcher_debounce_ms keep their
// exact current behavior; centrol guard itself uses
// NewWatcherWithDebounce, resolving guard.watcher_debounce_ms first.
func NewWatcher(repoRoot, runID string, ignore *IgnoreSet, emit EmitFunc) (*Watcher, error) {
	return NewWatcherWithDebounce(repoRoot, runID, ignore, emit, 0)
}

// NewWatcherWithDebounce is NewWatcher with an explicit per-path
// debounce window: events for the SAME path arriving within debounce
// of each other are coalesced into a single emitted entry (the last
// event's payload wins); events for DIFFERENT paths are never coalesced
// with each other, however close together they arrive. debounce <= 0
// behaves exactly like NewWatcher (no coalescing).
func NewWatcherWithDebounce(repoRoot, runID string, ignore *IgnoreSet, emit EmitFunc, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		repoRoot: repoRoot, runID: runID, ignore: ignore, fsw: fsw, emit: emit,
		debounce: debounce, pending: map[string]*pendingFSEvent{},
	}
	if err := w.addRecursive(repoRoot); err != nil {
		fsw.Close()
		return nil, err
	}
	return w, nil
}

func (w *Watcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if hardExcludedAbs(w.repoRoot, path) {
			return filepath.SkipDir
		}
		rel, relErr := filepath.Rel(w.repoRoot, path)
		if relErr == nil && rel != "." && w.ignore.Match(rel) {
			return filepath.SkipDir
		}
		return w.fsw.Add(path)
	})
}

// Close stops the underlying fsnotify watcher, then flushes every
// still-pending debounce window immediately (rather than dropping it)
// and waits for those flush emits to complete before returning — a
// debounced write sitting in its coalescing window at the moment the
// wrapped agent exits must still make it into the ledger, not vanish
// because the window hadn't elapsed yet.
func (w *Watcher) Close() error {
	err := w.fsw.Close()
	w.flushAllPending()
	w.debounceWG.Wait()
	return err
}

// emitDebounced routes one fs.* observation through the per-path
// coalescing window (see NewWatcherWithDebounce): debounce <= 0 emits
// immediately, matching NewWatcher's undebounced behavior exactly.
// Otherwise, an event for a path that already has a pending window
// supersedes it (the superseded timer is canceled — see the Stop()
// accounting comment below — and its payload is replaced with this
// newer one), and a fresh window starts; the entry only actually emits
// once debounce elapses with no further event for that same path.
func (w *Watcher) emitDebounced(typ, relSlash string, payload map[string]interface{}) {
	if w.debounce <= 0 {
		w.checkedEmit(typ, payload)
		return
	}

	w.debounceMu.Lock()
	if old, ok := w.pending[relSlash]; ok {
		delete(w.pending, relSlash)
		w.debounceMu.Unlock()
		if old.timer.Stop() {
			// Canceled before it fired: its own AfterFunc body will
			// never run, so it will never call debounceWG.Done() —
			// this call must, on its behalf, exactly like
			// proxy.Interceptor.resolvePending's identical pattern.
			w.debounceWG.Done()
		}
		w.debounceMu.Lock()
	}

	pe := &pendingFSEvent{typ: typ, payload: payload}
	w.debounceWG.Add(1)
	pe.timer = time.AfterFunc(w.debounce, func() {
		// Whenever this closure actually runs, any Stop() call above
		// has already lost that race (Stop() returns false once a
		// timer has started firing) — so this closure unconditionally
		// owns and must discharge the Done() its Add(1) promised.
		defer w.debounceWG.Done()

		w.debounceMu.Lock()
		cur, ok := w.pending[relSlash]
		if !ok || cur != pe {
			// Superseded by a newer event for the same path (or
			// already flushed by Close) before this fired.
			w.debounceMu.Unlock()
			return
		}
		delete(w.pending, relSlash)
		w.debounceMu.Unlock()
		w.checkedEmit(cur.typ, cur.payload)
	})
	w.pending[relSlash] = pe
	w.debounceMu.Unlock()
}

// flushAllPending emits every still-pending debounced entry immediately,
// bypassing its remaining window, and cancels each entry's timer —
// called by Close so a run's teardown never silently drops a write that
// was still coalescing when the agent exited.
func (w *Watcher) flushAllPending() {
	for {
		w.debounceMu.Lock()
		var path string
		var pe *pendingFSEvent
		for p, e := range w.pending {
			path, pe = p, e
			break
		}
		if pe == nil {
			w.debounceMu.Unlock()
			return
		}
		delete(w.pending, path)
		w.debounceMu.Unlock()

		if pe.timer.Stop() {
			w.debounceWG.Done()
		}
		w.checkedEmit(pe.typ, pe.payload)
	}
}

// scanExisting walks a just-created directory (already registered with
// fsw.Add by addRecursive, called immediately before this) and emits a
// synthetic fs.create for every regular file already present in it.
// This closes the mkdir-then-immediate-write race described in handle:
// files are only missed if written before the watch registration, never
// after, and this runs strictly after that registration.
func (w *Watcher) scanExisting(root string) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if hardExcludedAbs(w.repoRoot, path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(w.repoRoot, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if w.ignore.Match(relSlash) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil // the directory's own Create event is handled by the caller; only pre-existing files inside it are this function's job
		}

		// Audit fix: handle() treats symlink-escape resolution as
		// mandatory before every emission (see its own doc comment); this
		// backfill reaches the ledger the same way handle() does, so a
		// symlink that raced into the directory before its watch was
		// registered must be refused identically, not emitted as a plain
		// fs.create.
		real, inside, resolveErr := resolveNoEscape(w.repoRoot, path)
		if resolveErr != nil {
			return nil
		}
		if !inside {
			w.checkedEmit("policy.violation", map[string]interface{}{
				"reason": "symlink escape: path resolves outside repo root",
				"path":   relSlash,
				"target": real,
			})
			return nil
		}

		w.checkedEmit("fs.create", map[string]interface{}{"path": relSlash})
		return nil
	})
}

// Run drains fsnotify events until the channel closes (i.e. until Close
// is called), applying the two mandatory safety rules before every
// emission: hard .centrol/.git exclusion, then symlink-escape resolution.
// Directories created mid-run are added to the watch set so nested
// activity is observed without requiring a native recursive watch.
func (w *Watcher) Run() {
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Watcher errors are operational noise, not governed events;
			// they are not part of the Pulse Semantics ledger contract.
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	absPath := ev.Name

	// MANDATORY, unconditional, before anything else.
	if hardExcludedAbs(w.repoRoot, absPath) {
		return
	}

	real, inside, err := resolveNoEscape(w.repoRoot, absPath)
	if err != nil {
		return
	}
	if !inside {
		// Symlink (or the path itself) resolves outside the repo root.
		// Refuse to follow it; record as a policy violation rather than
		// silently dropping it.
		w.checkedEmit("policy.violation", map[string]interface{}{
			"reason": "symlink escape: path resolves outside repo root",
			"path":   filepath.ToSlash(mustRel(w.repoRoot, absPath)),
			"target": real,
		})
		return
	}

	rel, err := filepath.Rel(w.repoRoot, real)
	if err != nil {
		return
	}
	relSlash := filepath.ToSlash(rel)
	if w.ignore.Match(relSlash) {
		return
	}

	// If a new directory appeared, start watching it too so nested files
	// are observed without a native recursive watch mode. Then scan for
	// anything that raced in between the directory being created and
	// the watch on it actually being registered — e.g. a script doing
	// `mkdir -p docs && echo x > docs/f.txt` back to back can create and
	// write a file before fsnotify's watch on the new directory is live.
	// The watch is registered first, then the scan catches anything
	// that slipped in before that registration; anything written after
	// registration is caught by the watch itself (possibly emitted
	// twice, once by each path — a harmless duplicate log line, unlike
	// the silent miss this closes).
	if ev.Op&fsnotify.Create != 0 {
		if info, statErr := os.Stat(real); statErr == nil && info.IsDir() {
			_ = w.addRecursive(real)
			w.scanExisting(real)
		}
	}

	var typ string
	switch {
	case ev.Op&fsnotify.Create != 0:
		typ = "fs.create"
	case ev.Op&fsnotify.Remove != 0:
		typ = "fs.delete"
	case ev.Op&fsnotify.Write != 0:
		typ = "fs.write"
	case ev.Op&fsnotify.Rename != 0:
		typ = "fs.delete" // the old name stops existing; the new name arrives as its own Create
	default:
		return
	}

	w.emitDebounced(typ, relSlash, map[string]interface{}{"path": relSlash})
}

func mustRel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return rel
}
