// MIT License — see the header in main.go for the full text.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writer builds a small ledger file by hand, using exactly the same
// hashing rule main.go verifies — not internal/ledger's Append — so
// these tests stay as independent of the centrol module as main.go
// itself. prevHash/prevSeq chain across calls the way real entries do.
type writer struct {
	prevHash string
	prevSeq  int
}

func (w *writer) append(t *testing.T, path, run, src, typ string, payload map[string]interface{}) []byte {
	t.Helper()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	pre := entryPre{
		V:       1,
		Seq:     w.prevSeq + 1,
		TS:      "2026-10-01T12:00:00Z",
		Run:     run,
		Src:     src,
		Type:    typ,
		Payload: payloadBytes,
		Prev:    w.prevHash,
	}
	hash, err := computeHash(pre)
	if err != nil {
		t.Fatalf("computeHash: %v", err)
	}
	e := entry{
		V: pre.V, Seq: pre.Seq, TS: pre.TS, Run: pre.Run, Src: pre.Src,
		Type: pre.Type, Payload: pre.Payload, Prev: pre.Prev, Hash: hash,
	}
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	w.prevHash, w.prevSeq = hash, pre.Seq
	return bytes.TrimRight(line, "\n")
}

func buildValidLedger(t *testing.T, path string, n int) [][]byte {
	t.Helper()
	w := &writer{}
	var lines [][]byte
	for i := 0; i < n; i++ {
		lines = append(lines, w.append(t, path, "run-1", "guard", "fs.write", map[string]interface{}{"path": "src/main.go", "n": i}))
	}
	return lines
}

func runCLI(t *testing.T, path string) (code int, stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code = run([]string{path}, &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

func TestValidChainPasses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lighthouse.jsonl")
	buildValidLedger(t, path, 5)

	code, stdout, _ := runCLI(t, path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "OK: 5 entries across 1 segments, chain intact") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestTamperedEntryFailsAtRightSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lighthouse.jsonl")
	buildValidLedger(t, path, 5)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
	// Tamper with entry seq 3 (index 2): flip its payload's "n" field
	// without touching its stored hash, so the hash recomputed from the
	// tampered content no longer matches what's on disk.
	var e entry
	if err := json.Unmarshal([]byte(lines[2]), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e.Payload = json.RawMessage(`{"path":"src/main.go","n":999}`)
	tampered, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	lines[2] = string(tampered)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, stdout, _ := runCLI(t, path)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "FAIL at seq 3: hash mismatch") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestDeletedEntryFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lighthouse.jsonl")
	buildValidLedger(t, path, 5)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// Delete entry seq 3 (index 2) outright: what follows (seq 4) now
	// has both a sequence gap and a prev that points at a hash no
	// longer in the file.
	lines = append(lines[:2], lines[3:]...)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, stdout, _ := runCLI(t, path)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "FAIL at seq 4: sequence gap: expected 3, got 4") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestRotatedSegmentsVerifyAcrossBoundary(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "lighthouse.jsonl")
	rotated := filepath.Join(dir, "lighthouse.001.jsonl")

	w := &writer{}
	w.append(t, base, "run-1", "guard", "fs.write", map[string]interface{}{"path": "a"})
	w.append(t, base, "run-1", "guard", "fs.write", map[string]interface{}{"path": "b"})
	// Seal the base segment (a real rotation's last entry in the old
	// segment), then continue the exact same chain into the new one —
	// same prevHash/prevSeq state, just a different file.
	w.append(t, base, "run-1", "guard", "ledger.seal", map[string]interface{}{"segment": "lighthouse.jsonl"})
	w.append(t, rotated, "run-1", "guard", "ledger.rotate", map[string]interface{}{"from_segment": "lighthouse.jsonl", "to_segment": "lighthouse.001.jsonl"})
	w.append(t, rotated, "run-1", "guard", "fs.write", map[string]interface{}{"path": "c"})

	code, stdout, _ := runCLI(t, dir)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "OK: 5 entries across 2 segments, chain intact") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestRotatedSegmentsDetectTamperAcrossBoundary(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "lighthouse.jsonl")
	rotated := filepath.Join(dir, "lighthouse.001.jsonl")

	w := &writer{}
	w.append(t, base, "run-1", "guard", "fs.write", map[string]interface{}{"path": "a"})
	w.append(t, base, "run-1", "guard", "ledger.seal", map[string]interface{}{"segment": "lighthouse.jsonl"})
	w.append(t, rotated, "run-1", "guard", "ledger.rotate", map[string]interface{}{"from_segment": "lighthouse.jsonl", "to_segment": "lighthouse.001.jsonl"})

	// Break the link at the rotation boundary itself: the rotate
	// entry's prev no longer matches the seal entry's hash.
	raw, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var e entry
	if err := json.Unmarshal(bytes.TrimRight(raw, "\n"), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e.Prev = strings.Repeat("0", len(e.Prev))
	tampered, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(rotated, append(tampered, '\n'), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, stdout, _ := runCLI(t, dir)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "hash mismatch") {
		t.Fatalf("expected the rewritten rotate entry to fail its own hash check first, got: %q", stdout)
	}
}

func TestMalformedJSONLFailsWithClearError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lighthouse.jsonl")
	buildValidLedger(t, path, 2)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("not json at all\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	code, stdout, _ := runCLI(t, path)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "malformed entry") || !strings.Contains(stdout, "line 3") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestMissingFileIsReadError(t *testing.T) {
	code, _, stderr := runCLI(t, filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr)
	}
}

func TestEmptyDirectoryIsReadError(t *testing.T) {
	code, _, stderr := runCLI(t, t.TempDir())
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "no .jsonl ledger segments") {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

func TestUsageErrorExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	code = run([]string{"a", "b"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// A chain with its oldest entries deleted has every remaining link
// intact. It must still fail: the first entry is not seq 1.
func TestDeletedHeadFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lighthouse.jsonl")
	buildValidLedger(t, path, 5)
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[2:], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := runCLI(t, path)
	if code != 1 || !strings.Contains(stdout, "FAIL at seq 3: chain does not start at seq 1") {
		t.Fatalf("code=%d stdout=%q, want exit 1 with a does-not-start-at-seq-1 failure", code, stdout)
	}
}

// A slice of a longer chain (an archived batch) verifies with
// --first-seq naming its first seq, and only with that value.
func TestFirstSeqVerifiesASlice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lighthouse.jsonl")
	buildValidLedger(t, path, 5)
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[2:], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for first, want := range map[string]int{"3": 0, "2": 1, "1": 1, "4": 1} {
		var out, errb bytes.Buffer
		if code := run([]string{"--first-seq", first, path}, &out, &errb); code != want {
			t.Errorf("--first-seq %s: exit %d, want %d (%s)", first, code, want, out.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"--first-seq", "0", path}, &out, &errb); code != 2 {
		t.Errorf("--first-seq 0: exit %d, want 2", code)
	}
}

func TestHelpPrintsUsageAndExitsZero(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		var out, errb bytes.Buffer
		if code := run([]string{flag}, &out, &errb); code != 0 {
			t.Errorf("%s: exit %d, want 0", flag, code)
		}
		for _, want := range []string{"Usage:", "--first-seq N", "Exit codes:", "<path>"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: help missing %q", flag, want)
			}
		}
		if errb.Len() != 0 {
			t.Errorf("%s: wrote to stderr: %q", flag, errb.String())
		}
	}
}

func TestMissingFileAndValidLedgerStillBehaveAfterHelpFlag(t *testing.T) {
	if code, _, _ := runCLI(t, filepath.Join(t.TempDir(), "nope.jsonl")); code != 2 {
		t.Errorf("missing file: exit %d, want 2", code)
	}
	path := filepath.Join(t.TempDir(), "lighthouse.jsonl")
	buildValidLedger(t, path, 3)
	if code, out, _ := runCLI(t, path); code != 0 || !strings.Contains(out, "OK: 3 entries") {
		t.Errorf("valid ledger: exit %d, out %q", code, out)
	}
}
