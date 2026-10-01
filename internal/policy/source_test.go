package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTOMLRoundTrip(t *testing.T) {
	src := "[proxy]\nmass_mutation_threshold = 50\nallowed_servers = [\"a\", \"b\"]\nstrict_matching = true\n"
	doc, err := parseTOMLSubset([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc["proxy"]["mass_mutation_threshold"] != 50 {
		t.Fatalf("expected 50, got %v", doc["proxy"]["mass_mutation_threshold"])
	}
	servers, ok := doc["proxy"]["allowed_servers"].([]string)
	if !ok || len(servers) != 2 || servers[0] != "a" || servers[1] != "b" {
		t.Fatalf("expected [a b], got %v", doc["proxy"]["allowed_servers"])
	}
	if doc["proxy"]["strict_matching"] != true {
		t.Fatalf("expected true, got %v", doc["proxy"]["strict_matching"])
	}

	out := writeTOMLSubset(doc)
	reparsed, err := parseTOMLSubset(out)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if reparsed["proxy"]["mass_mutation_threshold"] != 50 {
		t.Fatalf("round trip lost the threshold value")
	}
}

func TestLocalFileSourceResolveMissingFileIsNotFoundNotError(t *testing.T) {
	dir := t.TempDir()
	src := NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "config.toml"))
	_, found, err := src.Resolve("proxy.mass_mutation_threshold")
	if err != nil {
		t.Fatalf("expected no error for a missing config file, got %v", err)
	}
	if found {
		t.Fatalf("expected found=false for a missing config file")
	}
}

func TestLocalFileSourceWriteThenResolve(t *testing.T) {
	dir := t.TempDir()
	src := NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "config.toml"))
	if err := src.Write("proxy.mass_mutation_threshold", 50); err != nil {
		t.Fatalf("Write: %v", err)
	}
	v, found, err := src.Resolve("proxy.mass_mutation_threshold")
	if err != nil || !found {
		t.Fatalf("expected to resolve the written value, found=%v err=%v", found, err)
	}
	if v != 50 {
		t.Fatalf("expected 50, got %v", v)
	}
}

func TestLocalFileSourceWritePreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	src := NewLocalFileSource(SourceRepoConfig, path)
	if err := src.Write("proxy.mass_mutation_threshold", 50); err != nil {
		t.Fatal(err)
	}
	if err := src.Write("proxy.allowed_servers", []string{"@scope/pkg"}); err != nil {
		t.Fatal(err)
	}
	thresh, found, _ := src.Resolve("proxy.mass_mutation_threshold")
	if !found || thresh != 50 {
		t.Fatalf("expected the first write to survive the second, got %v found=%v", thresh, found)
	}
	servers, found, _ := src.Resolve("proxy.allowed_servers")
	if !found {
		t.Fatalf("expected allowed_servers to be found")
	}
	if s, ok := servers.([]string); !ok || len(s) != 1 || s[0] != "@scope/pkg" {
		t.Fatalf("expected [@scope/pkg], got %v", servers)
	}
}

// TestLocalFileSourceWriteCleansUpTempFileOnFailedRename is the
// audit's 4d fix: Write used a fixed ".tmp" name with no cleanup on a
// failed rename. This forces a deterministic rename failure (the
// destination is itself a directory — renaming a file onto a
// directory always fails) and confirms no orphan "*.tmp" file is left
// behind in the config directory afterward.
func TestLocalFileSourceWriteCleansUpTempFileOnFailedRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}

	src := NewLocalFileSource(SourceRepoConfig, path)
	if err := src.Write("proxy.target_url", "https://example.com"); err == nil {
		t.Fatal("expected Write to fail when its rename target is a directory")
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no orphan temp file left behind after a failed rename, found %v", matches)
	}
}

// TestLocalFileSourceWriteLeavesNoTempFileOnSuccess confirms the
// happy path doesn't leave its own temp file behind either — only the
// final config.toml should exist.
func TestLocalFileSourceWriteLeavesNoTempFileOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	src := NewLocalFileSource(SourceRepoConfig, path)
	if err := src.Write("proxy.target_url", "https://example.com"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only config.toml in %s, found %v", dir, names)
	}
}

func TestEnterpriseSourceNeverFindsAnythingAndIsNotWritable(t *testing.T) {
	e := NewEnterpriseSource()
	_, found, err := e.Resolve("proxy.mass_mutation_threshold")
	if err != nil || found {
		t.Fatalf("expected the enterprise stub to never find anything, found=%v err=%v", found, err)
	}
	if e.Writable() {
		t.Fatalf("expected the enterprise stub to never be writable")
	}
}

// TestResolutionChainPriorityOrder is the core acceptance test for
// section 3: session contract beats enterprise beats repo config beats
// user config beats the built-in default, and every level's absence
// falls through correctly to the next.
func TestResolutionChainPriorityOrder(t *testing.T) {
	dir := t.TempDir()
	session := NewSessionContractSource()
	enterprise := NewEnterpriseSource()
	repo := NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "repo.toml"))
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "user.toml"))
	def := NewDefaultSource(map[string]interface{}{"proxy.mass_mutation_threshold": 20})
	r := NewResolver(session, enterprise, repo, user, def)

	// Nothing set anywhere but the default.
	v, source, found, err := r.Resolve("proxy.mass_mutation_threshold")
	if err != nil || !found || v != 20 || source != SourceDefault {
		t.Fatalf("expected default 20, got v=%v source=%s found=%v err=%v", v, source, found, err)
	}

	// User config sets 30 — should now win over the default.
	if err := user.Write("proxy.mass_mutation_threshold", 30); err != nil {
		t.Fatal(err)
	}
	v, source, found, err = r.Resolve("proxy.mass_mutation_threshold")
	if err != nil || !found || v != 30 || source != SourceUserConfig {
		t.Fatalf("expected user_config 30, got v=%v source=%s", v, source)
	}

	// Repo config sets 50 — should now win over user config.
	if err := repo.Write("proxy.mass_mutation_threshold", 50); err != nil {
		t.Fatal(err)
	}
	v, source, found, err = r.Resolve("proxy.mass_mutation_threshold")
	if err != nil || !found || v != 50 || source != SourceRepoConfig {
		t.Fatalf("expected repo_config 50, got v=%v source=%s", v, source)
	}

	// Session contract override wins over everything.
	session.Set("proxy.mass_mutation_threshold", 99)
	v, source, found, err = r.Resolve("proxy.mass_mutation_threshold")
	if err != nil || !found || v != 99 || source != SourceSessionContract {
		t.Fatalf("expected session_contract 99, got v=%v source=%s", v, source)
	}
}

func TestWriteConfigTargetsHighestPriorityWritableSource(t *testing.T) {
	dir := t.TempDir()
	session := NewSessionContractSource()
	enterprise := NewEnterpriseSource()
	repo := NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "repo.toml"))
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "user.toml"))
	def := NewDefaultSource(nil)
	r := NewResolver(session, enterprise, repo, user, def)

	source, err := r.WriteConfig("proxy.allowed_servers", []string{"@mcp/server-filesystem"})
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if source != SourceRepoConfig {
		t.Fatalf("expected repo_config to be the write target (highest-priority writable persisted source), got %s", source)
	}
	v, foundSource, found, err := r.Resolve("proxy.allowed_servers")
	if err != nil || !found || foundSource != SourceRepoConfig {
		t.Fatalf("expected the write to be resolvable back from repo_config, got found=%v source=%s err=%v", found, foundSource, err)
	}
	_ = v
}
