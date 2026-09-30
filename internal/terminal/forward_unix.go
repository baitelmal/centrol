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

	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, forwardedSignals...)
	defer signal.Stop(sigCh)

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
