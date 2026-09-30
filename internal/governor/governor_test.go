package governor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scirem/centrol/internal/ledger"
)

func newTestGovernor(t *testing.T) *Governor {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(filepath.Join(dir, "lighthouse.jsonl"), 100)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	g, err := New(l)
	if err != nil {
		t.Fatalf("governor.New: %v", err)
	}
	return g
}

func TestEmitValidSignalWrites(t *testing.T) {
	g := newTestGovernor(t)
	e, err := g.Emit("run-1", "guard", "run.start", map[string]interface{}{"agent": "claude-code"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if e.Type != "run.start" || e.Seq != 1 {
		t.Fatalf("unexpected entry: %+v", e)
	}
}

func TestEmitUnknownTypeIsSilenced(t *testing.T) {
	g := newTestGovernor(t)
	_, err := g.Emit("run-1", "guard", "totally.made.up", map[string]interface{}{"x": 1})
	if err != ErrSilenced {
		t.Fatalf("expected ErrSilenced, got %v", err)
	}
	entries, err := g.Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(entries) != 1 || entries[0].Type != "policy.silence" {
		t.Fatalf("expected one policy.silence entry, got %+v", entries)
	}
}

func TestEmitUnknownSrcIsSilenced(t *testing.T) {
	g := newTestGovernor(t)
	_, err := g.Emit("run-1", "not-a-real-surface", "run.start", map[string]interface{}{})
	if err != ErrSilenced {
		t.Fatalf("expected ErrSilenced, got %v", err)
	}
	entries, _ := g.Tail(10)
	if len(entries) != 1 || entries[0].Type != "policy.silence" {
		t.Fatalf("expected one policy.silence entry, got %+v", entries)
	}
}

func TestEmitMissingRunIsSilenced(t *testing.T) {
	g := newTestGovernor(t)
	_, err := g.Emit("", "guard", "run.start", map[string]interface{}{})
	if err != ErrSilenced {
		t.Fatalf("expected ErrSilenced, got %v", err)
	}
}

// Non-conforming signals must still fully capture the raw payload inward
// even though nothing is returned outward but ErrSilenced.
func TestSilenceCapturesRawPayload(t *testing.T) {
	g := newTestGovernor(t)
	_, _ = g.Emit("run-1", "guard", "not.a.type", map[string]interface{}{"secret": "value-1234"})
	entries, _ := g.Tail(1)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry")
	}
	if !contains([]string{"policy.silence"}, entries[0].Type) {
		t.Fatalf("expected policy.silence entry")
	}
	if string(entries[0].Payload) == "" || !jsonContains(t, entries[0].Payload, "value-1234") {
		t.Fatalf("expected raw payload to be captured, got %s", entries[0].Payload)
	}
}

func jsonContains(t *testing.T, raw []byte, substr string) bool {
	t.Helper()
	return len(raw) > 0 && (func() bool {
		for i := 0; i+len(substr) <= len(raw); i++ {
			if string(raw[i:i+len(substr)]) == substr {
				return true
			}
		}
		return false
	})()
}

func TestChainStaysValidAfterMixOfGoodAndSilenced(t *testing.T) {
	g := newTestGovernor(t)
	g.Emit("run-1", "guard", "run.start", map[string]interface{}{})
	g.Emit("run-1", "guard", "bogus.type", map[string]interface{}{})
	g.Emit("run-1", "guard", "fs.write", map[string]interface{}{"path": "a.go"})
	g.Emit("run-1", "bad-src", "fs.write", map[string]interface{}{})
	g.Emit("run-1", "guard", "run.end", map[string]interface{}{})

	res, err := g.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected valid chain (silences are real entries), got %+v", res.FailedAt)
	}
	if res.EntryCount != 5 {
		t.Fatalf("expected 5 entries (3 real + 2 silence), got %d", res.EntryCount)
	}
}

// TestSchemaDataMatchesCanonical guards against the embedded copy
// (schema_data.json) drifting from the canonical schemas/event.v1.json
// at the repo root, since go:embed cannot reach outside this package.
func TestSchemaDataMatchesCanonical(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "schemas", "event.v1.json"))
	if err != nil {
		t.Skipf("canonical schema not found at expected relative path, skipping: %v", err)
	}
	if string(canonical) != string(embeddedSchema) {
		t.Fatalf("internal/governor/schema_data.json has drifted from schemas/event.v1.json; copy the canonical file over the embedded one")
	}
}

// TestEmitAcceptsAllThreeSources confirms the Governor is event-source
// agnostic: src=guard, src=proxy, and the reserved src=verify all go
// through the same Emit() call and land in the ledger with valid
// hashes. The Governor contains no guard-specific or proxy-specific
// logic; it validates against the schema and writes, regardless of src.
func TestEmitAcceptsAllThreeSources(t *testing.T) {
	g := newTestGovernor(t)
	for _, src := range []string{"guard", "proxy", "verify"} {
		e, err := g.Emit("run-1", src, "run.start", map[string]interface{}{"src_under_test": src})
		if err != nil {
			t.Fatalf("Emit with src=%s: %v", src, err)
		}
		if e.Src != src {
			t.Fatalf("expected entry.Src=%s, got %s", src, e.Src)
		}
	}
	res, err := g.Verify()
	if err != nil || !res.OK {
		t.Fatalf("expected valid chain across all three sources: ok=%v err=%v", res.OK, err)
	}
	if res.EntryCount != 3 {
		t.Fatalf("expected 3 entries, got %d", res.EntryCount)
	}
}

// TestVerifyReservedEventTypesAreValid confirms every reserved verify.*
// event type added for the v0.3 validator surface already validates
// against this schema (still frozen at v: 1) even though nothing emits
// them yet, so shipping the verify surface later requires no version
// bump.
func TestVerifyReservedEventTypesAreValid(t *testing.T) {
	g := newTestGovernor(t)
	reservedTypes := []string{
		"verify.start", "verify.end", "verify.attempt",
		"verify.test_result", "verify.invariant_pass", "verify.invariant_fail",
		"verify.packet_emitted", "verify.approved", "verify.rejected", "verify.auto_approved",
	}
	for _, typ := range reservedTypes {
		_, err := g.Emit("run-1", "verify", typ, map[string]interface{}{"synthetic": true})
		if err != nil {
			t.Fatalf("expected reserved type %s to validate against the schema, got %v", typ, err)
		}
	}
	res, err := g.Verify()
	if err != nil || !res.OK {
		t.Fatalf("expected valid chain for all reserved verify.* events: ok=%v err=%v failedAt=%+v", res.OK, err, res.FailedAt)
	}
	if res.EntryCount != len(reservedTypes) {
		t.Fatalf("expected %d entries, got %d", len(reservedTypes), res.EntryCount)
	}
}
