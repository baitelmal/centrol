//go:build !windows

package terminal

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// forwardedSignals are the signals centrol guard forwards to the wrapped
// agent process: SIGINT (Ctrl+C), SIGTERM, SIGHUP, and SIGWINCH (terminal
// resize). Without this, interactive sessions freeze, resizes don't
// reach the child's line-discipline-aware renderer, and Ctrl+C detaches
// the child instead of stopping it.
var forwardedSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH}

// Run execs the given command with stdin/stdout/stderr passed through
// unmodified (no pty allocation — the child manages raw mode on the
// inherited terminal itself, which is what "not interfering with raw
// terminal mode" means in practice), forwards the signals above for the
// duration of the run, and returns the child's exit code so the caller
// can propagate it as centrol guard's own exit code.
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
		case sig := <-sigCh:
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		case waitErr := <-done:
			if waitErr == nil {
				return 0, nil
			}
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					if status.Signaled() {
						// Conventional shell exit code for death-by-signal.
						return 128 + int(status.Signal()), nil
					}
					return status.ExitStatus(), nil
				}
				return 1, nil
			}
			return -1, waitErr
		}
	}
}
