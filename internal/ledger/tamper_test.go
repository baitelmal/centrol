package ledger

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// waitForDetection blocks until the collector either receives n
// detections or deadline elapses, returning whatever was collected so
// far either way — tests assert on the result rather than failing
// inside the helper, so a timeout failure shows what actually arrived.
func waitForDetections(t *testing.T, ch <-chan TamperDetection, n int, deadline time.Duration) []TamperDetection {
	t.Helper()
	var got []TamperDetection
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for len(got) < n {
		select {
		case d := <-ch:
			got = append(got, d)
		case <-timer.C:
			return got
		}
	}
	return got
}

// assertNoDetection waits up to window for anything to arrive on ch and
// fails the test if it does — used to confirm the tool's own writes
// stay silent.
func assertNoDetection(t *testing.T, ch <-chan TamperDetection, window time.Duration) {
	t.Helper()
	select {
	case d := <-ch:
		t.Fatalf("unexpected tamper detection for the tool's own write: %+v", d)
	case <-time.After(window):
	}
}

func startTamperWatcher(t *testing.T, l *Ledger) <-chan TamperDetection {
	t.Helper()
	ch := make(chan TamperDetection, 16)
	tw, err := l.WatchForTamper(func(d TamperDetection) { ch <- d })
	if err != nil {
		t.Fatalf("WatchForTamper: %v", err)
	}
	t.Cleanup(func() { _ = tw.Close() })
	return ch
}

func TestTamperWatcherDetectsExternalModifyToActiveSegment(t *testing.T) {
	l := newTestLedger(t)
	mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{"agent": "x"})
	ch := startTamperWatcher(t, l)
	// Let the Append's own ownWriteGrace window lapse before modifying
	// the file externally: otherwise this write — not the external one
	// below — is what the grace window is covering, and the assertion
	// would no longer be exercising own-write-vs-external-write at all.
	time.Sleep(2 * ownWriteGrace)

	segPath := l.segmentPath(0)
	// Appending whitespace, not valid JSON: a real external modification
	// never happens to look like one of this ledger's own entries, and
	// this specifically must NOT break the hash chain (a trailing blank
	// line is skipped by Verify), since this test only cares whether the
	// MODIFY event itself is detected and reported.
	f, err := os.OpenFile(segPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	got := waitForDetections(t, ch, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("got %d detections, want 1", len(got))
	}
	if got[0].Path != segPath {
		t.Fatalf("Path = %q, want %q", got[0].Path, segPath)
	}
	if got[0].Event != "modify" {
		t.Fatalf("Event = %q, want %q", got[0].Event, "modify")
	}
	if got[0].DetectedAt.IsZero() {
		t.Fatal("DetectedAt is zero")
	}
}

func TestTamperWatcherDetectsExternalModifyToSealedSegment(t *testing.T) {
	l := newTestLedger(t)
	mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{"agent": "x"})

	// Force a rotation so a sealed segment (index 0) exists alongside
	// the new active one (index 1), then watch — this ledger's own
	// rotateAtBytes is large (100MB, from newTestLedger's Open call), so
	// rotation is driven directly instead of writing 100MB of entries.
	l.rotateAtBytes = 1
	mustAppend(t, l, "run-1", "guard", "fs.write", map[string]interface{}{"path": "a"})
	sealedPath := l.segmentPath(0)
	if _, err := os.Stat(sealedPath); err != nil {
		t.Fatalf("expected a sealed segment at %s: %v", sealedPath, err)
	}

	ch := startTamperWatcher(t, l)
	time.Sleep(2 * ownWriteGrace)

	f, err := os.OpenFile(sealedPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	got := waitForDetections(t, ch, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("got %d detections, want 1", len(got))
	}
	if got[0].Path != sealedPath {
		t.Fatalf("Path = %q, want %q", got[0].Path, sealedPath)
	}
	if got[0].Event != "modify" {
		t.Fatalf("Event = %q, want %q", got[0].Event, "modify")
	}
}

func TestTamperWatcherDetectsExternalAttribChange(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("fsnotify never reports Chmod on Windows; see tamper.go")
	}
	l := newTestLedger(t)
	mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{"agent": "x"})
	ch := startTamperWatcher(t, l)
	time.Sleep(2 * ownWriteGrace)

	segPath := l.segmentPath(0)
	if err := os.Chmod(segPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	got := waitForDetections(t, ch, 1, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("got %d detections, want 1: %+v", len(got), got)
	}
	if got[0].Event != "attrib" {
		t.Fatalf("Event = %q, want %q", got[0].Event, "attrib")
	}
}

func TestTamperWatcherIgnoresTheLedgersOwnWrites(t *testing.T) {
	l := newTestLedger(t)
	ch := startTamperWatcher(t, l)

	// A burst of real Append calls — including a forced rotation, so
	// the seal+rotate housekeeping writes (ledger.go's own internal
	// writes, not just the "requested" entry) are covered too — none of
	// which should ever be reported as tampering.
	l.rotateAtBytes = 1
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			mustAppend(t, l, "run-1", "guard", "fs.write", map[string]interface{}{"path": "f", "n": n})
		}(i)
	}
	wg.Wait()

	assertNoDetection(t, ch, 1*time.Second)
}

func TestTamperWatcherIgnoresSiblingFilesInTheSameDirectory(t *testing.T) {
	l := newTestLedger(t)
	mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{"agent": "x"})
	ch := startTamperWatcher(t, l)

	// .meta.json and the lock file live in the exact same directory as
	// the segment files; a watcher that didn't filter by filename would
	// misreport ordinary ledger bookkeeping as tampering.
	siblings := []string{
		filepath.Join(l.dir, ".lighthouse.meta.json"),
		filepath.Join(l.dir, "not-a-segment.txt"),
	}
	for _, p := range siblings {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	assertNoDetection(t, ch, 1*time.Second)
}

func TestIsSegmentPathRecognizesBaseAndRotatedOnly(t *testing.T) {
	l := newTestLedger(t)
	cases := []struct {
		path string
		want bool
	}{
		{l.segmentPath(0), true},
		{l.segmentPath(1), true},
		{l.segmentPath(42), true},
		{filepath.Join(l.dir, ".lighthouse.lock"), false},
		{filepath.Join(l.dir, ".lighthouse.meta.json"), false},
		{filepath.Join(l.dir, "lighthouse.jsonl.centrol-backup-20261001"), false},
		{filepath.Join(l.dir, "notlighthouse.jsonl"), false},
		{filepath.Join(t.TempDir(), "lighthouse.jsonl"), false}, // right name, wrong directory
	}
	for _, c := range cases {
		if got := l.isSegmentPath(c.path); got != c.want {
			t.Errorf("isSegmentPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
