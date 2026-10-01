// Package governor is the WHAT tier: it validates every signal against
// the frozen event schema and is the sole writer to the ledger. No other
// package should call ledger.Append directly.
package governor

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/scirem/centrol/internal/ledger"
)

// schema_data.json is a build-time copy of schemas/event.v1.json at the
// repo root (go:embed cannot reach outside the package directory). The
// two are kept identical; see TestSchemaDataMatchesCanonical.
//
//go:embed schema_data.json
var embeddedSchema []byte

// schema captures just the parts of schemas/event.v1.json this Governor
// interprets: which fields are required and which fields are constrained
// to an enum. This is not a general JSON-Schema engine — the event
// schema is small, frozen, and versioned, so a full interpreter would be
// weight without benefit for a single static binary.
type schema struct {
	Required   []string `json:"required"`
	Properties map[string]struct {
		Type string   `json:"type"`
		Enum []string `json:"enum"`
	} `json:"properties"`
}

// ErrSilenced is returned by Emit when a signal failed schema validation.
// It is not a failure the caller should surface to the agent: the
// Governor has already recorded full detail in the ledger as
// policy.silence, and the outward response must stay silent per the
// Protocol Silence guarantee.
var ErrSilenced = fmt.Errorf("governor: signal silenced (non-conforming, recorded as policy.silence)")

// Governor validates and, on success, forwards signals to the ledger
// (the rules/schema hand — Emit/Tail/Verify/Resume below) and, as of
// Pass 3.9, also owns run lifecycle and process termination (the
// run/change hand — Stop, MarkStopped, Phase, Cause, ExitCode, Exit,
// WatchSignals, Run, all in run.go). Both hands share this one struct;
// run's own mutex (runState.mu) is always acquired independently of
// any ledger-level locking — no code path in either hand holds one
// lock while acquiring the other, so no lock ordering rule beyond
// "never nest them" is needed.
type Governor struct {
	ledger *ledger.Ledger
	sc     schema

	run runState

	// TargetExitTimeout bounds how long Run's waitForTargetExit (run.go,
	// audit item 4g) waits for a targetWaiter's Wait() to return after
	// Stop has already signaled the target to shut down, before
	// escalating to targetKiller.Kill(). Zero (the default for a
	// Governor built via New, or in any existing test) falls back to
	// DefaultTargetExitTimeout; cmd_proxy.go sets this explicitly from
	// the resolved proxy.target_exit_timeout_seconds policy value.
	TargetExitTimeout time.Duration
}

// New loads schemas/event.v1.json (embedded at build time) and binds to
// the given ledger.
func New(l *ledger.Ledger) (*Governor, error) {
	var sc schema
	if err := json.Unmarshal(embeddedSchema, &sc); err != nil {
		return nil, fmt.Errorf("governor: parsing embedded schema: %w", err)
	}
	return &Governor{ledger: l, sc: sc}, nil
}

func (g *Governor) allowedEnum(field string) []string {
	if p, ok := g.sc.Properties[field]; ok {
		return p.Enum
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// validate checks the pre-hash, pre-seq fields of a proposed signal
// against the schema's required fields and enums. seq/hash/prev/v/ts are
// populated by the ledger itself and are not the agent's to declare.
func (g *Governor) validate(run, src, typ string, payload map[string]interface{}) error {
	if run == "" {
		return fmt.Errorf("run is required")
	}
	if src == "" {
		return fmt.Errorf("src is required")
	}
	if typ == "" {
		return fmt.Errorf("type is required")
	}
	if srcEnum := g.allowedEnum("src"); len(srcEnum) > 0 && !contains(srcEnum, src) {
		return fmt.Errorf("src %q not in allowed set %v", src, srcEnum)
	}
	if typeEnum := g.allowedEnum("type"); len(typeEnum) > 0 && !contains(typeEnum, typ) {
		return fmt.Errorf("type %q not in allowed set %v", typ, typeEnum)
	}
	if payload == nil {
		return fmt.Errorf("payload is required (may be empty object, not nil)")
	}
	return nil
}

// Emit validates a signal and, if it conforms, appends it to the ledger.
// If it does not conform, Emit writes a policy.silence entry capturing
// the raw payload and returns ErrSilenced: the caller must not surface
// any error detail outward to the agent, only stay silent.
func (g *Governor) Emit(run, src, typ string, payload map[string]interface{}) (ledger.Entry, error) {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	if err := g.validate(run, src, typ, payload); err != nil {
		silencePayload := map[string]interface{}{
			"reason":      err.Error(),
			"raw_src":     src,
			"raw_type":    typ,
			"raw_payload": payload,
		}
		silenceSrc := src
		if !contains(g.allowedEnum("src"), silenceSrc) {
			// Even the src itself may be non-conforming; the ledger's own
			// src enum only knows guard/proxy, so fall back to "guard" as
			// the recording surface while preserving the raw value in payload.
			silenceSrc = "guard"
		}
		silenceRun := run
		if silenceRun == "" {
			silenceRun = "unknown"
		}
		res, appendErr := g.ledger.Append(silenceRun, silenceSrc, "policy.silence", silencePayload, time.Now().UTC())
		if appendErr != nil {
			return ledger.Entry{}, fmt.Errorf("governor: failed to record silence: %w (original: %s)", appendErr, err.Error())
		}
		_ = res
		return ledger.Entry{}, ErrSilenced
	}

	res, err := g.ledger.Append(run, src, typ, payload, time.Now().UTC())
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("governor: ledger append failed: %w", err)
	}
	return res.Entry, nil
}

// Tail exposes read queries for `centrol status` / `centrol audit`.
func (g *Governor) Tail(n int) ([]ledger.Entry, error) {
	return g.ledger.Tail(n)
}

// Verify exposes chain verification for `centrol audit --verify`.
func (g *Governor) Verify() (ledger.VerifyResult, error) {
	return g.ledger.Verify()
}

// Resume reports the current chain head so a fresh process can continue
// the run without re-walking the whole ledger.
func (g *Governor) Resume() (hash string, seq int, hasChain bool, err error) {
	return g.ledger.LastHashSeq()
}
