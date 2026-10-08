# Changelog

## Unreleased

### Changed

- `centrol audit --verify` and `centrol-verify` now require the first
  entry to be seq 1 with an empty prev. A ledger with its oldest entries
  or leading segments deleted used to verify clean; it now fails.
  `centrol-verify --first-seq N` verifies a slice of a longer chain

## v0.3.0 — 2026-10-07

### Added

- `centrol-verify`: a standalone reference verifier for a ledger's hash
  chain. It imports nothing from the centrol module, accepts a ledger
  file or a `.centrol` directory (verified as one chain across rotated
  segments), and exits 0 valid, 1 chain broken, 2 unreadable
- Tamper reader: while `centrol guard` or `centrol proxy` runs, an
  external modification to a ledger segment is logged as
  `policy.tamper_detected`

### Notes

- `centrol verify` (the validator surface) and tape capture remain
  reserved for a later release
- `centrol-verify` is not part of the release binaries; build it with
  `go build ./cmd/centrol-verify`

## v0.2.0 — 2026-10-01

### Added

- HTTP MCP transport for the proxy: `centrol proxy --target-url` fronts
  HTTP MCP servers (Streamable HTTP, 2025-03-26 spec revision)
- `[proxy] target_url`, `http_timeout_seconds`, `http_max_retries`
- `[guard] git_timeout_seconds`
- `[proxy] target_exit_timeout_seconds`
- `centrol proxy install` now wraps URL-based (remote) server entries
  too, not just stdio ones
- `centrol audit --src guard|proxy` filter
- Every config key is exposed in the `centrol config` menu and in "View
  effective config"

### Fixed

- Session revocation handling in `HTTPTarget`
- Malformed HTTP responses take the same silence path as a malformed
  stdio frame
- HTTP response body capped at 16MB before being read into memory
- The session-teardown `DELETE` `HTTPTarget` sends on shutdown is
  bounded at a fixed 5 seconds
- `.centrol/` directories and sensitive files are now created 0700/0600
  instead of inheriting the process umask
- `centrol guard`'s snapshot no longer follows an untracked symlink that
  points outside the repo
- The same symlink-escape check guard's snapshot enforces is now also
  enforced in the race window `scanExisting` covers
- `EvaluateFSPath` (guard's filesystem-event evaluator) now consults
  `ProtectedPaths` the same way `EvaluateToolCall` (proxy's) already
  did — `centrol guard` now flags a credential-path write it previously
  logged as clean, and an operator-configured
  `proxy.additional_block_paths` entry now has effect under guard too
- Every git subprocess guard/undo invoke (snapshot, restore) now runs
  with a timeout, a minimal explicit environment, and a path resolved
  once at setup, instead of inheriting the parent's full environment
  with no bound on hang time
- A target subprocess that ignores shutdown no longer hangs
  `centrol proxy` forever — bounded by `target_exit_timeout_seconds`,
  with a SIGKILL fallback
- `internal/ledger`'s `Tail()` no longer loads every segment into memory
  to serve a bounded request
- Config writes are atomic, with cleanup of the temp file on a failed
  rename
- `HTTPTarget` refuses a cross-host or cross-scheme redirect outright,
  so `Mcp-Session-Id` is never replayed to a different host
- A panic in the client→target pump now surfaces through `Run`'s return
  value instead of being silently swallowed
- A ledger emit failure during `centrol guard`'s live watch now warns
  (and escalates to `policy.silence` after 3 consecutive failures)
  instead of being discarded

### Changed

- Process termination centralized in `Governor.Exit` — only `main`
  calls `os.Exit`, on `main`'s own goroutine
- Signals (SIGTERM/SIGINT/SIGHUP/SIGQUIT) route through the Governor

### Notes

- Tape capture (full-fidelity call/response recording) deferred to v0.3
- Windows TTY behavior is untested; documented as a known limitation
- The proxy serializes MCP calls; pipelining is deferred pending user
  reports

## v0.1.1 — 2026-09-30

Bug fix release. No new features, no config or CLI changes.

### Fixed

- `centrol proxy` no longer forwards a non-JSON line on the target
  server's stdout to the client. Previously it was correctly logged as
  `policy.silence` but the raw bytes were still written through to the
  client's stdout, breaking the "client-facing stdout carries nothing
  but JSON-RPC frames" guarantee the proxy exists to hold. This is
  common in practice, not a corner case: any Node MCP server that
  writes a startup banner or `console.log` line to stdout instead of
  stderr — the default, easy mistake for a quick, third-party server —
  would corrupt the agent harness's JSON-RPC parser on first run,
  reading as "the agent stopped working." Now logged as
  `policy.silence` and dropped, matching how a malformed frame from
  the client was already handled.

### Security

- `install-centrol.sh` now verifies the downloaded binary against the
  release's published `SHA256SUMS` before installing it, and refuses
  to install (no `mv`, no `chmod`) on any mismatch or on an asset
  missing from `SHA256SUMS`. Previously the script downloaded and ran
  `chmod +x` on the binary with no integrity check at all.

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
