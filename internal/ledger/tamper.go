package ledger

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ownWriteGrace is how long after this ledger's own write completes
// (see (*Ledger).markOwnWrite) a filesystem event for one of its
// segment files is still attributed to that write rather than reported
// as external tampering.
//
// This is a grace window, not a start/stop flag, because fsnotify
// delivers events asynchronously relative to the write() syscall that
// produced them: the kernel mechanism behind it (inotify on Linux,
// kqueue on macOS/BSD, ReadDirectoryChangesW on Windows) generates the
// event essentially immediately, but it is only pulled off the watch
// descriptor and pushed onto fsnotify's Events channel once this
// process's own event-reading goroutine next gets scheduled — which can
// lag the write by anywhere from microseconds to low milliseconds under
// ordinary scheduler contention. A flag cleared synchronously the
// instant Append's write call returns would misreport an own write as
// tampering whenever that delivery lag outlives the flag.
//
// The window is deliberately kept to tens of milliseconds, not longer:
// it exists solely to absorb fsnotify's own delivery lag for THIS
// write, not to act as a general post-write cooldown during which any
// unrelated event — including a genuine external modification that
// happens to land moments after a legitimate Append — gets waved
// through. A wider window trades a real detection gap for no extra
// safety, since the delivery lag it needs to cover never approaches
// this scale in practice.
const ownWriteGrace = 50 * time.Millisecond

// TamperDetection is one externally caused modification observed on one
// of this ledger's own segment files — i.e., a write that did not pass
// through this process's own Append (see ownWriteGrace).
type TamperDetection struct {
	// Path is the absolute path to the segment file that changed.
	Path string
	// Event is "modify" (the file's content changed) or "attrib" (only
	// its metadata — permissions, ownership, timestamps — changed).
	Event string
	// DetectedAt is when this process observed the event, not when the
	// external write actually happened (fsnotify cannot tell us that).
	DetectedAt time.Time
}

// TamperWatcher observes one Ledger's own segment files — the active
// segment and any sealed (rotated) ones already on disk — for external
// modifications, for as long as a `centrol guard` or `centrol proxy`
// run keeps it open. See (*Ledger).WatchForTamper.
//
// Detection only. A TamperWatcher never reacts to what it finds — no
// permission change, no process termination, no sealing. Centrol's
// free tier gives you a verifiable record and tells you when it was
// tampered with while watching; actually preventing offline tampering
// (off-machine anchoring, managed verification) is enterprise-tier
// territory, not this type's job.
type TamperWatcher struct {
	fsw      *fsnotify.Watcher
	l        *Ledger
	onTamper func(TamperDetection)
	done     chan struct{}
}

// WatchForTamper starts observing l's own segment files for external
// modifications, reporting each one to onTamper exactly once. onTamper
// is called from this watcher's own goroutine — it must not block, and
// if it needs to touch the ledger itself (recording the detection as a
// policy.tamper_detected entry, as the Governor does — see
// (*Governor).WatchForTamper), that write is itself an Append, which
// marks its own completion as an own-write exactly like any other, so
// it is never mistaken for a second tamper event.
//
// l.dir always exists by the time a Ledger is constructed (Open creates
// it eagerly), so watching it here never races the directory's own
// creation. New segments created by a later rotation need no separate
// Add call: fsnotify watches the directory inode, not each file
// individually, so a sibling file created inside it is covered for
// free — handle filters by filename (isSegmentPath) rather than by
// which files existed when WatchForTamper was called.
func (l *Ledger) WatchForTamper(onTamper func(TamperDetection)) (*TamperWatcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("ledger: starting tamper watcher: %w", err)
	}
	if err := fsw.Add(l.dir); err != nil {
		fsw.Close()
		return nil, fmt.Errorf("ledger: starting tamper watcher: %w", err)
	}
	tw := &TamperWatcher{fsw: fsw, l: l, onTamper: onTamper, done: make(chan struct{})}
	go tw.run()
	return tw, nil
}

func (tw *TamperWatcher) run() {
	defer close(tw.done)
	for {
		select {
		case ev, ok := <-tw.fsw.Events:
			if !ok {
				return
			}
			tw.handle(ev)
		case _, ok := <-tw.fsw.Errors:
			if !ok {
				return
			}
			// Same stance as guard.Watcher.Run: a watcher-level error is
			// operational noise, not a governed event in its own right.
		}
	}
}

func (tw *TamperWatcher) handle(ev fsnotify.Event) {
	if !tw.l.isSegmentPath(ev.Name) {
		return // the lock file, meta.json, or some other sibling in the same directory — not this ledger's content
	}

	var kind string
	switch {
	case ev.Op&fsnotify.Write != 0:
		kind = "modify"
	case ev.Op&fsnotify.Chmod != 0:
		// Platform note (v0.3 stage 2): fsnotify.Chmod is backed by
		// inotify's IN_ATTRIB on Linux and kqueue's NOTE_ATTRIB on
		// macOS/BSD — both fire reliably for a permission, ownership,
		// or timestamp-only change with no content write. On Windows,
		// fsnotify's ReadDirectoryChangesW backend never produces a
		// Chmod event at all: its toFSnotifyFlags/newEvent translation
		// (backend_windows.go) maps FILE_ACTION_MODIFIED to Write only
		// and has no path to Chmod, because ReadDirectoryChangesW is
		// never asked to watch FILE_NOTIFY_CHANGE_ATTRIBUTES in the
		// first place (see that file's watch-flags construction). So an
		// attribute-only external change (e.g. a bare chmod with no
		// content write) goes undetected on Windows — the "modify" path
		// above still catches any attack that touches content, on every
		// platform including Windows; only the narrower attribute-only
		// case is Linux/macOS/BSD-only. This is a real gap in fsnotify
		// itself, not something centrol works around: per this stage's
		// scope, no Windows-specific workaround is built for it.
		kind = "attrib"
	default:
		// Create/Remove/Rename on a segment file is its own, separate
		// story (this ledger's own rotation, or a segment being deleted
		// out from under it) — detection-only tamper reporting is scoped
		// to IN_MODIFY/IN_ATTRIB per spec, not every possible event op.
		return
	}

	now := time.Now()
	if tw.l.isOwnWriteAt(now) {
		return // this ledger's own Append, not external tampering
	}
	tw.onTamper(TamperDetection{Path: ev.Name, Event: kind, DetectedAt: now})
}

// Close stops watching and waits for the watcher's own goroutine to
// drain before returning, so nothing is left running after Close
// returns — the same contract guard.Watcher.Close holds, for the same
// reason (a caller that just closed it should never see it fire again).
func (tw *TamperWatcher) Close() error {
	err := tw.fsw.Close()
	<-tw.done
	return err
}

// isSegmentPath reports whether absPath is one of this ledger's own
// segment files — the base file or a "<name>.NNN<ext>" rotation — as
// opposed to some other sibling file in the same directory (the lock
// file, meta.json, a snapshot directory, ...). Mirrors segments'
// own naming parse (ledger.go), just for one path instead of a
// directory listing.
func (l *Ledger) isSegmentPath(absPath string) bool {
	dir, name := filepath.Split(absPath)
	if filepath.Clean(dir) != filepath.Clean(l.dir) {
		return false
	}
	if name == l.name+l.ext {
		return true
	}
	prefix := l.name + "."
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, l.ext) {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, prefix), l.ext)
	var idx int
	_, err := fmt.Sscanf(mid, "%03d", &idx)
	return err == nil
}
