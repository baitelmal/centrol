package policy

import (
	"path/filepath"
	"strings"
)

// launcherCommands are wrapper binaries whose own name isn't the
// server's identity — the identity is in their arguments instead
// ("npx @modelcontextprotocol/server-filesystem /path" identifies as
// the package, not as "npx").
var launcherCommands = map[string]bool{
	"npx": true, "npm": true, "pnpm": true, "yarn": true,
	"node": true, "python": true, "python3": true, "uvx": true, "uv": true,
}

var launcherSkipTokens = map[string]bool{
	"exec": true, "dlx": true, "run": true, "-y": true, "--yes": true,
}

// ServerIdentity extracts the package/binary name a target command
// identifies as, for allowlist matching by name rather than full
// command string (the default matching rule). This is a heuristic, not
// an exact parse: MCP servers are launched enough different ways (bare
// binary, npx, python -m, ...) that there is no single canonical form.
// It errs toward finding *a* reasonable identity token over refusing to
// match — under-matching a known server just costs one extra prompt,
// which is the safer failure direction for an accountability layer.
func ServerIdentity(command string, args []string) string {
	base := filepath.Base(command)
	if !launcherCommands[base] {
		return base
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") || launcherSkipTokens[a] {
			continue
		}
		return a
	}
	return base // launcher with no identifiable argument; fall back to its own name
}

// MatchesAllowlist reports whether a target is present in the
// allowlist. Package/binary-name matching (the default, strict=false)
// compares ServerIdentity's output — so
// "@modelcontextprotocol/server-filesystem" matches regardless of which
// directory arguments were passed to it. strict=true compares the full
// command string instead.
func MatchesAllowlist(identity, fullCommand string, allowed []string, strict bool) bool {
	needle := identity
	if strict {
		needle = fullCommand
	}
	for _, a := range allowed {
		if a == needle {
			return true
		}
	}
	return false
}
