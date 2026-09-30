package policy

import (
	"path/filepath"
	"testing"
)

func TestResolvePromptTimeoutSecondsDefaultAndFloor(t *testing.T) {
	r := NewResolver(NewDefaultSource(nil))
	n, src, err := ResolvePromptTimeoutSeconds(r)
	if err != nil || n != DefaultPromptTimeoutSeconds || src != SourceDefault {
		t.Fatalf("expected default %d, got %d/%s (err=%v)", DefaultPromptTimeoutSeconds, n, src, err)
	}

	dir := t.TempDir()
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "u.toml"))
	user.Write("gate.prompt_timeout_seconds", 5)
	r2 := NewResolver(user, NewDefaultSource(nil))
	_, _, err = ResolvePromptTimeoutSeconds(r2)
	if err == nil {
		t.Fatalf("expected 5 (below the 30s floor) to be rejected")
	}
}

func TestResolveAllowSessionAmendDefaultTrue(t *testing.T) {
	r := NewResolver(NewDefaultSource(nil))
	allow, src, err := ResolveAllowSessionAmend(r)
	if err != nil || !allow || src != SourceDefault {
		t.Fatalf("expected default true, got %v/%s (err=%v)", allow, src, err)
	}
}

func TestResolveAdditionalBlockPaths(t *testing.T) {
	dir := t.TempDir()
	repo := NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "r.toml"))
	repo.Write("proxy.additional_block_paths", []string{"/opt/secrets"})
	r := NewResolver(repo, NewDefaultSource(nil))
	paths, src, err := ResolveAdditionalBlockPaths(r)
	if err != nil || len(paths) != 1 || paths[0] != "/opt/secrets" || src != SourceRepoConfig {
		t.Fatalf("expected [/opt/secrets] from repo_config, got %v/%s (err=%v)", paths, src, err)
	}
}

func TestResolveMCPCallTimeoutSecondsFloor(t *testing.T) {
	dir := t.TempDir()
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "u.toml"))
	user.Write("proxy.mcp_call_timeout_seconds", 2)
	r := NewResolver(user, NewDefaultSource(nil))
	_, _, err := ResolveMCPCallTimeoutSeconds(r)
	if err == nil {
		t.Fatalf("expected 2 (below the 5s floor) to be rejected")
	}
}

func TestResolveLogLevelValidatesEnum(t *testing.T) {
	dir := t.TempDir()
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "u.toml"))
	user.Write("general.log_level", "loud")
	r := NewResolver(user, NewDefaultSource(nil))
	_, _, err := ResolveLogLevel(r)
	if err == nil {
		t.Fatalf("expected an invalid log level to be rejected")
	}

	user.Write("general.log_level", "debug")
	level, src, err := ResolveLogLevel(r)
	if err != nil || level != "debug" || src != SourceUserConfig {
		t.Fatalf("expected debug/user_config, got %s/%s (err=%v)", level, src, err)
	}
}

func TestResolveObserveModeDefaultFalse(t *testing.T) {
	r := NewResolver(NewDefaultSource(nil))
	observe, src, err := ResolveObserveMode(r)
	if err != nil || observe || src != SourceDefault {
		t.Fatalf("expected default false, got %v/%s (err=%v)", observe, src, err)
	}
}
