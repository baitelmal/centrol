package transport

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// StdioTarget fronts an MCP server run as a child process speaking
// JSON-RPC over its stdin/stdout — the transport centrol proxy has
// always supported. This is the exec.Command-based path moved here
// verbatim in v0.2.0 pass 1: same process wiring, same STDIO
// DISCIPLINE (the target's own stderr is diagnostic only, wired
// straight to the caller's diagOut, never mixed into the frame
// stream), same framing.
type StdioTarget struct {
	Command string
	Args    []string
	Stderr  io.Writer // the target's own stderr; diagnostic only, never client-facing

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	recvCh    <-chan []byte
	recvErrFn func() error
}

// NewStdioTarget returns a StdioTarget ready to Start. stderr receives
// the spawned process's own stderr stream verbatim (diagnostic only).
func NewStdioTarget(command string, args []string, stderr io.Writer) *StdioTarget {
	return &StdioTarget{Command: command, Args: args, Stderr: stderr}
}

// Start spawns the target process and wires its stdin/stdout pipes.
// ctx is accepted to satisfy Target but is not yet used to bound or
// cancel the subprocess — the pre-pass-1 stdio path had no such
// cancellation either, and this pass changes no behavior.
func (t *StdioTarget) Start(ctx context.Context) error {
	_ = ctx
	t.cmd = exec.Command(t.Command, t.Args...)
	t.cmd.Stderr = t.Stderr

	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("proxy: target stdin pipe: %w", err)
	}
	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("proxy: target stdout pipe: %w", err)
	}
	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("proxy: starting target %q: %w", t.Command, err)
	}
	t.stdin, t.stdout = stdin, stdout
	return nil
}

// Send writes one frame to the target's stdin.
func (t *StdioTarget) Send(frame []byte) error {
	return writeFrame(t.stdin, frame)
}

// Receive returns the channel of frames read from the target's
// stdout. Safe to call more than once; the underlying reader is only
// started once.
func (t *StdioTarget) Receive() (<-chan []byte, error) {
	if t.recvCh == nil {
		t.recvCh, t.recvErrFn = readFrames(t.stdout)
	}
	return t.recvCh, nil
}

// Err reports why the channel returned by Receive closed: nil for a
// clean end, or the underlying read failure otherwise (see
// readFrames's os.ErrClosed handling for what counts as "clean").
// Only meaningful after that channel has been fully drained. Not part
// of the Target interface — callers that want it use a type
// assertion, since not every transport has a meaningful distinction
// here.
func (t *StdioTarget) Err() error {
	if t.recvErrFn == nil {
		return nil
	}
	return t.recvErrFn()
}

// CloseInput closes the target's stdin, signaling "no more input is
// coming" without waiting for the target to exit. This lets a target
// that reads until EOF exit on its own — exactly what closing
// targetStdin did in the pre-pass-1 stdio path. Not part of the
// Target interface (a request/response transport like HTTP has no
// persistent input stream to half-close); callers that want it use a
// type assertion.
func (t *StdioTarget) CloseInput() error {
	if t.stdin == nil {
		return nil
	}
	return t.stdin.Close()
}

// Stop waits for the target process to exit and returns its exit
// error, if any — the same cmd.Wait() the pre-pass-1 stdio path
// called directly, reached through the interface now. Safe to call
// concurrently with CloseInput/Send/Receive; it does not itself close
// stdin.
func (t *StdioTarget) Stop() error {
	if t.cmd == nil {
		return nil
	}
	return t.cmd.Wait()
}
