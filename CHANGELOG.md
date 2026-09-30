# Changelog

## v0.1.0 — 2026-09-29

First release. Centrol wraps coding agents with an accountability layer:
a hash-chained record of what an agent did, and a one-command rollback.

### What works

- Every governed action — every file write, every MCP tool call — lands
  in an append-only, hash-chained ledger, whether or not you ever act on
  it; `centrol audit` views it, `--verify` proves it hasn't been
  tampered with across segments. This runs underneath everything else
  below, on every run, unconditionally.
- `centrol undo` — restores the pre-run snapshot, including untracked
  files present at snapshot time. Recovers uncommitted work an agent
  clobbered that plain git has no memory of (git's reflog only tracks
  commits/stashes, never working-tree-only changes). Takes its own
  pre-undo snapshot first, so a failed rollback is itself recoverable.
- `centrol proxy` — fronts MCP servers and hard-blocks a tool call that
  resolves to a credential or system path *before* it reaches the
  server — the caller gets a structured denial back, not silence.
- `centrol guard` — snapshots before a run and watches the filesystem
  for the duration; every write is logged, an out-of-scope one is
  flagged, feeding both the ledger above and `undo`.
- `centrol guard --observe` / `centrol proxy --observe` — evaluate
  policy without ever prompting or blocking; logs would_flag/would_block
- `centrol config` — interactive menu for allowlist, thresholds, block
  paths, observe mode; shows which source wins for every key
- Free, offline, no telemetry, no account required
- Builds clean for darwin/linux (amd64, arm64); packaged installs ship
  for darwin and linux today

### What doesn't work yet

- `centrol verify` (code validator) — reserved in the schema, stubs at
  the CLI, ships in v0.3
- HTTP/SSE transport for the proxy — stdio only today
- Auto-approve tiers 2 and 3 — every flagged action goes to human review
- Windows — compiles for windows/amd64, but there's no packaged install
  yet and interactive agent sessions are untested; treat it as
  unsupported until that changes

### Known limitations

- `centrol guard` is an accountability layer, not a sandbox. It logs and
  reverses; it does not prevent a determined agent from acting outside
  the governed interfaces. `centrol proxy` is the surface that blocks.
- SIGKILL (kill -9) cannot be trapped, so the run summary is lost on
  force-kill. The ledger entry is complete regardless.
- `centrol undo` restores tracked git state and untracked files present
  at snapshot time. It does not restore ignored files, databases, or
  external state, and reports what it cannot restore rather than
  dropping it silently.

### What's next

Active development continues. Direction is documented in the
README's roadmap section.

### Install

    curl -fsSL https://raw.githubusercontent.com/baitelmal/centrol/main/install-centrol.sh | sh

See the README for the full walkthrough.
