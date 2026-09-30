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
