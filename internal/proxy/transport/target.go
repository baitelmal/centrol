// Package transport defines how centrol proxy reaches the real MCP
// server it fronts, independent of the governance layer (policy
// evaluation, ledger emit, MCP call timeout tracking) that drives it.
//
// v0.2.0 pass 1 introduces this package as a pure refactor: StdioTarget
// wraps the exec.Command-based subprocess path centrol proxy already
// had, moved here verbatim with no observable behavior change. A
// later pass (v0.2.0 pass 2) adds HTTPTarget for MCP servers reached
// over HTTP; both are driven identically by proxy.RunTarget through
// the Target interface below.
package transport

import "context"

// Target is the abstraction proxy.RunTarget drives regardless of how
// the real MCP server is reached. Send/Receive carry whole JSON-RPC
// frames: one frame per line, without a trailing newline, matching
// centrol proxy's existing wire framing.
type Target interface {
	// Start prepares the target to exchange frames — spawning a
	// subprocess for StdioTarget, or just recording ctx for an
	// HTTP-based target that has no persistent connection to open.
	Start(ctx context.Context) error

	// Send delivers one JSON-RPC frame (without a trailing newline) to
	// the target.
	Send(frame []byte) error

	// Receive returns the channel of frames arriving from the target.
	// Safe to call more than once; every call returns the same
	// channel. The channel closes when the target's response stream
	// ends, cleanly or otherwise.
	Receive() (<-chan []byte, error)

	// Stop releases whatever Start acquired and reports the target's
	// own exit/cleanup outcome — waiting for a spawned subprocess to
	// exit for StdioTarget.
	Stop() error
}
