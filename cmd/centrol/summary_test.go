package main

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// goroutineID extracts this goroutine's numeric ID from its own stack
// trace (the standard, if unergonomic, way to get one in Go — there is
// no public runtime.GoID()). Used only by
// TestRunSetupCallsEndFromCallersGoroutine below to verify, directly,
// that a signal arriving during the pre-Run setup window never causes
// end (and so Governor.Exit) to be called from any goroutine other
// than the one that called runSetup — see runSetup's and
// newSignalWatch's doc comments in summary.go for why that must hold.
func goroutineID(t *testing.T) int64 {
	t.Helper()
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	fields := bytes.Fields(buf)
	if len(fields) < 2 {
		t.Fatalf("could not parse goroutine ID from stack trace: %q", buf)
	}
	id, err := strconv.ParseInt(string(fields[1]), 10, 64)
	if err != nil {
		t.Fatalf("could not parse goroutine ID %q: %v", fields[1], err)
	}
	return id
}

// TestRunSetupCallsEndFromCallersGoroutine is the targeted check Pass
// 3.9c Final's correction asks for: a signal arriving during the
// pre-Run setup window must route through end() — and so
// Governor.Exit — from the SAME goroutine that is running the setup
// code (cmdGuard/cmdProxy's own, ultimately main's), never from
// newSignalWatch's own signal-listening goroutine. It replaces the
// deviation the previous draft of this pass had accepted (the signal
// handler calling end/Exit directly) with the structural guarantee
// that runSetup's ctx-cancellation branch — not a callback invoked
// from another goroutine — is what calls end.
func TestRunSetupCallsEndFromCallersGoroutine(t *testing.T) {
	g, _ := openGovernorAt(t.TempDir())
	ctx, signalOutcome, stop := newSignalWatch(g)
	defer stop()

	callerGoroutine := goroutineID(t)

	var mu sync.Mutex
	var endCalledFromGoroutine int64
	var endCalls int
	fakeEnd := func(cause string, code int, printMsg func()) {
		mu.Lock()
		endCalledFromGoroutine = goroutineID(t)
		endCalls++
		mu.Unlock()
		if cause != "sigint" {
			t.Errorf(`expected cause "sigint", got %q`, cause)
		}
		if code != 130 {
			t.Errorf("expected code 130, got %d", code)
		}
		// A real end() calls Governor.Exit here, which never returns.
		// This fake stands in for that so the test process survives —
		// what it's checking is which goroutine reached this point,
		// not what happens after.
	}

	// step blocks until the test sends the signal below, simulating a
	// setup call with no cancellation hook of its own (LoadIgnore,
	// Snapshot, a policy resolver, the allowlist prompt).
	stepUnblocked := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Errorf("signaling self: %v", err)
		}
		close(stepUnblocked)
	}()

	err := runSetup(ctx, fakeEnd, signalOutcome, func() error {
		<-stepUnblocked // never returns on its own within this test's lifetime once signaled; runSetup's ctx branch must win instead.
		<-make(chan struct{})
		return nil
	})
	_ = err // runSetup's own return value isn't what this test checks.

	mu.Lock()
	defer mu.Unlock()
	if endCalls != 1 {
		t.Fatalf("expected end to be called exactly once, got %d", endCalls)
	}
	if endCalledFromGoroutine != callerGoroutine {
		t.Fatalf("expected end to be called from runSetup's caller's own goroutine (%d), got goroutine %d — a signal must never reach end/Exit from any other goroutine", callerGoroutine, endCalledFromGoroutine)
	}
}
