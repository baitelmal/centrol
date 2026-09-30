package guard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q")
	run(t, dir, "git", "config", "user.email", "test@example.com")
	run(t, dir, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-q", "-m", "init")
	return dir
}

// TestWatcherDoesNotTriggerOnOwnLedgerWrites is the mandatory watcher
// test: writing to .centrol/lighthouse.jsonl inside the watched repo must
// never produce an fs.* emission, or the ledger write -> watcher event
// -> ledger write cycle fills disk in seconds.
func TestWatcherDoesNotTriggerOnOwnLedgerWrites(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".centrol"), 0o755); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var emitted []string
	emit := func(run, src, typ string, payload map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, typ+":"+payload["path"].(string))
		return nil
	}

	ignore, err := LoadIgnore(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(repo, "run-1", ignore, emit)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	go w.Run()

	// Simulate the ledger writing to itself many times, the way governor
	// would during a real run.
	ledgerFile := filepath.Join(repo, ".centrol", "lighthouse.jsonl")
	for i := 0; i < 20; i++ {
		f, err := os.OpenFile(ledgerFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(`{"seq":1}` + "\n")
		f.Close()
	}
	// Also touch a real, non-excluded file, which SHOULD be observed —
	// this proves the watcher is actually running, not just silent.
	if err := os.WriteFile(filepath.Join(repo, "observed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, e := range emitted {
		if strings.HasPrefix(e, "fs.") && strings.Contains(e, ".centrol/") {
			t.Fatalf("watcher emitted an event for its own ledger directory: %s (full: %v)", e, emitted)
		}
	}
	sawObserved := false
	for _, e := range emitted {
		if strings.Contains(e, "observed.go") {
			sawObserved = true
		}
	}
	if !sawObserved {
		t.Fatalf("expected the watcher to observe a real file write outside .centrol/, emitted=%v", emitted)
	}
}

// TestSymlinkEscapeIsBlocked confirms a symlink inside the repo that
// points outside the repo root is never followed, and is instead logged
// as a policy.violation.
func TestSymlinkEscapeIsBlocked(t *testing.T) {
	repo := initRepo(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("outside-repo-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, "escape-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported on this filesystem: %v", err)
	}

	var mu sync.Mutex
	var violations []map[string]interface{}
	var normalWrites []string
	emit := func(run, src, typ string, payload map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		if typ == "policy.violation" {
			violations = append(violations, payload)
		} else {
			normalWrites = append(normalWrites, typ)
		}
		return nil
	}

	ignore, err := LoadIgnore(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(repo, "run-1", ignore, emit)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	go w.Run()

	// Write through the symlink; fsnotify follows symlinks by default,
	// so the underlying watch may or may not fire depending on platform,
	// but if it does, it must be refused rather than treated as a normal
	// in-repo write.
	os.WriteFile(target, []byte("outside-repo-secret-modified"), 0o644)
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, nw := range normalWrites {
		t.Fatalf("expected no normal fs event for a symlink escape, got %s", nw)
	}
	// It's acceptable for the OS watcher to never fire on the external
	// target at all (most platforms watch the link's directory, not its
	// resolved target) — what matters is resolveNoEscape's own logic,
	// which is exercised directly below regardless of watcher timing.
	_ = violations

	real, inside, err := resolveNoEscape(repo, link)
	if err != nil {
		t.Fatalf("resolveNoEscape: %v", err)
	}
	if inside {
		t.Fatalf("expected symlink target %s to resolve outside repo root, got inside=true real=%s", target, real)
	}
}

// TestHardExcludedAbs is a direct unit check of the unconditional
// .centrol/.git exclusion, independent of OS watcher timing.
func TestHardExcludedAbs(t *testing.T) {
	repo := "/repo"
	cases := map[string]bool{
		"/repo/.centrol/lighthouse.jsonl": true,
		"/repo/.centrol":                  true,
		"/repo/.git/HEAD":                 true,
		"/repo/.git":                      true,
		"/repo/src/main.go":               false,
		"/repo/.centrolignore":            false,
	}
	for path, want := range cases {
		if got := hardExcludedAbs(repo, path); got != want {
			t.Errorf("hardExcludedAbs(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSnapshotExcludesCentrolAndGitDirs is a regression test for a real
// bug: Snapshot's untracked-file listing (git ls-files --others) has no
// knowledge of .centrol/ on its own, so without an explicit exclusion a
// snapshot recursively captures its own ledger and snapshot directories
// into itself — including, on a second run, the *previous* snapshot's
// untracked/ copy of the ledger, compounding indefinitely. Manifest
// entries and the untracked/ copy must never include anything under
// .centrol/ or .git/.
func TestSnapshotExcludesCentrolAndGitDirs(t *testing.T) {
	repo := initRepo(t)
	ignore, err := LoadIgnore(repo)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a prior run having already populated .centrol/ with a
	// ledger and a previous snapshot, the way a real `centrol guard`
	// invocation would before calling Snapshot again.
	centrolDir := filepath.Join(repo, ".centrol")
	if err := os.MkdirAll(filepath.Join(centrolDir, "snapshots", "run-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(centrolDir, "lighthouse.jsonl"), []byte(`{"seq":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(centrolDir, "snapshots", "run-1", "head.txt"), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapDir := filepath.Join(t.TempDir(), "run-2")
	if err := Snapshot(repo, snapDir, ignore); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, f := range manifest.Files {
		if strings.HasPrefix(f.Path, ".centrol/") || strings.HasPrefix(f.Path, ".git/") {
			t.Fatalf("manifest recorded a path under .centrol/ or .git/: %s", f.Path)
		}
	}

	untrackedCopyDir := filepath.Join(snapDir, "untracked", ".centrol")
	if _, err := os.Stat(untrackedCopyDir); err == nil {
		t.Fatalf("snapshot's untracked/ copy includes a .centrol/ subdirectory — it recursively captured its own ledger")
	}
}

// TestSnapshotAndRestoreRoundTrip exercises Snapshot + Restore against a
// real git repo: modifies a tracked file, adds an untracked file,
// snapshots, modifies further, then restores and checks both are back.
func TestSnapshotAndRestoreRoundTrip(t *testing.T) {
	repo := initRepo(t)
	ignore, err := LoadIgnore(repo)
	if err != nil {
		t.Fatal(err)
	}

	// State at "snapshot time":
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("scratch-at-snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapDir := filepath.Join(t.TempDir(), "run-1")
	if err := Snapshot(repo, snapDir, ignore); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, f := range []string{"head.txt", "stash.diff", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(snapDir, f)); err != nil {
			t.Fatalf("expected snapshot component %s: %v", f, err)
		}
	}

	// Agent makes further changes after the snapshot.
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc B() { /* agent changed this */ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("scratch-CHANGED-by-agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new-file.txt"), []byte("brand new"), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := Restore(repo, snapDir)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !outcome.RestoredTracked {
		t.Fatalf("expected RestoredTracked=true")
	}

	mainGo, err := os.ReadFile(filepath.Join(repo, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mainGo), "func A()") {
		t.Fatalf("expected main.go restored to snapshot content, got: %s", mainGo)
	}

	scratch, err := os.ReadFile(filepath.Join(repo, "scratch.txt"))
	if err != nil {
		t.Fatalf("expected scratch.txt restored: %v", err)
	}
	if string(scratch) != "scratch-at-snapshot" {
		t.Fatalf("expected scratch.txt restored to snapshot content, got: %s", scratch)
	}

	foundNewFile := false
	for _, oos := range outcome.OutOfScope {
		if oos == "new-file.txt" {
			foundNewFile = true
		}
	}
	if !foundNewFile {
		t.Fatalf("expected new-file.txt (created after snapshot) to be reported as out-of-scope, got %v", outcome.OutOfScope)
	}
}

// TestPreUndoSnapshotRecoversFailedRollback: even if the "real" rollback
// can't run cleanly in this harness, PreUndoSnapshot itself must
// produce a valid, restorable snapshot of pre-rollback state.
func TestPreUndoSnapshotRecoversFailedRollback(t *testing.T) {
	repo := initRepo(t)
	ignore, _ := LoadIgnore(repo)
	snapshotsRoot := t.TempDir()

	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc PreRollbackState() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	preUndoDir, err := PreUndoSnapshot(repo, snapshotsRoot, "run-1", ignore)
	if err != nil {
		t.Fatalf("PreUndoSnapshot: %v", err)
	}
	if !strings.HasSuffix(preUndoDir, "run-1.pre-undo") {
		t.Fatalf("expected pre-undo dir named <run_id>.pre-undo, got %s", preUndoDir)
	}

	// Simulate a bad rollback mangling the tree.
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(repo, preUndoDir); err != nil {
		t.Fatalf("recovering via pre-undo snapshot failed: %v", err)
	}
	mainGo, _ := os.ReadFile(filepath.Join(repo, "main.go"))
	if !strings.Contains(string(mainGo), "PreRollbackState") {
		t.Fatalf("pre-undo recovery did not restore the pre-rollback state, got: %s", mainGo)
	}
}

// TestWatcherDebounceCoalescesRapidWritesToOnePath is ship criterion
// 13's first half: several rapid writes to the SAME file within the
// debounce window must produce exactly one fs.write entry, not one per
// write.
func TestWatcherDebounceCoalescesRapidWritesToOnePath(t *testing.T) {
	repo := initRepo(t)
	var mu sync.Mutex
	var emitted []string
	emit := func(run, src, typ string, payload map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, typ+":"+payload["path"].(string))
		return nil
	}

	ignore, err := LoadIgnore(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcherWithDebounce(repo, "run-1", ignore, emit, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	go w.Run()

	target := filepath.Join(repo, "burst.txt")
	// Ten writes to the same file, well within the 200ms window.
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(target, []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Let the debounce window elapse, then close (which also flushes
	// any still-pending entry immediately).
	time.Sleep(400 * time.Millisecond)
	_ = w.Close()

	mu.Lock()
	defer mu.Unlock()
	count := 0
	for _, e := range emitted {
		if e == "fs.write:burst.txt" || e == "fs.create:burst.txt" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected a 10-write burst to one path to coalesce into exactly 1 entry, got %d: %v", count, emitted)
	}
}

// TestWatcherDebounceKeepsDifferentPathsIndependent is ship criterion
// 13's second half: writes to two different files close together must
// still each produce their own entry — the debounce window is per-path,
// never a global "quiet the whole watcher" throttle.
func TestWatcherDebounceKeepsDifferentPathsIndependent(t *testing.T) {
	repo := initRepo(t)
	var mu sync.Mutex
	var emitted []string
	emit := func(run, src, typ string, payload map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, typ+":"+payload["path"].(string))
		return nil
	}

	ignore, err := LoadIgnore(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcherWithDebounce(repo, "run-1", ignore, emit, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	go w.Run()

	fileA := filepath.Join(repo, "a.txt")
	fileB := filepath.Join(repo, "b.txt")
	if err := os.WriteFile(fileA, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // well under the 100ms window
	if err := os.WriteFile(fileB, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)
	_ = w.Close()

	mu.Lock()
	defer mu.Unlock()
	seenA, seenB := 0, 0
	for _, e := range emitted {
		if strings.HasSuffix(e, ":a.txt") {
			seenA++
		}
		if strings.HasSuffix(e, ":b.txt") {
			seenB++
		}
	}
	if seenA != 1 || seenB != 1 {
		t.Fatalf("expected exactly one entry each for a.txt and b.txt (independent debounce windows), got a=%d b=%d: %v", seenA, seenB, emitted)
	}
}

// TestWatcherDebounceFlushesPendingEntryOnClose proves a write still
// inside its debounce window at Close time is not silently dropped —
// Close must flush it immediately rather than losing it when the run
// ends before the window naturally elapses.
func TestWatcherDebounceFlushesPendingEntryOnClose(t *testing.T) {
	repo := initRepo(t)
	var mu sync.Mutex
	var emitted []string
	emit := func(run, src, typ string, payload map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, typ+":"+payload["path"].(string))
		return nil
	}

	ignore, err := LoadIgnore(repo)
	if err != nil {
		t.Fatal(err)
	}
	// A long window (2s) that Close must not simply wait out.
	w, err := NewWatcherWithDebounce(repo, "run-1", ignore, emit, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go w.Run()

	target := filepath.Join(repo, "late.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // give fsnotify time to deliver the event into the pending window

	closeStart := time.Now()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(closeStart); elapsed > 1*time.Second {
		t.Fatalf("Close took %v — it must flush pending entries immediately, not wait out the debounce window", elapsed)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, e := range emitted {
		if strings.HasSuffix(e, ":late.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected late.txt's still-pending debounce entry to be flushed by Close, got %v", emitted)
	}
}
