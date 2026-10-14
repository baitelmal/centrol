package policy

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ApplyPolicy narrows the contract to the structured MCP policy p for
// the server this proxy fronts. A server the policy does not list gets
// no tools and paths of its own, and evaluateMCPPolicy refuses it
// before it would look at them.
func (c *Contract) ApplyPolicy(p *StructuredPolicy, identity string) {
	c.PolicySource = p.Source
	c.ServerIdentity = identity
	c.AllowedMCPServers = make([]string, len(p.Servers))
	for i, s := range p.Servers {
		c.AllowedMCPServers[i] = s.Server
	}
	if s, ok := p.Server(identity); ok {
		c.AllowedToolPatterns = s.Tools
		c.AllowedServerPaths = s.Paths
	}
}

// evaluateMCPPolicy is the call-time half of a structured policy; the
// pre-flight in cmd_proxy is the launch-time half. It does nothing when
// no policy is active.
func evaluateMCPPolicy(args ToolCallArgs, c Contract) (Tier, string) {
	if c.PolicySource == "" {
		return Allow, ""
	}
	if !containsString(c.AllowedMCPServers, c.ServerIdentity) {
		return Block, "server not permitted by policy"
	}
	if c.AllowedToolPatterns != nil && !containsString(c.AllowedToolPatterns, args.ToolName) {
		return Block, fmt.Sprintf("tool %s not permitted for server %s", args.ToolName, c.ServerIdentity)
	}
	if c.AllowedServerPaths != nil {
		for _, p := range args.PathArgs {
			if !pathMatchesAny(p, c) {
				return Block, fmt.Sprintf("path %s not permitted for server %s", p, c.ServerIdentity)
			}
		}
	}
	return Allow, ""
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pathMatchesAny reports whether p falls under one of the server's
// path patterns. p is normalized first, so a "src/../../etc" argument
// is judged by where it lands, not by how it is spelled. A relative
// pattern is matched against the path relative to the repo root, and so
// can never match a path outside the repo; an absolute pattern is
// matched against the absolute path.
func pathMatchesAny(p string, c Contract) bool {
	abs := canonicalizeCandidate(p, c.RepoRoot)
	rel, inRepo := "", false
	if c.RepoRoot != "" {
		if r, err := filepath.Rel(c.RepoRoot, abs); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel, inRepo = filepath.ToSlash(r), true
		}
	}
	for _, pattern := range c.AllowedServerPaths {
		if filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "/") {
			if matchPathPattern(pattern, filepath.ToSlash(abs)) {
				return true
			}
		} else if inRepo && matchPathPattern(pattern, rel) {
			return true
		}
	}
	return false
}

// matchPathPattern matches a slash-separated name against a pattern
// whose segments are path.Match globs, plus "**" for any number of
// segments (including none): "src/**" is "src" and everything under it,
// while a bare "src" is only that exact path. A malformed pattern
// matches nothing, so a typo narrows access instead of widening it.
func matchPathPattern(pattern, name string) bool {
	return matchSegments(
		strings.Split(strings.Trim(pattern, "/"), "/"),
		strings.Split(strings.Trim(name, "/"), "/"),
	)
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}
