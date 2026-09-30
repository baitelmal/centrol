package main

import (
	"sync"
	"time"

	"github.com/scirem/centrol/internal/policy"
	"github.com/scirem/centrol/internal/session"
)

// guardedContract is a session contract that can be safely read from one
// goroutine (the watcher, evaluating fs events) while amended from
// another (the scope poller, applying `centrol scope` requests) for the
// duration of one run.
type guardedContract struct {
	mu sync.Mutex
	c  policy.Contract
}

func newGuardedContract(c policy.Contract) *guardedContract {
	return &guardedContract{c: c}
}

func (gc *guardedContract) get() policy.Contract {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.c
}

func (gc *guardedContract) amend(path string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.c.AmendScope(path)
}

// contractAwareEmit wraps an EmitFunc so that, in addition to the raw
// fs.write/fs.create/fs.delete event the watcher always emits, an
// out-of-scope or otherwise non-Allow observation also produces a
// policy.violation entry — guard's own "compare against the session
// contract" behavior. This never blocks the write itself: guard is an
// accountability layer, not a sandbox, so it logs rather than prevents.
//
// When observe is true, the exact same evaluation happens but the
// logged entry carries payload.decision = "would_flag"/"would_block"
// instead of guard's normal (unlabeled) policy.violation, so an observe
// run's ledger is clearly distinguishable from an enforced one even
// though guard never actually blocks fs writes either way.
func contractAwareEmit(inner func(run, src, typ string, payload map[string]interface{}) error, gc *guardedContract, observe bool) func(run, src, typ string, payload map[string]interface{}) error {
	return func(run, src, typ string, payload map[string]interface{}) error {
		if err := inner(run, src, typ, payload); err != nil {
			return err
		}
		switch typ {
		case "fs.write", "fs.create", "fs.delete":
		default:
			return nil
		}
		path, _ := payload["path"].(string)
		tier, reason := policy.EvaluateFSPath(path, gc.get())
		if tier == policy.Allow {
			return nil
		}
		violationPayload := map[string]interface{}{
			"path": path, "reason": reason, "tier": tier.String(), "fs_event": typ,
		}
		if observe {
			decision := "would_flag"
			if tier == policy.Block {
				decision = "would_block"
			}
			violationPayload["decision"] = decision
		}
		return inner(run, "guard", "policy.violation", violationPayload)
	}
}

// pollScopeAmendments watches the .centrol/scope-requests.jsonl control
// file — deliberately NOT the fsnotify watcher, which hard-excludes
// .centrol/ unconditionally — and applies each new request to the live
// contract, logging it as contract.declare. It runs until stop is
// closed.
func pollScopeAmendments(dir, runID string, gc *guardedContract, emit func(run, src, typ string, payload map[string]interface{}) error, stop <-chan struct{}) {
	var offset int64
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			reqs, newOffset, err := session.PollScopeRequests(dir, offset)
			if err != nil {
				continue // transient read error; try again next tick rather than crash the run
			}
			offset = newOffset
			for _, r := range reqs {
				gc.amend(r.Path)
				_ = emit(runID, "guard", "contract.declare", map[string]interface{}{
					"amend": r.Path, "requested_at": r.TS,
				})
			}
		}
	}
}
