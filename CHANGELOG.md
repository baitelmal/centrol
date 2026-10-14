# Changelog

## v0.4.0 — 2026-10-14

### Added

- Structured MCP policy: `~/.centrol/allowlist.json` (yours) and
  `~/.centrol/enterprise-policy.json` (written by the enterprise tier).
  Both list the MCP servers allowed and, per server, the tools it may be
  called with (`tools`) and the paths its calls may touch (`paths`, with
  `*` and `**` globs). Absent `tools`/`paths` mean unrestricted; an empty
  list means nothing is allowed. If the enterprise file exists it is the
  policy, else `allowlist.json`, else `config.toml`'s `allowed_servers`
  as before. An active policy is absolute: its server list is the
  allowlist and `config.toml`'s list is ignored
- Call-time enforcement: with a policy active, `centrol proxy` refuses a
  tool call to an unlisted server, an unlisted tool, or a path outside
  the server's `paths`, before the existing checks run. Policies only
  narrow what centrol already allows; they cannot grant a path the repo
  scope or the protected paths refuse
- The enterprise policy's values show as locked in `centrol config`; the
  provenance name `local_allowlist` is new alongside `enterprise_policy`
- An HTTP target's identity for policy matching is its URL host, with
  the port if given

- Signed enterprise policy. `enterprise-policy.json` carries a
  top-level `"signature"` (base64 ed25519 over the canonical JSON of the
  file without it). With a pinned `~/.centrol/enterprise-pubkey` present
  it is verified on load, and each verified policy is copied to
  `~/.centrol/enterprise-policy.verified.json`. A policy that fails
  verification does not lock the station out: the last verified policy
  stays in force, so a single tamper is inert. Fail-closed only when
  that copy is also missing or invalid (the error names both files); a
  pubkey with no policy ever pushed permits no servers. The pubkey
  arrives with the first policy, not at enrollment, so solo to managed is
  atomic. With no pubkey nothing changes, and `allowlist.json` is
  untouched. Same-user tampering is recorded, not prevented: all three
  files can be overwritten, a coordinated overwrite is visible in the
  ledger, and prevention is sandboxing

### Changed

- With a policy active, an unlisted server is refused at launch with no
  prompt, and the message says whose policy it is and where to add the
  server ("Add to allowlist" would write a `config.toml` the policy
  shadows). `centrol config` likewise points at the file instead of
  offering to add a server. With no policy file, the prompt is
  unchanged
- A policy file that exists but cannot be read or parsed fails closed:
  centrol stops with an error naming the file rather than falling back
  to a more permissive source. The parser is strict (unknown fields are
  errors), so a field added by a later enterprise release fails closed
  on an older centrol until it is upgraded
- `Contract.AllowedToolPatterns`, declared since v0.1 and never used, now
  carries the policy's `tools` for the server being fronted. Despite the
  name, entries are exact tool names, not patterns; the name is kept
  from the declared contract shape

### Security

- The files that decide what the agent may do are now protected:
  `~/.centrol/enterprise-policy.json`, `enterprise-pubkey`,
  `enterprise-policy.verified.json`, `allowlist.json` and
  `config.toml` are in the built-in protected set, so an MCP tool call
  that names one is refused, and `centrol guard` watches them, so a
  direct write by the agent process is recorded as a block-tier
  `policy.violation`. A direct write cannot be refused, because the proxy
  sees tool calls only; it is detected. `centrol guard` creates
  `~/.centrol` (mode 0700) if it does not exist, so a policy file created
  from nothing is seen too
- The repo's own `.centrol/config.toml` is protected and watched the
  same way. It outranks the user config, and the guard excludes the
  repo's `.centrol` directory because the ledger lives there, so the
  file is carved out individually: its directory is watched without
  recursion and only events for that file are reported. Ledger segments
  (`lighthouse.jsonl`, `lighthouse.NNN.jsonl`) and everything else in
  the directory stay excluded, so the ledger writing to itself produces
  no events
- Config is read once, when `centrol proxy` or `centrol guard` starts,
  never per tool call. That includes `general.observe_mode`: a write to
  the repo config takes effect the next time centrol launches, not
  mid-run. MCP clients start a fresh proxy per session, so a changed
  file applies from the next session. This is why a write is a
  detection and review matter rather than an immediate bypass
- Keys in the repo `config.toml` that can weaken enforcement if an
  agent writes them, now covered by the protection above:
  `general.observe_mode = true` (evaluates, logs, never blocks),
  `proxy.allowed_servers` (the server allowlist when no policy file is
  active), `proxy.additional_block_paths` (the repo list replaces the
  user list, so `[]` drops extra blocks; the built-in protected set is
  not affected), `proxy.target_url` (an HTTP target skips the launch-time
  allowlist check), `proxy.strict_matching = false`,
  `gate.allow_session_amend = true`, and `proxy.mass_mutation_threshold`
  (raised up to 1000). `guard.watcher_debounce_ms` has no upper bound and
  is a minor lever. `gate.prompt_timeout_seconds` (floor 30), the
  timeout and retry keys and `general.log_level` cannot weaken
  enforcement. An active policy file still overrides the allowlist keys

### Notes

- With no `HOME`, no policy file is looked for and the flat
  `config.toml` allowlist applies, so minimal containers keep working
- `tools/list` is not filtered; a tool outside the policy still appears
  to the client and is refused when called

## v0.3.0 — 2026-10-07

### Added

- `centrol-verify`: a standalone reference verifier for a ledger's hash
  chain. It imports nothing from the centrol module, accepts a ledger
  file or a `.centrol` directory (verified as one chain across rotated
  segments), and exits 0 valid, 1 chain broken, 2 unreadable
- Tamper reader: while `centrol guard` or `centrol proxy` runs, an
  external modification to a ledger segment is logged as
  `policy.tamper_detected`
- `centrol-verify` now ships as a release binary (previously
  build-from-source)
- Windows support (amd64 and arm64): release binaries and
  `install-centrol.ps1`, which installs both binaries per user after
  checking them against `SHA256SUMS`. The macOS/Linux install script now
  installs `centrol-verify` too
- `centrol --version` (and `-v`) prints the release version

### Changed

- `centrol audit --verify` now requires the first entry to be seq 1
  with an empty prev. A ledger with its oldest entries or leading
  segments deleted used to verify clean; it now fails. `centrol-verify`
  applies the same rule, and `centrol-verify --first-seq N` verifies a
  slice of a longer chain

### Fixed

- `centrol proxy install claude-desktop` on Windows now finds the config
  of the Microsoft Store (MSIX) Claude Desktop, which lives inside the
  package folder, and prefers it over a classic config when both exist.
  If the Store app has not created its config yet, the error says where
  it will be and what to do
- `centrol guard` in a repo with no commits yet now says so, instead of
  printing the raw `git rev-parse` error
- A wrapped command that is not found on Windows now adds a hint that it
  must be a real executable, and that shell builtins need `cmd /c`
- A guard that fails during its snapshot no longer prints a "Run
  complete" summary before the error

### Notes

- `centrol verify` (the validator surface) and tape capture remain
  reserved for a later release

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
