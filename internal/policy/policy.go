// Package policy implements the session contract (the ephemeral,
// per-run blueprint minted at run start) and the three policy tiers that
// guard and proxy evaluate actions against: AUTO-ALLOW, FLAG, HARD BLOCK.
package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tier is the outcome of evaluating one action against the contract.
type Tier int

const (
	Allow Tier = iota
	Flag
	Block
)

func (t Tier) String() string {
	switch t {
	case Allow:
		return "allow"
	case Flag:
		return "flag"
	case Block:
		return "block"
	default:
		return "unknown"
	}
}

// Kind identifies which surface a Contract was minted for. Each kind
// carries its own slice of the payload shape; the base fields below are
// common to all three.
type Kind string

const (
	KindGuard  Kind = "guard"
	KindProxy  Kind = "proxy"
	KindVerify Kind = "verify" // reserved for the v0.3 validator surface; nothing constructs one yet
)

// Contract is the ephemeral, single-use blueprint minted at run start by
// the harness (never declared by the agent itself), delivered to guard,
// proxy, or (in v0.3) verify for the duration of one run, and purged
// from memory when the run ends. Nothing about it persists between runs
// except whatever the Governor recorded in the ledger while it was
// active.
//
// Fields are grouped by which Kind uses them; a Contract only populates
// the group matching its Kind, but they live on one flat struct rather
// than a Go interface/union so evaluation code doesn't need a type
// switch to read them.
type Contract struct {
	// Base fields — present for every kind.
	Kind           Kind
	TaskID         string
	Scope          string    // human-readable scope description for this run (e.g. the task prompt's derived intent); distinct from the guard kind's path-level AllowedPaths below
	ProtectedPaths []string  // absolute path prefixes that are never reachable regardless of any other field (credentials, system paths) — guard and proxy both consult this
	ExpiresAt      time.Time // zero value means "no explicit expiry set"; the contract is still purged at run end regardless

	// kind=guard
	AllowedPaths      []string // repo-relative path prefixes the run is scoped to; "." means the whole repo
	AllowedMCPServers []string // shared with kind=proxy below

	// kind=proxy
	// AllowedMCPServers is shared with kind=guard above.
	AllowedToolPatterns []string

	// kind=verify (reserved; nothing constructs a verify contract yet)
	AllowedDeps   []string
	RequiredTests []string
	TestFilesHash string

	// Operational fields needed by evaluation logic but not part of the
	// payload shapes above: the resolved repo root a guard/proxy
	// contract governs, and the mass-mutation threshold.
	RepoRoot              string
	MassMutationThreshold int
}

// DefaultContract derives the ephemeral session contract for a guard or
// proxy run (verify contracts are constructed separately once the v0.3
// surface exists). Honest scope: this CLI has no NL understanding of
// the task prompt, so unless narrowed later with `centrol scope`, the
// default contract covers the whole repo — the harness-level prompt-
// derivation this struct is shaped for is an integration point, not
// something this binary invents.
func DefaultContract(kind Kind, taskID, repoRoot string) Contract {
	return Contract{
		Kind:                  kind,
		TaskID:                taskID,
		RepoRoot:              repoRoot,
		AllowedPaths:          []string{"."},
		MassMutationThreshold: DefaultMassMutationThreshold,
		ProtectedPaths:        defaultProtectedPaths(),
	}
}

func defaultProtectedPaths() []string {
	home, err := os.UserHomeDir()
	paths := []string{"/etc", "/root"}
	if err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".ssh"), filepath.Join(home, ".aws"))
	} else {
		// Fall back to the literal tilde form; still useful as a
		// substring guard even if it can't be resolved on this host.
		paths = append(paths, "~/.ssh", "~/.aws")
	}
	return CanonicalizeBlockPaths(paths)
}

// CanonicalizeBlockPath resolves p to its canonical absolute form,
// exactly once, so later comparisons are canonical-to-canonical rather
// than raw-string-prefix (which macOS's /etc -> /private/etc symlink,
// a relative "../../../etc", or any other block-path symlink would
// slip past). The steps, in order:
//
//  1. Expand a leading "~" to the user's home directory.
//  2. filepath.Abs to resolve "." and ".." segments and make it absolute.
//  3. filepath.EvalSymlinks on the result, so a block path that is
//     itself a symlink (or has a symlinked ancestor directory) resolves
//     to where it actually points.
//
// If EvalSymlinks fails (the path doesn't exist yet, e.g. ~/.aws before
// the agent's first AWS call ever creates it), the Abs'd-but-unresolved
// path is kept rather than discarding the entry — a not-yet-existing
// protected path is still worth blocking preemptively.
//
// This is called ONCE, at config load / contract construction time —
// never per-event. See EvaluateToolCall, which canonicalizes the
// candidate path the cheaper way (expand + Abs only, no EvalSymlinks)
// and compares it against these already-canonical prefixes.
func CanonicalizeBlockPath(p string) string {
	if p == "" {
		return p
	}
	expanded := expandHome(p)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return expanded
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// CanonicalizeBlockPaths applies CanonicalizeBlockPath to every entry.
func CanonicalizeBlockPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = CanonicalizeBlockPath(p)
	}
	return out
}

func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// canonicalizeCandidate resolves a candidate target path (a tool-call
// argument, not a block-list entry) the same way EXCEPT it does not
// call EvalSymlinks — that would mean a fresh filesystem stat on every
// single evaluated event, which the spec explicitly rules out ("do not
// re-resolve symlinks per event"). Expand + Abs is enough to normalize
// "../../../etc"-style traversal and a relative path against repoRoot
// into the same absolute form the (already-canonical) block list uses.
func canonicalizeCandidate(p, repoRoot string) string {
	if p == "" {
		return p
	}
	expanded := expandHome(p)
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded)
	}
	if repoRoot != "" {
		return filepath.Join(repoRoot, expanded)
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return filepath.Clean(expanded)
	}
	return abs
}

// AmendScope implements `centrol scope +<path>`: widens the contract
// mid-run. The caller is responsible for emitting the corresponding
// contract.declare ledger entry; AmendScope only mutates the in-memory
// contract used for subsequent evaluations.
func (c *Contract) AmendScope(path string) {
	for _, p := range c.AllowedPaths {
		if p == path {
			return
		}
	}
	c.AllowedPaths = append(c.AllowedPaths, path)
}

// configInfraPatterns flags writes to files whose blast radius extends
// beyond the code itself (build/deploy/CI configuration).
var configInfraPatterns = []string{
	"Dockerfile", "docker-compose.yml", "docker-compose.yaml",
	"package.json", "package-lock.json",
	"go.mod", "go.sum",
	"requirements.txt", "Pipfile", "Pipfile.lock",
	"Cargo.toml", "Cargo.lock",
}

func isConfigInfraPath(relPath string) bool {
	base := filepath.Base(relPath)
	for _, p := range configInfraPatterns {
		if base == p {
			return true
		}
	}
	if strings.Contains(filepath.ToSlash(relPath), ".github/workflows/") {
		return true
	}
	if strings.Contains(filepath.ToSlash(relPath), "/migrations/") || strings.HasPrefix(filepath.ToSlash(relPath), "migrations/") {
		return true
	}
	return false
}

func inScope(relPath string, allowed []string) bool {
	relPath = filepath.ToSlash(filepath.Clean(relPath))
	for _, a := range allowed {
		a = filepath.ToSlash(filepath.Clean(a))
		if a == "." || a == "" {
			return true
		}
		if relPath == a || strings.HasPrefix(relPath, a+"/") {
			return true
		}
	}
	return false
}

// EvaluateFSPath evaluates a filesystem write/create/delete observed by
// guard, path given repo-relative (already symlink-resolved by caller).
func EvaluateFSPath(relPath string, c Contract) (Tier, string) {
	if strings.Contains(relPath, "..") {
		return Block, "path traversal outside repo root"
	}
	// Audit fix: ProtectedPaths's own doc comment says "guard and proxy
	// both consult this," but this function never did — only
	// EvaluateToolCall (proxy's tool-call evaluator) checked it. Since
	// every path reaching here is already repo-relative, the built-in
	// defaults (/etc, /root, ~/.ssh, ~/.aws) can only ever match here if
	// the repo itself is nested under one of them; the practical case
	// this closes is an operator-configured proxy.additional_block_paths
	// entry that points inside the repo (e.g. a secrets/ directory),
	// which was silently unenforced under `centrol guard`. Same
	// canonical-to-canonical comparison as EvaluateToolCall, and checked
	// before scope for the same reason: a hard block always outranks a
	// flag, regardless of whether the path is otherwise in scope.
	candidate := canonicalizeCandidate(relPath, c.RepoRoot)
	for _, blocked := range c.ProtectedPaths {
		if candidate == blocked || strings.HasPrefix(candidate, blocked+string(filepath.Separator)) {
			return Block, "credential or system path: " + blocked
		}
	}
	if !inScope(relPath, c.AllowedPaths) {
		return Flag, "write outside contract scope"
	}
	if isConfigInfraPath(relPath) {
		return Flag, "config/infra file"
	}
	return Allow, "matches contract, in-repo"
}

// EvaluateMassMutation flags a single tool call or batch that touches
// more files than the contract's threshold, independent of whether each
// individual file was in scope. The reason string names the exact
// threshold and count in use so a flag prompt or ledger entry is
// self-explanatory without cross-referencing config:
// "mass-mutation threshold (20) exceeded: 23 files touched in one call".
func EvaluateMassMutation(fileCount int, c Contract) (Tier, string) {
	threshold := c.MassMutationThreshold
	if threshold <= 0 {
		threshold = DefaultMassMutationThreshold
	}
	if fileCount > threshold {
		return Flag, fmt.Sprintf("mass-mutation threshold (%d) exceeded: %d files touched in one call", threshold, fileCount)
	}
	return Allow, ""
}

// ToolCallArgs is the minimal shape policy needs from an MCP tools/call
// request to evaluate it; proxy is responsible for extracting these from
// the JSON-RPC params in whatever shape the target server uses.
type ToolCallArgs struct {
	ToolName     string
	PathArgs     []string // any argument values that look like filesystem paths
	ShellChain   bool     // true if args contain shell metacharacters chaining commands
	NewMCPServer bool     // true if this call would register a new MCP server mid-session
}

// EvaluateToolCall implements the proxy's three policy tiers for one
// intercepted MCP tools/call.
func EvaluateToolCall(args ToolCallArgs, c Contract) (Tier, string) {
	if args.ShellChain {
		return Block, "shell chain in tool args"
	}
	for _, p := range args.PathArgs {
		if strings.Contains(p, "..") {
			return Block, "path traversal (..) in tool args"
		}
		// Canonical-to-canonical comparison: c.ProtectedPaths was
		// canonicalized once, at contract construction (defaultProtectedPaths)
		// or config load (additional_block_paths) — never here. The
		// candidate is canonicalized the cheap way (expand + Abs, no
		// per-event EvalSymlinks) so a relative path or a macOS
		// /etc-vs-/private/etc mismatch still matches, without a fresh
		// filesystem stat on every evaluated call.
		candidate := canonicalizeCandidate(p, c.RepoRoot)
		for _, blocked := range c.ProtectedPaths {
			if candidate == blocked || strings.HasPrefix(candidate, blocked+string(filepath.Separator)) {
				return Block, "credential or system path: " + blocked
			}
		}
	}
	if args.NewMCPServer {
		return Flag, "new MCP server registration mid-session"
	}
	for _, p := range args.PathArgs {
		rel := p
		if filepath.IsAbs(p) && c.RepoRoot != "" {
			if r, err := filepath.Rel(c.RepoRoot, p); err == nil {
				rel = r
			}
		}
		if tier, reason := EvaluateFSPath(rel, c); tier != Allow {
			return tier, reason
		}
	}
	return Allow, "matches contract, source file, in-repo"
}
