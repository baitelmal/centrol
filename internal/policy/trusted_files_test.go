package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trustedHome points HOME at a fresh directory with symlinks resolved,
// so canonical comparisons are not thrown by a temp dir that is itself a
// symlink. It deliberately is not under /root, which the defaults
// already protect and would hide a missing entry.
func trustedHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

var trustedNames = []string{"enterprise-policy.json", "allowlist.json", "config.toml"}

func TestTrustedFilesAreExactlyTheThreeInHome(t *testing.T) {
	home := trustedHome(t)
	var want []string
	for _, n := range trustedNames {
		want = append(want, filepath.Join(home, ".centrol", n))
	}
	got := TrustedFiles("")
	if len(got) != len(want) {
		t.Fatalf("TrustedFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("TrustedFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if p := UserConfigPath(); p != want[2] {
		t.Errorf("UserConfigPath = %q, want %q", p, want[2])
	}
}

func TestTrustedFilesEmptyWithoutAHomeOrRepo(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if got := TrustedFiles(""); len(got) != 0 {
		t.Fatalf("TrustedFiles = %v, want none when HOME is unresolvable", got)
	}
}

// An MCP tool call that names a policy file is refused by the protected
// set, whether the file exists yet or not, and however the path is
// spelled. The repo root is the home directory so that ~/.centrol is
// inside the repo: otherwise the existing outside-the-repo rule would
// refuse the call for its own reason and hide a missing entry.
func TestToolCallsToThePolicyFilesAreBlocked(t *testing.T) {
	for _, exists := range []bool{false, true} {
		home := trustedHome(t)
		if exists {
			if err := os.MkdirAll(filepath.Join(home, ".centrol"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, n := range trustedNames {
				if err := os.WriteFile(filepath.Join(home, ".centrol", n), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		c := DefaultContract(KindProxy, "run-1", home)
		for _, n := range trustedNames {
			for _, spelling := range []string{
				filepath.Join(home, ".centrol", n),
				"~/.centrol/" + n,
				filepath.Join(".centrol", n),
			} {
				tier, reason := EvaluateToolCall(ToolCallArgs{ToolName: "write_file", PathArgs: []string{spelling}}, c)
				if tier != Block || !strings.Contains(reason, "credential or system path") {
					t.Errorf("exists=%v: write to %q = (%v, %q), want a protected-path Block", exists, spelling, tier, reason)
				}
			}
		}
	}
}

// Only the three files are protected. Another file in ~/.centrol, or a
// near-miss name beside a policy file, is not blocked by this set (the
// repo root is the home directory, so that no other rule applies).
func TestOtherFilesUnderCentrolHomeAreNotBlocked(t *testing.T) {
	home := trustedHome(t)
	c := DefaultContract(KindProxy, "run-1", home)
	for _, name := range []string{"notes.txt", "allowlist.json.bak", "config.toml.old", "enterprise-policy.json.tmp"} {
		path := filepath.Join(home, ".centrol", name)
		if tier, reason := EvaluateToolCall(ToolCallArgs{ToolName: "write_file", PathArgs: []string{path}}, c); tier == Block {
			t.Errorf("write to %q was blocked (%q); only the three policy files are protected", path, reason)
		}
	}
}

// The guard judges a direct write to a policy file by the same
// protected set, so it is recorded as a block-tier violation (recorded,
// not prevented — see Watcher.WatchTrustedFiles).
func TestGuardEventsForThePolicyFilesAreBlockTier(t *testing.T) {
	home := trustedHome(t)
	c := DefaultContract(KindGuard, "run-1", home)
	for _, n := range trustedNames {
		path := filepath.Join(home, ".centrol", n)
		if tier, reason := EvaluateFSPath(path, c); tier != Block || !strings.Contains(reason, "credential or system path") {
			t.Errorf("EvaluateFSPath(%q) = (%v, %q), want Block", path, tier, reason)
		}
	}
}

// The repo's own config.toml outranks the user config, so an MCP call
// that names it is refused by the protected set. The repo is separate
// from the home directory, and the file is inside the repo scope, so no
// other rule would refuse the call.
func TestToolCallsToTheRepoConfigAreBlocked(t *testing.T) {
	trustedHome(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, exists := range []bool{false, true} {
		if exists {
			if err := os.MkdirAll(filepath.Join(repo, ".centrol"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".centrol", "config.toml"), []byte("[general]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c := DefaultContract(KindProxy, "run-1", repo)
		for _, spelling := range []string{filepath.Join(repo, ".centrol", "config.toml"), ".centrol/config.toml"} {
			tier, reason := EvaluateToolCall(ToolCallArgs{ToolName: "write_file", PathArgs: []string{spelling}}, c)
			if tier != Block || !strings.Contains(reason, "credential or system path") {
				t.Errorf("exists=%v: write to %q = (%v, %q), want a protected-path Block", exists, spelling, tier, reason)
			}
		}
	}
}

// Only the config file is carved out of the repo's .centrol directory.
// The ledger is guarded by its hash chain and tamper detection, not by
// this set, so a tool call naming a segment is not refused by it.
func TestRepoCentrolDirectoryIsNotProtectedBeyondTheConfig(t *testing.T) {
	trustedHome(t)
	repo := t.TempDir()
	c := DefaultContract(KindProxy, "run-1", repo)
	for _, name := range []string{"lighthouse.jsonl", "lighthouse.001.jsonl", "scope-requests.jsonl", "config.toml.bak"} {
		tier, reason := EvaluateToolCall(ToolCallArgs{ToolName: "write_file", PathArgs: []string{filepath.Join(".centrol", name)}}, c)
		if tier == Block {
			t.Errorf(".centrol/%s was blocked (%q); only config.toml is added to the protected set", name, reason)
		}
	}
}

func TestTrustedFilesIncludeTheRepoConfigAndKeepTheHomeFiles(t *testing.T) {
	home := trustedHome(t)
	repo := filepath.Join(home, "proj")
	got := TrustedFiles(repo)
	want := []string{
		filepath.Join(home, ".centrol", "enterprise-policy.json"),
		filepath.Join(home, ".centrol", "allowlist.json"),
		filepath.Join(home, ".centrol", "config.toml"),
		filepath.Join(repo, ".centrol", "config.toml"),
	}
	if len(got) != len(want) {
		t.Fatalf("TrustedFiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("TrustedFiles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if RepoConfigPath(repo) != want[3] {
		t.Errorf("RepoConfigPath = %q, want %q", RepoConfigPath(repo), want[3])
	}
}

func TestGuardEventsForTheRepoConfigAreBlockTier(t *testing.T) {
	trustedHome(t)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	c := DefaultContract(KindGuard, "run-1", repo)
	path := filepath.Join(repo, ".centrol", "config.toml")
	if tier, reason := EvaluateFSPath(path, c); tier != Block || !strings.Contains(reason, "credential or system path") {
		t.Fatalf("EvaluateFSPath(%q) = (%v, %q), want a protected-path Block", path, tier, reason)
	}
}
