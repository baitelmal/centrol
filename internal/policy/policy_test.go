package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvaluateFSPathAllowInScope(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	tier, _ := EvaluateFSPath("src/main.go", c)
	if tier != Allow {
		t.Fatalf("expected Allow, got %v", tier)
	}
}

func TestEvaluateFSPathBlockTraversal(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	tier, _ := EvaluateFSPath("../../etc/passwd", c)
	if tier != Block {
		t.Fatalf("expected Block for traversal, got %v", tier)
	}
}

func TestEvaluateFSPathFlagsConfigInfra(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	tier, _ := EvaluateFSPath("Dockerfile", c)
	if tier != Flag {
		t.Fatalf("expected Flag for Dockerfile, got %v", tier)
	}
	tier, _ = EvaluateFSPath(".github/workflows/ci.yml", c)
	if tier != Flag {
		t.Fatalf("expected Flag for workflow file, got %v", tier)
	}
}

func TestEvaluateFSPathFlagsOutOfScope(t *testing.T) {
	c := Contract{Kind: KindGuard, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	tier, _ := EvaluateFSPath("docs/readme.md", c)
	if tier != Flag {
		t.Fatalf("expected Flag for out-of-scope path, got %v", tier)
	}
	tier, _ = EvaluateFSPath("src/inner/file.go", c)
	if tier != Allow {
		t.Fatalf("expected Allow for in-scope nested path, got %v", tier)
	}
}

func TestAmendScopeWidensContract(t *testing.T) {
	c := Contract{Kind: KindGuard, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	c.AmendScope("docs")
	tier, _ := EvaluateFSPath("docs/readme.md", c)
	if tier != Allow {
		t.Fatalf("expected Allow after scope amendment, got %v", tier)
	}
}

func TestEvaluateMassMutation(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	if tier, _ := EvaluateMassMutation(5, c); tier != Allow {
		t.Fatalf("expected Allow for 5 files, got %v", tier)
	}
	if tier, _ := EvaluateMassMutation(25, c); tier != Flag {
		t.Fatalf("expected Flag for 25 files, got %v", tier)
	}
}

func TestEvaluateToolCallHardBlocksCredentials(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	tier, _ := EvaluateToolCall(ToolCallArgs{ToolName: "read_file", PathArgs: c.ProtectedPaths[:1]}, c)
	if tier != Block {
		t.Fatalf("expected Block for credential path, got %v", tier)
	}
}

func TestEvaluateToolCallHardBlocksShellChain(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	tier, _ := EvaluateToolCall(ToolCallArgs{ToolName: "run_shell", ShellChain: true}, c)
	if tier != Block {
		t.Fatalf("expected Block for shell chain, got %v", tier)
	}
}

func TestEvaluateToolCallFlagsNewMCPServer(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	tier, _ := EvaluateToolCall(ToolCallArgs{ToolName: "register_server", NewMCPServer: true}, c)
	if tier != Flag {
		t.Fatalf("expected Flag for new MCP server, got %v", tier)
	}
}

func TestEvaluateToolCallAllowsInRepoPath(t *testing.T) {
	c := DefaultContract(KindGuard, "run-1", "/repo")
	tier, _ := EvaluateToolCall(ToolCallArgs{ToolName: "write_file", PathArgs: []string{"/repo/src/main.go"}}, c)
	if tier != Allow {
		t.Fatalf("expected Allow for in-repo path, got %v", tier)
	}
}

// TestVerifyContractShape confirms a synthetic kind=verify contract
// (the v0.3 validator surface, reserved but not yet wired to any
// command) can be constructed and carries its own field group without
// disturbing the base fields shared with guard/proxy contracts.
func TestVerifyContractShape(t *testing.T) {
	c := Contract{
		Kind:          KindVerify,
		TaskID:        "task-42",
		Scope:         "verify PR #17 against main",
		AllowedDeps:   []string{"github.com/scirem/centrol"},
		RequiredTests: []string{"./internal/ledger/...", "./internal/governor/..."},
		TestFilesHash: "deadbeef",
	}
	if c.Kind != KindVerify {
		t.Fatalf("expected Kind=verify")
	}
	if len(c.RequiredTests) != 2 {
		t.Fatalf("expected 2 required tests, got %v", c.RequiredTests)
	}
	if c.TaskID != "task-42" || c.Scope == "" {
		t.Fatalf("expected base fields (TaskID, Scope) to be populated alongside kind=verify fields")
	}
}

// TestCanonicalBlockPathsHandleRelativeTraversal proves ship criterion
// 10's first half: a block-path entry expressed with ".." segments (or
// relative to cwd) canonicalizes to the same absolute form a plain
// absolute entry would, so EvaluateToolCall blocks a matching candidate
// either way.
func TestCanonicalBlockPathsHandleRelativeTraversal(t *testing.T) {
	dir := t.TempDir()
	sensitive := filepath.Join(dir, "sensitive")
	if err := os.MkdirAll(sensitive, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	// Configured with a relative, traversal-laden path that reaches the
	// same "sensitive" directory from three levels down.
	relative := filepath.Join(nested, "..", "..", "sensitive")
	canon := CanonicalizeBlockPath(relative)
	if canon != sensitive {
		t.Fatalf("CanonicalizeBlockPath(%q) = %q, want %q", relative, canon, sensitive)
	}

	c := DefaultContract(KindProxy, "run-1", dir)
	c.ProtectedPaths = []string{canon}
	tier, reason := EvaluateToolCall(ToolCallArgs{
		ToolName: "write_file",
		PathArgs: []string{filepath.Join(sensitive, "secret.txt")},
	}, c)
	if tier != Block {
		t.Fatalf("expected Block for a path under the canonicalized block entry, got %v (%s)", tier, reason)
	}
}

// TestCanonicalBlockPathsResolveSymlinks proves ship criterion 10's
// second half: a configured additional_block_paths entry that is
// itself a symlink canonicalizes (once, via CanonicalizeBlockPath) to
// where it actually points, so a write to the real underlying path is
// blocked even though the configured string named the symlink, not the
// target.
func TestCanonicalBlockPathsResolveSymlinks(t *testing.T) {
	dir := t.TempDir()
	realTarget := filepath.Join(dir, "real-secrets")
	if err := os.MkdirAll(realTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "secrets-link")
	if err := os.Symlink(realTarget, link); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	canon := CanonicalizeBlockPath(link)
	if canon != realTarget {
		// filepath.EvalSymlinks itself resolves any symlinked ancestor
		// of dir too (e.g. on macOS /tmp -> /private/tmp), so compare
		// against the EvalSymlinks'd form of realTarget rather than
		// assuming t.TempDir() is already fully resolved.
		wantReal, _ := filepath.EvalSymlinks(realTarget)
		if canon != wantReal {
			t.Fatalf("CanonicalizeBlockPath(%q) = %q, want the resolved real target %q", link, canon, wantReal)
		}
	}

	c := DefaultContract(KindProxy, "run-1", dir)
	c.ProtectedPaths = []string{canon}
	tier, reason := EvaluateToolCall(ToolCallArgs{
		ToolName: "write_file",
		PathArgs: []string{filepath.Join(realTarget, "secret.txt")},
	}, c)
	if tier != Block {
		t.Fatalf("expected Block writing through the resolved real target, got %v (%s)", tier, reason)
	}
}
