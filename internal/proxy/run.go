package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/scirem/centrol/internal/gate"
	"github.com/scirem/centrol/internal/proxy/transport"
)

// Target describes how to reach the real MCP server this proxy fronts
// as a stdio subprocess (a command to spawn). This is the stable
// public shape Run and every existing stdio caller already use; it is
// unchanged by the v0.2.0 pass 1 transport-interface refactor below.
type Target struct {
	Command string
	Args    []string
}

// Run spawns target as a child process and drives it through
// RunTarget via a transport.StdioTarget. Behavior is unchanged from
// the pre-v0.2.0 stdio path — this refactor only moved the
// exec.Command wiring into internal/proxy/transport; every pump,
// STDIO DISCIPLINE guarantee, and error path below is identical.
func Run(target Target, in *Interceptor, clientIn io.Reader, clientOut io.Writer, diagOut io.Writer) error {
	st := transport.NewStdioTarget(target.Command, target.Args, diagOut)
	return RunTarget(st, in, clientIn, clientOut, diagOut)
}

// targetErrReporter is an optional capability a transport.Target may
// implement to report why its Receive() channel closed: nil for a
// clean end, or the underlying read failure otherwise (see
// transport.StdioTarget.Err). Not part of transport.Target itself,
// since not every transport has a meaningful distinction here;
// RunTarget checks for it via a type assertion.
type targetErrReporter interface {
	Err() error
}

// targetInputCloser is an optional capability a transport.Target may
// implement to signal "no more input is coming" without waiting for
// the target's own exit — e.g. closing a spawned subprocess's stdin
// so it can exit on its own if it reads until EOF, exactly like
// closing targetStdin did in the pre-pass-1 stdio path. Not part of
// transport.Target, since a request/response transport (HTTP, added
// in a later pass) has no persistent input stream to half-close;
// RunTarget checks for it via a type assertion.
type targetInputCloser interface {
	CloseInput() error
}

// RunTarget wires clientIn -> target and target -> clientOut through
// the Interceptor for any transport.Target, and blocks until the
// target exits (Stop returns) or an unrecoverable pipe error occurs.
// Run (above) is this generalized over transport.StdioTarget — the
// stable stdio entry point; a later pass adds the equivalent HTTP
// entry point, driven through this same function.
//
// STDIO DISCIPLINE: clientOut receives ONLY JSON-RPC frames written via
// WriteFrame. Every other message (errors, the FLAG UI) goes to
// diagOut (stderr) or the controlling TTY, never here.
func RunTarget(target transport.Target, in *Interceptor, clientIn io.Reader, clientOut io.Writer, diagOut io.Writer) error {
	_ = diagOut // kept for signature symmetry with Run/StderrPrompt; the transport itself owns where its own stderr goes (see transport.NewStdioTarget)

	if err := target.Start(context.Background()); err != nil {
		return err
	}

	clientToTargetErr := make(chan error, 1)
	targetToClientDone := make(chan error, 1) // this is the goroutine that writes to clientOut — RunTarget must not return until it has actually finished
	var clientOutMu sync.Mutex                // WriteFrame calls interleave from both pump directions; guard the shared writer so frames never partially interleave on the wire.

	// Interceptor itself never touches clientOut (its job is evaluation
	// and logging, not I/O) — so RunTarget supplies the one callback
	// that needs to, bound to the same mutex the two pump goroutines
	// below already share. This must be set before either goroutine
	// starts: a tools/call forwarded in the first few lines of the
	// client pump can already be arming its timeout by the time this
	// function returns from the caller's perspective, and OnTimeout
	// must be ready by then.
	in.OnTimeout = func(id json.RawMessage, tool string, elapsed time.Duration) {
		resp, err := timeoutErrorResponse(id, tool)
		if err != nil {
			return
		}
		clientOutMu.Lock()
		_ = WriteFrame(clientOut, resp)
		clientOutMu.Unlock()
	}

	// client -> target
	go func() {
		// Signals "no more input is coming" when this pump's own loop
		// ends, for whatever reason — exactly matching the pre-pass-1
		// stdio path's `defer targetStdin.Close()`. This is deliberately
		// NOT the same thing as target.Stop() below (which waits for
		// the target to exit): a target blocked reading until EOF needs
		// this signal to ever exit on its own, independent of whether
		// the target happens to exit for some other reason first.
		defer func() {
			if ic, ok := target.(targetInputCloser); ok {
				_ = ic.CloseInput()
			}
		}()
		clientLines, clientReadErr := ReadFrames(clientIn)
		for line := range clientLines {
			forward, fwdLine, blockResp, err := in.HandleClientRequest(line)
			if err != nil {
				clientToTargetErr <- fmt.Errorf("proxy: handling client request: %w", err)
				return
			}
			if forward {
				if err := target.Send(fwdLine); err != nil {
					clientToTargetErr <- fmt.Errorf("proxy: writing to target: %w", err)
					return
				}
				continue
			}
			if blockResp != nil {
				clientOutMu.Lock()
				err := WriteFrame(clientOut, blockResp)
				clientOutMu.Unlock()
				if err != nil {
					clientToTargetErr <- fmt.Errorf("proxy: writing block response to client: %w", err)
					return
				}
			}
		}
		// The channel is closed either because clientIn was exhausted
		// cleanly (normal EOF — e.g. the client hung up) or because the
		// scanner itself failed (oversized frame, read error). Only the
		// latter is a real failure: it means bytes on the wire were lost
		// without ever being evaluated or forwarded, which is exactly
		// what Protocol Silence exists to make visible rather than let
		// pass as an ordinary clean close.
		if rerr := clientReadErr(); rerr != nil {
			_ = in.Emit(in.RunID, "proxy", "policy.silence", map[string]interface{}{
				"reason": "stream read error", "stream": "clientIn", "error": rerr.Error(),
			})
			clientToTargetErr <- fmt.Errorf("proxy: reading from client: %w", rerr)
			return
		}
		clientToTargetErr <- nil
	}()

	// target -> client
	go func() {
		targetLines, err := target.Receive()
		if err != nil {
			targetToClientDone <- fmt.Errorf("proxy: receiving from target: %w", err)
			return
		}
		for line := range targetLines {
			fwdLine, err := in.HandleTargetResponse(line)
			if err != nil {
				targetToClientDone <- fmt.Errorf("proxy: handling target response: %w", err)
				return
			}
			if fwdLine == nil {
				// A late response to a call that already timed out
				// (see Interceptor.resolvePending): the client already
				// has its structured mcp_call_timeout error for this
				// id, so this frame is deliberately dropped, not
				// forwarded.
				continue
			}
			clientOutMu.Lock()
			werr := WriteFrame(clientOut, fwdLine)
			clientOutMu.Unlock()
			if werr != nil {
				targetToClientDone <- fmt.Errorf("proxy: writing to client: %w", werr)
				return
			}
		}
		// Same distinction as the client -> target pump above: a clean
		// close here is the target exiting normally (the expected,
		// common case handled below via target.Stop()); a non-nil error
		// means the target's own read stream failed mid-read. Not every
		// transport.Target distinguishes the two (see targetErrReporter),
		// so this is an optional check.
		if er, ok := target.(targetErrReporter); ok {
			if rerr := er.Err(); rerr != nil {
				_ = in.Emit(in.RunID, "proxy", "policy.silence", map[string]interface{}{
					"reason": "stream read error", "stream": "targetStdout", "error": rerr.Error(),
				})
				targetToClientDone <- fmt.Errorf("proxy: reading from target: %w", rerr)
				return
			}
		}
		targetToClientDone <- nil
	}()

	waitErr := target.Stop()

	// The target has exited, which closes its Receive channel and makes
	// the target -> client goroutine's loop end shortly — but "shortly"
	// is not "already", and that goroutine is the only other writer to
	// clientOut. RunTarget must not return (and the caller must not
	// treat clientOut as final) until it actually has. This is a real
	// fix, not a defensive guess: without this blocking receive, a
	// caller (or a test) reading clientOut right after RunTarget returns
	// can race the final in-flight WriteFrame — see the race this closes
	// in proxy_test.go.
	drainErr := <-targetToClientDone
	if drainErr != nil {
		return drainErr
	}

	// Symmetric to the drain above, but for mcp_call_timeout_seconds:
	// every timer armTimeout started must be accounted for — fired, or
	// canceled by a real response — before RunTarget treats clientOut as
	// final. This returns immediately once that's true; it does not
	// wait out the full timeout duration for calls that already
	// resolved normally (see Interceptor.resolvePending's Stop() path).
	in.WaitPendingTimeouts()

	// The client -> target direction has no such deadline in real usage
	// (a live MCP client's stdin may stay open long after the target
	// it was talking to exits), so this check stays non-blocking: pick
	// up an error if the goroutine already finished, but never wait on
	// it here.
	select {
	case pumpErr := <-clientToTargetErr:
		if pumpErr != nil {
			return pumpErr
		}
	default:
	}
	return waitErr
}

// StderrPrompt is a PromptFunc that renders the FLAG UI ([Allow once |
// Allow session | Deny]) on the controlling TTY when available, falling
// back to stderr-only (non-interactive: always Deny) otherwise. It never
// touches stdout. timeout bounds how long it waits for a human response
// before returning Timeout (fail closed) — see gate.PromptWithTimeout.
func StderrPrompt(diagOut io.Writer, timeout time.Duration) PromptFunc {
	return func(toolName, reason, argsPreview string, allowSessionOffered bool) (Decision, error) {
		tty, err := gate.OpenControllingTTY()
		if err != nil {
			fmt.Fprintf(diagOut, "centrol: FLAGGED %s (%s) — no interactive terminal available, denying\n", toolName, reason)
			return Deny, nil
		}
		defer tty.Close()

		options := []gate.Option{gate.Opt("Allow once")}
		if allowSessionOffered {
			options = append(options, gate.Opt("Allow session"))
		}
		options = append(options, gate.Opt("Deny"))

		title := "centrol: FLAGGED tool call"
		detail := fmt.Sprintf("tool:   %s\n  reason: %s\n  args:   %s", toolName, reason, argsPreview)

		decision, err := gate.PromptWithTimeout(tty, tty, title, detail, options, timeout)
		if err == gate.ErrPromptTimeout {
			return Timeout, nil
		}
		if err != nil {
			return Deny, err
		}
		if decision < 0 || int(decision) >= len(options) {
			return Deny, nil // gate.NoDecision or anything unmatched fails closed
		}
		switch options[decision].Display {
		case "Allow once":
			return AllowOnce, nil
		case "Allow session":
			return AllowSession, nil
		default:
			return Deny, nil
		}
	}
}
