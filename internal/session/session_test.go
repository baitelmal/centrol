package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scirem/centrol/internal/ledger"
)

func TestRunMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := RunMarker{RunID: "run-1", Kind: "guard", RepoRoot: "/repo", StartedAt: time.Now().UTC(), AllowedPaths: []string{"."}}
	if err := WriteCurrentRun(dir, m); err != nil {
		t.Fatalf("WriteCurrentRun: %v", err)
	}
	got, ok, err := ReadCurrentRun(dir)
	if err != nil {
		t.Fatalf("ReadCurrentRun: %v", err)
	}
	if !ok || got.RunID != "run-1" || got.Kind != "guard" {
		t.Fatalf("expected marker round trip, got %+v ok=%v", got, ok)
	}
	if err := ClearCurrentRun(dir); err != nil {
		t.Fatalf("ClearCurrentRun: %v", err)
	}
	_, ok, err = ReadCurrentRun(dir)
	if err != nil {
		t.Fatalf("ReadCurrentRun after clear: %v", err)
	}
	if ok {
		t.Fatalf("expected no marker after ClearCurrentRun")
	}
}

func TestReadCurrentRunNoMarkerIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	_, ok, err := ReadCurrentRun(dir)
	if err != nil {
		t.Fatalf("expected no error for a missing marker, got %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for a missing marker")
	}
}

// TestPollScopeRequestsTailsAcrossMultiplePolls confirms the core
// offset-tracking contract a live run relies on: each poll only returns
// what's new since the last offset, and a request appended between
// polls is picked up exactly once.
func TestPollScopeRequestsTailsAcrossMultiplePolls(t *testing.T) {
	dir := t.TempDir()

	reqs, offset, err := PollScopeRequests(dir, 0)
	if err != nil {
		t.Fatalf("PollScopeRequests on nonexistent file: %v", err)
	}
	if len(reqs) != 0 || offset != 0 {
		t.Fatalf("expected empty poll on nonexistent file, got %v offset=%d", reqs, offset)
	}

	if err := AppendScopeRequest(dir, "docs"); err != nil {
		t.Fatalf("AppendScopeRequest: %v", err)
	}
	reqs, offset, err = PollScopeRequests(dir, offset)
	if err != nil {
		t.Fatalf("PollScopeRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Path != "docs" {
		t.Fatalf("expected 1 request for 'docs', got %v", reqs)
	}

	// A second poll from the returned offset, with nothing new
	// appended, must return nothing — not the same request again.
	reqs2, offset2, err := PollScopeRequests(dir, offset)
	if err != nil {
		t.Fatalf("PollScopeRequests (no new data): %v", err)
	}
	if len(reqs2) != 0 {
		t.Fatalf("expected no requests on a repeat poll with nothing new, got %v", reqs2)
	}
	if offset2 != offset {
		t.Fatalf("expected offset unchanged when nothing new was appended, got %d vs %d", offset2, offset)
	}

	// Append two more; only these two should come back.
	if err := AppendScopeRequest(dir, "tests"); err != nil {
		t.Fatal(err)
	}
	if err := AppendScopeRequest(dir, "scripts"); err != nil {
		t.Fatal(err)
	}
	reqs3, _, err := PollScopeRequests(dir, offset2)
	if err != nil {
		t.Fatalf("PollScopeRequests: %v", err)
	}
	if len(reqs3) != 2 || reqs3[0].Path != "tests" || reqs3[1].Path != "scripts" {
		t.Fatalf("expected exactly [tests, scripts] on this poll, got %v", reqs3)
	}
}

func TestPollScopeRequestsSkipsMalformedLineWithoutFailingTheWholePoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, scopeRequestsFile)
	good1, _ := json.Marshal(ScopeRequest{Path: "a"})
	good2, _ := json.Marshal(ScopeRequest{Path: "b"})
	content := string(good1) + "\n" + "not json at all" + "\n" + string(good2) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reqs, _, err := PollScopeRequests(dir, 0)
	if err != nil {
		t.Fatalf("PollScopeRequests: %v", err)
	}
	if len(reqs) != 2 || reqs[0].Path != "a" || reqs[1].Path != "b" {
		t.Fatalf("expected the two well-formed requests despite a malformed line between them, got %v", reqs)
	}
}

func TestHonestyScoreStartsAt100AndDeductsForViolationsAndSilence(t *testing.T) {
	entries := []ledger.Entry{
		{Type: "run.start"},
		{Type: "fs.write"},
	}
	if got := HonestyScore(entries); got != 100 {
		t.Fatalf("expected clean entries to stay at the 100 ceiling, got %d", got)
	}

	withViolation := append(entries, ledger.Entry{Type: "policy.violation"})
	if got := HonestyScore(withViolation); got >= 100 {
		t.Fatalf("expected a policy.violation to deduct from the score, got %d", got)
	}

	withSilence := append(entries, ledger.Entry{Type: "policy.silence"})
	violationScore := HonestyScore(withViolation)
	silenceScore := HonestyScore(withSilence)
	if silenceScore <= violationScore {
		t.Fatalf("expected policy.silence to deduct less than policy.violation (silence=%d, violation=%d)", silenceScore, violationScore)
	}
}

func TestHonestyScoreRecoversSlowlyOnCleanRuns(t *testing.T) {
	dented := []ledger.Entry{{Type: "policy.violation"}}
	base := HonestyScore(dented)

	var recovering []ledger.Entry
	recovering = append(recovering, dented...)
	for i := 0; i < 5; i++ {
		recovering = append(recovering, ledger.Entry{Type: "fs.write"})
	}
	recoveredSome := HonestyScore(recovering)
	if recoveredSome <= base {
		t.Fatalf("expected the score to recover somewhat after clean entries, base=%d recovered=%d", base, recoveredSome)
	}
	if recoveredSome >= 100 {
		t.Fatalf("expected recovery to be slow, not an instant snap back to 100, got %d", recoveredSome)
	}
}

func TestHonestyScoreNeverGoesBelowZero(t *testing.T) {
	var entries []ledger.Entry
	for i := 0; i < 50; i++ {
		entries = append(entries, ledger.Entry{Type: "policy.violation"})
	}
	if got := HonestyScore(entries); got < 0 {
		t.Fatalf("expected score to floor at 0, got %d", got)
	}
}
