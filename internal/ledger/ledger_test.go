package ledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "lighthouse.jsonl"), 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

func mustAppend(t *testing.T, l *Ledger, run, src, typ string, payload map[string]interface{}) Entry {
	t.Helper()
	res, err := l.Append(run, src, typ, payload, time.Time{})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return res.Entry
}

func TestAppendAndVerifyCleanChain(t *testing.T) {
	l := newTestLedger(t)
	mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{"agent": "claude-code"})
	mustAppend(t, l, "run-1", "guard", "fs.write", map[string]interface{}{"path": "main.go"})
	mustAppend(t, l, "run-1", "proxy", "tool.call", map[string]interface{}{"tool": "read_file"})
	mustAppend(t, l, "run-1", "guard", "run.end", map[string]interface{}{"exit_code": 0})

	res, err := l.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected clean chain, got failure: %+v", res.FailedAt)
	}
	if res.EntryCount != 4 {
		t.Fatalf("expected 4 entries, got %d", res.EntryCount)
	}
}

func TestSeqAndPrevChainCorrectly(t *testing.T) {
	l := newTestLedger(t)
	e1 := mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{})
	e2 := mustAppend(t, l, "run-1", "guard", "fs.write", map[string]interface{}{})
	e3 := mustAppend(t, l, "run-1", "guard", "run.end", map[string]interface{}{})

	if e1.Seq != 1 || e2.Seq != 2 || e3.Seq != 3 {
		t.Fatalf("expected seq 1,2,3 got %d,%d,%d", e1.Seq, e2.Seq, e3.Seq)
	}
	if e1.Prev != "" {
		t.Fatalf("first entry prev should be empty, got %q", e1.Prev)
	}
	if e2.Prev != e1.Hash {
		t.Fatalf("e2.Prev should equal e1.Hash")
	}
	if e3.Prev != e2.Hash {
		t.Fatalf("e3.Prev should equal e2.Hash")
	}
}

// TestTamperDetection manually edits an entry's payload (without fixing
// up its hash) and confirms Verify fails at exactly that sequence number,
// not before and not silently.
func TestTamperDetection(t *testing.T) {
	l := newTestLedger(t)
	mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{"agent": "claude-code"})
	mustAppend(t, l, "run-1", "guard", "fs.write", map[string]interface{}{"path": "main.go"})
	mustAppend(t, l, "run-1", "guard", "fs.write", map[string]interface{}{"path": "util.go"})
	mustAppend(t, l, "run-1", "guard", "run.end", map[string]interface{}{"exit_code": 0})

	segPath := l.segmentPath(0)
	lines := readLines(t, segPath)
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}

	// Tamper with entry seq=2 (fs.write main.go) by changing its payload
	// path, which invalidates the stored hash for that entry only.
	var e Entry
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Seq != 2 {
		t.Fatalf("expected to tamper seq 2, line was seq %d", e.Seq)
	}
	e.Payload = json.RawMessage(`{"path":"TAMPERED.go"}`)
	tampered, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}
	lines[1] = string(tampered)
	writeLines(t, segPath, lines)

	res, err := l.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatalf("expected tamper detection to fail verify, but it passed")
	}
	if res.FailedAt == nil {
		t.Fatalf("expected FailedAt to be set")
	}
	if res.FailedAt.Seq != 2 {
		t.Fatalf("expected tamper detected at seq 2, got seq %d (reason: %s)", res.FailedAt.Seq, res.FailedAt.Reason)
	}
}

// TestTamperHashItselfDetected covers tampering with the hash field
// directly (not just payload), which must also be caught.
func TestTamperHashItselfDetected(t *testing.T) {
	l := newTestLedger(t)
	mustAppend(t, l, "run-1", "guard", "run.start", map[string]interface{}{})
	mustAppend(t, l, "run-1", "guard", "run.end", map[string]interface{}{})

	segPath := l.segmentPath(0)
	lines := readLines(t, segPath)
	var e Entry
	json.Unmarshal([]byte(lines[0]), &e)
	e.Hash = "0000000000000000000000000000000000000000000000000000000000000000"
	b, _ := json.Marshal(e)
	lines[0] = string(b)
	writeLines(t, segPath, lines)

	res, err := l.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatalf("expected forged hash to fail verify")
	}
	if res.FailedAt.Seq != 1 {
		t.Fatalf("expected failure at seq 1, got %d", res.FailedAt.Seq)
	}
}

// TestConcurrentAppendKeepsChainValid spins up many goroutines appending
// concurrently and checks the resulting chain is fully valid with no
// lost or duplicated sequence numbers.
func TestConcurrentAppendKeepsChainValid(t *testing.T) {
	l := newTestLedger(t)
	const goroutines = 8
	const perGoroutine = 25

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_, err := l.Append("run-concurrent", "guard", "fs.write",
					map[string]interface{}{"goroutine": g, "i": i}, time.Time{})
				if err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append error: %v", err)
	}

	res, err := l.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("chain invalid after concurrent writes: %+v", res.FailedAt)
	}
	want := goroutines * perGoroutine
	if res.EntryCount != want {
		t.Fatalf("expected %d entries, got %d (lost or duplicated writes)", want, res.EntryCount)
	}

	tail, err := l.Tail(want)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	seen := map[int]bool{}
	for _, e := range tail {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d after concurrent append", e.Seq)
		}
		seen[e.Seq] = true
	}
	for i := 1; i <= want; i++ {
		if !seen[i] {
			t.Fatalf("missing seq %d after concurrent append", i)
		}
	}
}

// TestRotationSealAndCrossSegmentVerify forces rotation with a tiny
// threshold and confirms: a seal entry closes the old segment, a rotate
// entry opens the new one referencing the old terminal hash, multiple
// segment files exist, and Verify walks across all of them successfully.
func TestRotationSealAndCrossSegmentVerify(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "lighthouse.jsonl"), 1) // MB param unused directly here
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Force a tiny rotation threshold so a handful of entries triggers it.
	l.rotateAtBytes = 300

	const total = 60
	for i := 0; i < total; i++ {
		_, err := l.Append("run-rotate", "guard", "fs.write",
			map[string]interface{}{"i": i, "note": "padding-to-force-rotation-boundaries"}, time.Time{})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	segs, err := l.segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	if len(segs) < 2 {
		t.Fatalf("expected rotation to produce multiple segments, got %d: %v", len(segs), segs)
	}

	res, err := l.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("cross-segment verify failed: %+v", res.FailedAt)
	}
	if res.SegmentCount != len(segs) {
		t.Fatalf("segment count mismatch: %d vs %d", res.SegmentCount, len(segs))
	}

	// Confirm we actually saw seal/rotate housekeeping entries somewhere
	// in the chain, and that the rotate entry's terminal_hash matches the
	// seal entry's hash immediately preceding it.
	tail, err := l.Tail(0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	sawSeal, sawRotate := false, false
	for i, e := range tail {
		if e.Type == "ledger.seal" {
			sawSeal = true
		}
		if e.Type == "ledger.rotate" {
			sawRotate = true
			if i == 0 {
				t.Fatalf("rotate entry should never be first overall entry")
			}
			prevSeal := tail[i-1]
			if prevSeal.Type != "ledger.seal" {
				t.Fatalf("rotate entry at seq %d should immediately follow a seal entry, found %s", e.Seq, prevSeal.Type)
			}
			var payload struct {
				TerminalHash string `json:"terminal_hash"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("unmarshal rotate payload: %v", err)
			}
			if payload.TerminalHash != prevSeal.Hash {
				t.Fatalf("rotate terminal_hash %q != preceding seal hash %q", payload.TerminalHash, prevSeal.Hash)
			}
		}
	}
	if !sawSeal || !sawRotate {
		t.Fatalf("expected both seal and rotate housekeeping entries, sawSeal=%v sawRotate=%v", sawSeal, sawRotate)
	}
}

// TestResumeAfterRestart confirms LastHashSeq lets a fresh Ledger handle
// (simulating a new process) continue the chain correctly.
func TestResumeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "lighthouse.jsonl")
	l1, err := Open(base, 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, l1, "run-1", "guard", "run.start", map[string]interface{}{})
	mustAppend(t, l1, "run-1", "guard", "fs.write", map[string]interface{}{})

	l2, err := Open(base, 100) // fresh handle, simulates process restart
	if err != nil {
		t.Fatalf("Open (2nd): %v", err)
	}
	hash, seq, ok, err := l2.LastHashSeq()
	if err != nil {
		t.Fatalf("LastHashSeq: %v", err)
	}
	if !ok || seq != 2 || hash == "" {
		t.Fatalf("expected resume at seq 2 with a hash, got seq=%d hash=%q ok=%v", seq, hash, ok)
	}

	e3 := mustAppend(t, l2, "run-1", "guard", "run.end", map[string]interface{}{})
	if e3.Seq != 3 || e3.Prev != hash {
		t.Fatalf("resumed append did not continue chain correctly: seq=%d prev=%q want prev=%q", e3.Seq, e3.Prev, hash)
	}

	res, err := l2.Verify()
	if err != nil || !res.OK {
		t.Fatalf("post-restart verify failed: ok=%v err=%v failedAt=%+v", res.OK, err, res.FailedAt)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}
