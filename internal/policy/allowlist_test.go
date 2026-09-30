package policy

import "testing"

func TestServerIdentityFromNpxLauncher(t *testing.T) {
	id := ServerIdentity("npx", []string{"-y", "@modelcontextprotocol/server-filesystem", "/some/dir"})
	if id != "@modelcontextprotocol/server-filesystem" {
		t.Fatalf("expected the package name, got %q", id)
	}
}

func TestServerIdentityIgnoresDirectoryArgs(t *testing.T) {
	id1 := ServerIdentity("npx", []string{"@mcp/server-filesystem", "/path/a"})
	id2 := ServerIdentity("npx", []string{"@mcp/server-filesystem", "/path/b", "/path/c"})
	if id1 != id2 {
		t.Fatalf("expected identity to match regardless of directory args, got %q vs %q", id1, id2)
	}
}

func TestServerIdentityBareBinary(t *testing.T) {
	id := ServerIdentity("/usr/local/bin/my-mcp-server", []string{"--port", "1234"})
	if id != "my-mcp-server" {
		t.Fatalf("expected the binary's base name, got %q", id)
	}
}

func TestServerIdentityLauncherWithNoArgsFallsBackToLauncherName(t *testing.T) {
	id := ServerIdentity("npx", nil)
	if id != "npx" {
		t.Fatalf("expected fallback to the launcher's own name, got %q", id)
	}
}

func TestMatchesAllowlistPackageNameMatching(t *testing.T) {
	allowed := []string{"@modelcontextprotocol/server-filesystem"}
	if !MatchesAllowlist("@modelcontextprotocol/server-filesystem", "npx @modelcontextprotocol/server-filesystem /a", allowed, false) {
		t.Fatalf("expected a package-name match")
	}
	if !MatchesAllowlist("@modelcontextprotocol/server-filesystem", "npx @modelcontextprotocol/server-filesystem /b", allowed, false) {
		t.Fatalf("expected the match to hold regardless of directory args")
	}
	if MatchesAllowlist("@other/server", "npx @other/server /a", allowed, false) {
		t.Fatalf("expected no match for a different package")
	}
}

func TestMatchesAllowlistStrictMatching(t *testing.T) {
	fullCmd := "npx @modelcontextprotocol/server-filesystem /a"
	allowed := []string{fullCmd}
	if !MatchesAllowlist("@modelcontextprotocol/server-filesystem", fullCmd, allowed, true) {
		t.Fatalf("expected an exact full-command match under strict matching")
	}
	if MatchesAllowlist("@modelcontextprotocol/server-filesystem", "npx @modelcontextprotocol/server-filesystem /b", allowed, true) {
		t.Fatalf("expected strict matching to require the exact same command string, including args")
	}
}
