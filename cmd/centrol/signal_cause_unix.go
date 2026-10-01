//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

// signalCauseFromErr inspects err for a wrapped *exec.ExitError that
// terminated because the process was killed by a signal — Pass 3.9
// Section 5's race (b): guard.Snapshot's git subprocesses (spawned via
// exec.Command with no SysProcAttr) inherit centrol's own process
// group, so the exact same SIGTERM/SIGINT a real terminal just sent
// centrol can also kill them directly, surfacing as an ordinary
// "signal: terminated" error on the snapshot path instead of (or
// racing) the early signal handler.
//
// errors.As walks err's full %w chain (guard.runGit wraps the raw
// *exec.ExitError once, guard.Snapshot wraps that again), so this finds
// the underlying ExitError regardless of how many layers of context
// guard.Snapshot's own error wrapping added.
//
// When found and the process died from SIGTERM or SIGINT specifically,
// this normalizes to the exact same cause/code earlySignalHandler
// itself would report for that signal (Section 5: "Both paths report
// the same truth through the same decision point") — so whichever of
// race (b)'s two sites calls Governor.Stop first, the run's recorded
// outcome is identical either way. Any other signal, or no signal at
// all (an ordinary git failure — bad repo state, no commits yet),
// reports ok=false: the caller falls back to treating it as an
// ordinary snapshot failure.
func signalCauseFromErr(err error) (cause string, code int, ok bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "", 0, false
	}
	status, isWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	if !isWaitStatus || !status.Signaled() {
		return "", 0, false
	}
	switch status.Signal() {
	case syscall.SIGTERM:
		return "sigterm", 143, true
	case syscall.SIGINT:
		return "sigint", 130, true
	default:
		return "", 0, false
	}
}
