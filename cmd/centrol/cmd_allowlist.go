package main

import (
	"fmt"
	"os"

	"github.com/scirem/centrol/internal/gate"
)

type allowlistDecision int

const (
	allowlistDeny allowlistDecision = iota
	allowlistAllowOnce
	allowlistAllowOnceUnattended
	allowlistAllowSession
	allowlistAdd
)

func (d allowlistDecision) String() string {
	switch d {
	case allowlistAllowOnce:
		return "allow_once"
	case allowlistAllowOnceUnattended:
		return "allow_once (no interactive terminal)"
	case allowlistAllowSession:
		return "allow_session"
	case allowlistAdd:
		return "add_to_allowlist"
	default:
		return "deny"
	}
}

// promptServerAllowlist renders the four-option FLAG UI for an unknown
// MCP server: [Allow once | Allow session | Add to allowlist | Deny].
//
// When no interactive terminal is available, this allows once rather
// than denying — deliberately different from the per-tool-call FLAG
// prompt's fail-closed default. `centrol proxy` is normally launched BY
// an MCP client (Claude Desktop, Cursor, Windsurf) with stdin wired to
// the JSON-RPC stream, never a TTY — that is the common case for this
// command, not an edge case, and it's exactly what `centrol proxy
// install` sets client configs up to do. Denying the whole server
// connection by default would silently break every such client on
// first use, which is a worse failure than proceeding once and leaving
// an auditable trail: the decision is still logged as policy.amend with
// scope "allow_once (no interactive terminal)", so `centrol audit`
// shows every unreviewed server, and `centrol config` or `centrol
// proxy install` remain the way to allowlist one permanently ahead of
// time for unattended use.
func promptServerAllowlist(identity, fullCommand string) allowlistDecision {
	tty, err := gate.OpenControllingTTY()
	if err != nil {
		fmt.Fprintf(os.Stderr, "centrol: unknown MCP server %q — no interactive terminal, allowing this run (logged)\n", identity)
		fmt.Fprintf(os.Stderr, "  allowlist it permanently ahead of time with: centrol config\n")
		return allowlistAllowOnceUnattended
	}
	defer tty.Close()

	options := []gate.Option{
		gate.Opt("Allow once"), gate.Opt("Allow session"), gate.Opt("Add to allowlist"), gate.Opt("Deny"),
	}
	decision, err := gate.Prompt(tty, tty,
		"⚠ Unknown MCP server",
		fmt.Sprintf("%s\n  full command: %s", identity, fullCommand),
		options,
	)
	if err != nil || decision == gate.NoDecision {
		return allowlistDeny
	}
	switch options[decision].Display {
	case "Allow once":
		return allowlistAllowOnce
	case "Allow session":
		return allowlistAllowSession
	case "Add to allowlist":
		return allowlistAdd
	default:
		return allowlistDeny
	}
}
