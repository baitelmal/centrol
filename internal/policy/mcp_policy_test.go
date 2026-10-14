package policy

import (
	"strings"
	"testing"
)

const testRepo = "/repo"

// policyContract is a proxy contract with a structured policy applied
// for identity, built the way cmd_proxy builds it.
func policyContract(source string, identity string, servers ...PolicyServer) Contract {
	c := DefaultContract(KindProxy, "run-1", testRepo)
	c.ApplyPolicy(&StructuredPolicy{Source: source, Servers: servers}, identity)
	return c
}

func call(tool string, paths ...string) ToolCallArgs {
	return ToolCallArgs{ToolName: tool, PathArgs: paths}
}

func TestStructuredPolicyEnforcement(t *testing.T) {
	scoped := PolicyServer{
		Server: "fs",
		Paths:  []string{"src/**", "docs/*.md"},
		Tools:  []string{"read_file", "list_directory"},
	}
	cases := []struct {
		name     string
		contract Contract
		args     ToolCallArgs
		want     Tier
		reason   string // substring of the block reason
	}{
		{"unlisted server is refused", policyContract(SourceEnterprisePolicy, "other", scoped), call("read_file", "src/a.go"), Block, "server not permitted by policy"},
		{"empty policy permits no server", policyContract(SourceEnterprisePolicy, "fs"), call("read_file"), Block, "server not permitted by policy"},
		{"listed tool and path pass", policyContract(SourceEnterprisePolicy, "fs", scoped), call("read_file", "src/pkg/a.go"), Allow, ""},
		{"unlisted tool is refused", policyContract(SourceEnterprisePolicy, "fs", scoped), call("write_file", "src/a.go"), Block, "tool write_file not permitted for server fs"},
		{"path outside the server's paths is refused", policyContract(SourceEnterprisePolicy, "fs", scoped), call("read_file", "cmd/main.go"), Block, "path cmd/main.go not permitted for server fs"},
		{"a single-segment glob does not cross directories", policyContract(SourceEnterprisePolicy, "fs", scoped), call("read_file", "docs/deep/x.md"), Block, "docs/deep/x.md"},
		{"a single-segment glob matches within its directory", policyContract(SourceEnterprisePolicy, "fs", scoped), call("read_file", "docs/guide.md"), Allow, ""},
		{"traversal is judged by where it lands", policyContract(SourceEnterprisePolicy, "fs", scoped), call("read_file", "src/../cmd/main.go"), Block, "not permitted for server fs"},
		{"an absolute path inside the repo is matched relative to it", policyContract(SourceEnterprisePolicy, "fs", scoped), call("read_file", "/repo/src/a.go"), Allow, ""},
		{"a relative pattern never matches outside the repo", policyContract(SourceEnterprisePolicy, "fs", scoped), call("read_file", "/elsewhere/src/a.go"), Block, "not permitted for server fs"},
		{"a call with no path arguments has nothing to scope", policyContract(SourceEnterprisePolicy, "fs", scoped), call("list_directory"), Allow, ""},

		{"absent paths and tools mean unrestricted", policyContract(SourceEnterprisePolicy, "git", PolicyServer{Server: "git"}), call("anything", "any/where.txt"), Allow, ""},
		{"empty paths allow no paths", policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs", Paths: []string{}}), call("read_file", "src/a.go"), Block, "path src/a.go"},
		{"empty paths still allow a call that names none", policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs", Paths: []string{}}), call("read_file"), Allow, ""},
		{"empty tools allow no tools", policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs", Tools: []string{}}), call("read_file"), Block, "tool read_file not permitted"},
		{"an absolute pattern matches an absolute path", policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs", Paths: []string{"/repo/data/**"}}), call("read_file", "/repo/data/x/y.csv"), Allow, ""},
		{"an absolute pattern does not match elsewhere", policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs", Paths: []string{"/repo/data/**"}}), call("read_file", "/repo/src/y.csv"), Block, "/repo/src/y.csv"},
		{"a policy cannot grant a path the repo scope refuses", policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs", Paths: []string{"/data/**"}}), call("read_file", "/data/x.csv"), Block, "outside repo root"},
		{"a malformed pattern matches nothing", policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs", Paths: []string{"src/[**"}}), call("read_file", "src/a.go"), Block, "src/a.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tier, reason := EvaluateToolCall(tc.args, tc.contract)
			if tier != tc.want || !strings.Contains(reason, tc.reason) {
				t.Fatalf("EvaluateToolCall = (%v, %q), want (%v, containing %q)", tier, reason, tc.want, tc.reason)
			}
		})
	}
}

// A local allowlist.json is enforced exactly like the enterprise
// policy: the schema and the check are shared, only the source differs.
func TestLocalAllowlistEnforcesIdenticallyToEnterprise(t *testing.T) {
	server := PolicyServer{Server: "fs", Paths: []string{"src/**"}, Tools: []string{"read_file"}}
	calls := []ToolCallArgs{
		call("read_file", "src/a.go"),
		call("write_file", "src/a.go"),
		call("read_file", "cmd/a.go"),
	}
	for _, args := range calls {
		ent := policyContract(SourceEnterprisePolicy, "fs", server)
		loc := policyContract(SourceLocalAllowlist, "fs", server)
		entTier, entReason := EvaluateToolCall(args, ent)
		locTier, locReason := EvaluateToolCall(args, loc)
		if entTier != locTier || entReason != locReason {
			t.Errorf("%+v: enterprise (%v, %q) differs from local (%v, %q)", args, entTier, entReason, locTier, locReason)
		}
	}
}

// With no structured policy the contract is untouched and the existing
// evaluation stands: nothing is refused for its server, tool or scope.
func TestNoStructuredPolicyLeavesEvaluationUnchanged(t *testing.T) {
	c := DefaultContract(KindProxy, "run-1", testRepo)
	if c.PolicySource != "" || c.AllowedMCPServers != nil || c.AllowedToolPatterns != nil || c.AllowedServerPaths != nil {
		t.Fatalf("a default contract must carry no policy: %+v", c)
	}
	if tier, reason := EvaluateToolCall(call("any_tool", "src/a.go"), c); tier != Allow {
		t.Fatalf("EvaluateToolCall = (%v, %q), want Allow", tier, reason)
	}
	if tier, _ := EvaluateToolCall(call("any_tool", "/etc/passwd"), c); tier != Block {
		t.Fatalf("protected paths must still block with no policy, got %v", tier)
	}
}

// The policy narrows; it never overrides the checks beneath it.
func TestStructuredPolicyDoesNotLoosenExistingChecks(t *testing.T) {
	c := policyContract(SourceEnterprisePolicy, "fs", PolicyServer{Server: "fs"})
	if tier, _ := EvaluateToolCall(call("read_file", "/etc/passwd"), c); tier != Block {
		t.Errorf("a protected path must stay blocked for an unrestricted server, got %v", tier)
	}
	chained := ToolCallArgs{ToolName: "run", ShellChain: true}
	if tier, _ := EvaluateToolCall(chained, c); tier != Block {
		t.Errorf("a shell chain must stay blocked for an unrestricted server, got %v", tier)
	}
}

func TestMatchPathPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"src/**", "src", true},
		{"src/**", "src/a/b/c.go", true},
		{"src/**", "srcs/a.go", false},
		{"src", "src", true},
		{"src", "src/a.go", false},
		{"**", "anything/at/all", true},
		{"**/*.go", "a.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "a/b/c.txt", false},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c", false},
		{"docs/*.md", "docs/x.md", true},
		{"docs/*.md", "docs/a/x.md", false},
		{"[", "[", false},
	}
	for _, tc := range cases {
		if got := matchPathPattern(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchPathPattern(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestHTTPServerIdentityIsTheHost(t *testing.T) {
	cases := map[string]string{
		"https://mcp.example.com/sse":   "mcp.example.com",
		"http://127.0.0.1:8080/mcp":     "127.0.0.1:8080",
		"https://mcp.example.com:443/x": "mcp.example.com:443",
		"not a url":                     "",
		"":                              "",
		"://broken":                     "",
	}
	for raw, want := range cases {
		if got := HTTPServerIdentity(raw); got != want {
			t.Errorf("HTTPServerIdentity(%q) = %q, want %q", raw, got, want)
		}
	}
}
