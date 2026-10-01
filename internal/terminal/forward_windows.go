//go:build windows

package terminal

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// forwardedSignals: Windows has no SIGHUP or SIGWINCH; Go's runtime maps
// Ctrl+C / Ctrl+Break to os.Interrupt, which we forward as SIGINT-
// equivalent. There is no console resize signal to forward on Windows —
// child console applications receive resize directly from the console
// subsystem, so there is nothing centrol needs to relay for that case.
var forwardedSignals = []os.Signal{os.Interrupt}

// Run execs the given command with stdin/stdout/stderr passed through
// unmodified, forwards Ctrl+C for the duration of the run, and returns
// the child's exit code so the caller can propagate it as centrol guard's
// own exit code.
func Run(name string, args []string, env []string) (exitCode int, err error) {
	// signal.Notify is registered before cmd.Start, not after: a signal
	// landing in the gap between Start returning and Notify registering
	// would have nothing to catch it, since the caller (cmd_guard.go)
	// has already disarmed its own early-signal handler by the time it
	// calls Run — see that handler's doc comment on the handoff. Notify
	// is safe to register before the child exists: a signal that
	// arrives this early just sits buffered on sigCh (capacity 8) until
	// the select loop below starts, by which point cmd.Process is
	// always already set (Start has returned), so the forward below
	// still reaches the real child rather than a nil Process.
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, forwardedSignals...)
	defer signal.Stop(sigCh)

	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if env != nil {
		cmd.Env = env
	}

	if err := cmd.Start(); err != nil {
		return -1, err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	for {
		select {
		case <-sigCh:
			if cmd.Process != nil {
				_ = cmd.Process.Signal(os.Interrupt)
			}
		case waitErr := <-done:
			if waitErr == nil {
				return 0, nil
			}
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					return status.ExitStatus(), nil
				}
				return 1, nil
			}
			return -1, waitErr
		}
	}
}
