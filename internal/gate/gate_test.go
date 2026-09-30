package gate

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestPromptMatchesFullLabel(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("Deny\n")
	d, err := Prompt(in, &out, "title", "detail", []Option{Opt("Allow once"), Opt("Allow session"), Opt("Deny")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if d != 2 {
		t.Fatalf("expected Decision 2 (Deny), got %d", d)
	}
	if !strings.Contains(out.String(), "Allow once | Allow session | Deny") {
		t.Fatalf("expected rendered options in output, got %q", out.String())
	}
}

func TestPromptMatchesFirstLetterCaseInsensitive(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("a\n")
	d, err := Prompt(in, &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if d != 0 {
		t.Fatalf("expected Decision 0 (Allow once, matched by 'a'), got %d", d)
	}
}

func TestPromptUnrecognizedInputYieldsNoDecision(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("banana\n")
	d, err := Prompt(in, &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if d != NoDecision {
		t.Fatalf("expected NoDecision for unrecognized input, got %d", d)
	}
}

func TestPromptEmptyInputYieldsNoDecision(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("\n")
	d, err := Prompt(in, &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if d != NoDecision {
		t.Fatalf("expected NoDecision for empty input (fail closed), got %d", d)
	}
}

func TestPromptEOFYieldsNoDecisionNotError(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("") // immediate EOF, e.g. a closed/non-interactive tty
	d, err := Prompt(in, &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")})
	if err != nil {
		t.Fatalf("expected EOF to be treated as no input, not an error: %v", err)
	}
	if d != NoDecision {
		t.Fatalf("expected NoDecision on EOF, got %d", d)
	}
}

func TestPromptConsumesExactlyOneLine(t *testing.T) {
	var out bytes.Buffer
	// Two lines queued up, as if the human typed ahead or a script fed
	// canned input: the first Prompt call must consume only the first
	// line, leaving the second for a subsequent, independent gate call
	// (this is what makes Operational/Governance transitions properly
	// serialized rather than accidentally batching decisions).
	in := strings.NewReader("Deny\nAllow once\n")
	d1, err := Prompt(in, &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")})
	if err != nil {
		t.Fatalf("Prompt 1: %v", err)
	}
	if d1 != 1 {
		t.Fatalf("expected first Prompt to resolve to Deny (1), got %d", d1)
	}
	d2, err := Prompt(in, &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")})
	if err != nil {
		t.Fatalf("Prompt 2: %v", err)
	}
	if d2 != 0 {
		t.Fatalf("expected second Prompt to resolve to Allow once (0) from the remaining input, got %d", d2)
	}
}

// TestSyntheticVerifyOptionSet exercises the gate with the v0.3 verify
// surface's own option set — [Approve | Modify | Reject] — confirming
// the gate is genuinely generic and doesn't hardcode guard/proxy's
// [Allow once | Allow session | Deny] vocabulary anywhere.
func TestSyntheticVerifyOptionSet(t *testing.T) {
	verifyOptions := []Option{Opt("Approve"), Opt("Modify"), Opt("Reject")}

	var out bytes.Buffer
	d, err := Prompt(strings.NewReader("Modify\n"), &out, "verify: attempt 1 diff ready", "3 tests changed, 1 invariant touched", verifyOptions)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if d != 1 {
		t.Fatalf("expected Decision 1 (Modify), got %d", d)
	}
	if !strings.Contains(out.String(), "Approve | Modify | Reject") {
		t.Fatalf("expected verify's own option vocabulary rendered, got %q", out.String())
	}

	// First-letter matching works identically for this vocabulary.
	d2, err := Prompt(strings.NewReader("r\n"), &out, "t", "d", verifyOptions)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if d2 != 2 {
		t.Fatalf("expected 'r' to match Reject (2), got %d", d2)
	}
}

func TestPromptWithTimeoutFiresWhenNoInputArrives(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	var out bytes.Buffer
	start := time.Now()
	d, err := PromptWithTimeout(r, &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")}, 100*time.Millisecond)
	elapsed := time.Since(start)
	if err != ErrPromptTimeout {
		t.Fatalf("expected ErrPromptTimeout, got %v", err)
	}
	if d != NoDecision {
		t.Fatalf("expected NoDecision on timeout, got %d", d)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected the timeout to fire promptly, took %v", elapsed)
	}
}

func TestPromptWithTimeoutReturnsNormallyWhenInputArrivesInTime(t *testing.T) {
	var out bytes.Buffer
	d, err := PromptWithTimeout(strings.NewReader("Deny\n"), &out, "t", "d", []Option{Opt("Allow once"), Opt("Deny")}, time.Second)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if d != 1 {
		t.Fatalf("expected Decision 1 (Deny), got %d", d)
	}
}
