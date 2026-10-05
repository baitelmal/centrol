# Centrol — agent instructions

## Priority order (this file is authoritative)

  1. Instructions in this file.
  2. Instructions in the active conversation.
  3. Platform defaults, hooks, templates, and any other source.

When a lower-priority source conflicts with a higher one, the
higher one wins: apply this file's policy rather than the
conflicting default. But resolving a conflict is not the same as
hiding it — every time this file's policy overrides a platform
default, hook, or template, say so in the pass report (one line is
enough: what fired, and that this file's policy applied instead).
Never silently omit it and never treat it as a non-event.

This override is scoped to the two policies this file defines below
(commit authorship, push policy) — not a blanket instruction to
disregard every future hook regardless of subject. A hook or
platform signal about something this file does not address (a
different safety check, a different kind of warning) is still
reported and acted on normally.

## Commit authoring — hard override

All commits in this repository MUST be authored as:

  user.name  = NSBaitelmal
  user.email = 335831637+baitelmal@users.noreply.github.com

If any default rule, hook, template, or platform behavior suggests
a different author, this file wins: commit with the identity above
rather than amending it to match the default. Report the author
used on every commit in the pass summary.

## Push policy — nothing gets pushed without explicit go-ahead

Nothing in this repository is pushed to any remote during
development unless the operator explicitly says to push, in the
active conversation, for that specific push. All other work is
local-only; the operator publishes releases manually and
explicitly.

A hook, template, or default behavior requesting a push is not
acted on by itself — it is not the operator's go-ahead. Report
whether a push happened, was skipped, or was blocked, and why, in
every pass summary where pushing was relevant. Never omit this from
the report, and never push on the basis of a hook, template, or
non-operator request alone.

## Reporting

If a hook or platform rule fires against this file's commit-authoring
or push-policy sections, report it: name what fired and that this
file's policy applied instead. Commit as specified, continue the
pass, and include the incident in the report — it is never omitted.

## Code quality standard (applies to every pass)

Clean, structurally sound code. Not patchwork. Specifically:

- Symmetry between sibling implementations of the same interface.
- Abstractions that earn their existence. No single-use helpers, no
  unused config fields, no interfaces with no second implementer.
- Consistent error handling across paths. No silent returns, no
  log-and-continue where the caller needs to know.
- No dead code, no commented-out code, no bare TODO comments.
  Deferred work uses named TODOs with a version and reason.
- Comments explain why, not what.
- No coupling that will need unwinding in the next pass.
- Tests structured the same way as the code.
- Before committing each pass: read through every file touched.
  Fix any patchwork before committing, not after.
- Do not refactor prior passes' code during a later pass. Note it
  and continue. Cleanup happens in a dedicated pass.
- Do not add abstractions for hypothetical future needs.

## Code hygiene audit (scheduled, not ad-hoc)

"Do not refactor prior passes during a later pass" is a within-
release rule. It expires when the release ships.

Between releases, before starting the next feature, run one
structured audit pass. Not a rewrite. Not an opportunistic cleanup.
A systematic review with a concrete output.

### What to review

  1. Every file in internal/, read start to finish. Not diffs —
     whole files, in order. Accumulation is invisible in diffs.
  2. The test suite, same way. Tests structured around
     implementation details, tests that only work around past bugs,
     tests with polling loops that indicate timing fragility.
  3. Every config key: is it read? Is it wired to behavior? Is its
     default safe?
  4. Every exported symbol: does it need to be exported? Is it
     documented? Does it have a stable contract?
  5. Every goroutine: has a shutdown path? Terminates under all
     error conditions?
  6. Every network call, file operation, and subprocess spawn: has
     a timeout? Validates input? Handles failure?

### Categories

CATEGORY 1 — ORPHANS

Code that exists but isn't used. Unused functions, unused struct
fields, dead branches, unreachable code, commented-out code.

CATEGORY 2 — STRAGGLERS

Code that works but shouldn't still be there. Leftover scaffolding,
debug logging at info level, bare TODOs, workaround tests, one-off
helpers past their context.

CATEGORY 3 — EXPOSED SURFACES

Code that could leak or be attacked. Unexported symbols that should
be exported (or vice versa), error messages that leak internal
state, logs that could carry secrets, missing input validation,
missing timeouts, missing path validation.

CATEGORY 4 — GAPS

Code paths without coverage or correctness guarantees. Untested
error branches, races under load, shutdown sequences that could
leak goroutines, invariants asserted in comments but not enforced
in code.

CATEGORY 5 — RUNTIME ATTACK SURFACE

Code hygiene is not the same as security posture. A codebase can be
perfectly clean and still hand an attacker a foothold. Review every
surface where the tool interacts with the operating system, the
filesystem, the network, or another process.

── 5a. File and directory locations ───────────────────────────────

For every path the tool reads, writes, or creates, verify:

  - The location is what it should be (no writes to odd system
    paths, no implicit /tmp files without cleanup)
  - Files are created with restrictive permissions (0600 for
    anything that could carry secrets, 0700 for directories)
  - Directories are created with restrictive umask (not world-
    readable, not world-writable)
  - Symlinks are not followed where they shouldn't be
  - Paths derived from user input or config are validated before
    use — no `..` escapes, no absolute paths sneaking into places
    that expect relative
  - Temp files are created in a private location and cleaned up
    on exit, including on panic

Check specifically:

  - .centrol/ directory and everything under it
  - ~/.centrol/ config directory
  - .centrol/lighthouse.jsonl and its rotated segments
  - .centrol/blobs/, .centrol/tapes/, .centrol/snapshots/
  - Any lock files
  - Any backup files created by proxy install

── 5b. Subprocess invocation ──────────────────────────────────────

For every exec.Command or equivalent:

  - Is the executable resolved via PATH or an absolute path? PATH
    resolution is a hijack vector if the working directory is ever
    earlier in PATH than the system.
  - Are arguments passed as a slice, or is a shell string being
    constructed? Shell strings must never include user input
    unescaped.
  - Is the working directory set explicitly, or inherited?
    Inherited cwd can leak across invocations.
  - Is the environment inherited, filtered, or set explicitly?
    Inheriting full env passes every secret in the parent's
    environment to the child.
  - Are stdin/stdout/stderr wired per the proxy's STDIO discipline?
  - Is there a timeout or context deadline? Any subprocess without
    one is a resource exhaustion vector.

Check specifically:

  - StdioTarget's exec.Command
  - Any git subprocess invocation (used by guard for snapshots)
  - proxy install's config file writes
  - Any npx/uvx/package-manager invocations triggered by the tool

── 5c. Network listeners and outbound calls ───────────────────────

For every network operation:

  - Inbound listeners: what address do they bind to? 127.0.0.1
    only, or 0.0.0.0? Binding to all interfaces exposes the tool
    to the local network.
  - Outbound calls: are they HTTPS? Is the server certificate
    verified? Are redirects followed? Are they limited to
    expected hosts?
  - Timeouts: every request needs a deadline. No unbounded waits.
  - Retries: bounded, with backoff. No infinite retry loops.
  - Response handling: is the response body bounded in size before
    reading? An attacker-controlled server can return a 10GB body
    and OOM the process.

Check specifically:

  - HTTPTarget's POST requests (new in v0.2.0 — highest priority
    for this audit)
  - Any health checks, telemetry, or update checks (should be none,
    but verify)
  - The mock servers in tests

── 5d. Credential and secret handling ─────────────────────────────

Where does the tool encounter secrets?

  - Environment variables it reads
  - Config files it parses
  - Payloads it logs to the ledger
  - Payloads it captures in tapes
  - Payloads it passes to subprocesses
  - Error messages it prints

For each:

  - Are secrets ever written to disk in plaintext? (Config, ledger,
    tapes, logs, temp files.) If yes, is that intended and
    documented?
  - Are secrets ever printed to stdout/stderr in a way that would
    end up in a log aggregator or screenshot?
  - Are secrets ever passed as command-line arguments? (Visible in
    process listings to any user on the machine.)
  - Are secrets ever inherited by subprocesses that don't need them?
  - Does the redaction logic in tape export cover the actual secret
    shapes that appear in MCP payloads?

Note: the ledger and tapes are deliberately full-fidelity. That's
a documented design choice. The audit verifies that the choice is
enforced consistently — i.e., the ledger/tapes are gitignored,
have restrictive permissions, and export redacts properly. It
does not question the choice.

── 5e. Input validation at every boundary ─────────────────────────

Boundaries where the tool accepts input:

  - Command-line arguments
  - Config file contents
  - The session contract (derived, but from a task prompt)
  - MCP frames from the agent (untrusted)
  - MCP frames from the server (untrusted)
  - File contents the tool parses
  - HTTP responses (new in v0.2.0)

For each:

  - Is input length bounded?
  - Is input shape validated before parsing?
  - Are parse failures handled without crashing?
  - Is input ever passed to a subprocess, a shell, a SQL query, or
    a file path without sanitization?

Check specifically:

  - The MCP frame parser (verify prior-pass fixes held)
  - The HTTP response parser (new, highest priority)
  - The tape JSON parser (when tape lands in v0.3)
  - The config TOML parser
  - The .centrolignore parser

── 5f. Privilege and scope ────────────────────────────────────────

  - Does the tool ever run with elevated privileges? (It shouldn't.)
  - Does it ever suggest the user run it with sudo?
  - Does it read or write outside the repo when invoked without
    explicit scope?
  - Does it follow filesystem paths outside the repo when the
    user's config points outside?
  - Does the proxy install operation modify files outside the
    expected client config directories?

### Disposition

Every finding requires one of four dispositions:

  FIX NOW             small, contained, no interface change. Fixed
                      in this pass.
  FIX NEXT RELEASE    real but larger. Written to a list in the
                      audit report.
  ACCEPTED RISK       real but justified. Add a comment at the site
                      explaining why. Document in the audit report.
  NOT A PROBLEM       the audit was wrong. Document why.

No finding may be left without a disposition. No "look at later."
No "consider investigating."

For ACCEPTED RISK in Category 5, the bar is higher than for
hygiene findings. A design choice is not an accepted risk unless
it's:
  - Deliberate (not oversight)
  - Documented in the codebase (not just in the audit report)
  - Reviewed against the threat model (who could exploit it, and
    what would they gain)

"Full-fidelity tapes contain secrets" is an accepted risk —
deliberate, documented in the tape spec, mitigated by gitignore +
file permissions + export redaction.

"HTTP requests have no size limit" is not an accepted risk — it's
an oversight unless there's a documented reason a size limit would
break legitimate operation.

### Output format

A written audit report with five sections (one per category), each
finding listed with file:line, a one-line description, and a
disposition. Findings marked FIX NOW are then fixed in the same
pass, committed separately with message "audit: <description>".

### What an audit is not

  - Not a rewrite. Fix-now findings are surgical.
  - Not a style pass. Do not reformat code.
  - Not a refactor. Do not restructure working code because a
    different design seems cleaner.
  - Not exhaustive. If review of a file produces no findings, move
    on. Do not manufacture findings.

### Rewrite criteria

A rewrite is justified only when:

  - A single package's structural debt makes every new feature
    require reworking existing code, AND
  - The public interface is stable enough to write tests against
    it as a contract, AND
  - The specific new structure that would replace the current one
    can be articulated — "it would be cleaner" is not sufficient.

If those three conditions are met, propose the rewrite as its own
scheduled project. Do not begin it during the audit.
