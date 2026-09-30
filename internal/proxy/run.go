package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/scirem/centrol/internal/gate"
)

// Target describes how to reach the real MCP server this proxy fronts.
type Target struct {
	Command string
	Args    []string
}

// Run spawns the target as a child process, wires clientIn -> target and
// target -> clientOut through the Interceptor, and blocks until the
// target exits or an unrecoverable pipe error occurs.
//
// STDIO DISCIPLINE: clientOut receives ONLY JSON-RPC frames written via
// WriteFrame. Every other message (errors, the FLAG UI) goes to
// diagOut (stderr) or the controlling TTY, never here.
func Run(target Target, in *Interceptor, clientIn io.Reader, clientOut io.Writer, diagOut io.Writer) error {
	cmd := exec.Command(target.Command, target.Args...)
	cmd.Stderr = diagOut // the target's own stderr is diagnostic, never client-facing stdout

	targetStdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("proxy: target stdin pipe: %w", err)
	}
	targetStdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("proxy: target stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("proxy: starting target %q: %w", target.Command, err)
	}

	clientToTargetErr := make(chan error, 1)
	targetToClientDone := make(chan error, 1) // this is the goroutine that writes to clientOut — Run must not return until it has actually finished
	var clientOutMu sync.Mutex                // WriteFrame calls interleave from both pump directions; guard the shared writer so frames never partially interleave on the wire.

	// Interceptor itself never touches clientOut (its job is evaluation
	// and logging, not I/O) — so Run supplies the one callback that
	// needs to, bound to the same mutex the two pump goroutines below
	// already share. This must be set before either goroutine starts:
	// a tools/call forwarded in the first few lines of the client pump
	// can already be arming its timeout by the time this function
	// returns from Run's caller's perspective, and OnTimeout must be
	// ready by then.
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
		defer targetStdin.Close()
		clientLines, clientReadErr := ReadFrames(clientIn)
		for line := range clientLines {
			forward, fwdLine, blockResp, err := in.HandleClientRequest(line)
			if err != nil {
				clientToTargetErr <- fmt.Errorf("proxy: handling client request: %w", err)
				return
			}
			if forward {
				if err := WriteFrame(targetStdin, fwdLine); err != nil {
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
		targetLines, targetReadErr := ReadFrames(targetStdout)
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
		// common case handled below via cmd.Wait()); a non-nil error
		// means the target's stdout stream itself failed mid-read.
		if rerr := targetReadErr(); rerr != nil {
			_ = in.Emit(in.RunID, "proxy", "policy.silence", map[string]interface{}{
				"reason": "stream read error", "stream": "targetStdout", "error": rerr.Error(),
			})
			targetToClientDone <- fmt.Errorf("proxy: reading from target: %w", rerr)
			return
		}
		targetToClientDone <- nil
	}()

	waitErr := cmd.Wait()

	// The target has exited, which closes targetStdout and makes the
	// target -> client goroutine's ReadFrames loop end shortly — but
	// "shortly" is not "already", and that goroutine is the only other
	// writer to clientOut. Run must not return (and the caller must not
	// treat clientOut as final) until it actually has. This is a real
	// fix, not a defensive guess: without this blocking receive, a
	// caller (or a test) reading clientOut right after Run returns can
	// race the final in-flight WriteFrame — see the race this closes in
	// proxy_test.go.
	drainErr := <-targetToClientDone
	if drainErr != nil {
		return drainErr
	}

	// Symmetric to the drain above, but for mcp_call_timeout_seconds:
	// every timer armTimeout started must be accounted for — fired, or
	// canceled by a real response — before Run treats clientOut as
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
