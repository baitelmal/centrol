package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writePolicyFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// structuredChain builds the standard chain over temp files. An empty
// enterprise or local argument means that file is absent.
func structuredChain(t *testing.T, enterprise, local string) (*Resolver, *LocalFileSource) {
	t.Helper()
	dir := t.TempDir()
	entPath := filepath.Join(dir, "enterprise-policy.json")
	if enterprise != "" {
		entPath = writePolicyFile(t, "enterprise-policy.json", enterprise)
	}
	localPath := filepath.Join(dir, "allowlist.json")
	if local != "" {
		localPath = writePolicyFile(t, "allowlist.json", local)
	}
	repo := NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "repo.toml"))
	return NewResolver(
		NewSessionContractSource(),
		NewEnterpriseSourceAt(entPath),
		NewLocalAllowlistSourceAt(localPath),
		repo,
		NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "user.toml")),
		NewDefaultSource(nil),
	), repo
}

const (
	enterpriseOnlyPolicy = `{"servers":[{"server":"from-enterprise"}]}`
	localOnlyPolicy      = `{"servers":[{"server":"from-local"}]}`
)

func TestEnterprisePolicyBeatsLocalAllowlist(t *testing.T) {
	r, _ := structuredChain(t, enterpriseOnlyPolicy, localOnlyPolicy)
	servers, source, err := ResolveAllowedServers(r)
	if err != nil || source != SourceEnterprisePolicy || !reflect.DeepEqual(servers, []string{"from-enterprise"}) {
		t.Fatalf("ResolveAllowedServers = (%v, %q, %v), want [from-enterprise] from enterprise", servers, source, err)
	}
	p, err := r.ActivePolicy()
	if err != nil || p == nil || p.Source != SourceEnterprisePolicy {
		t.Fatalf("ActivePolicy = (%+v, %v), want the enterprise policy", p, err)
	}
}

func TestLocalAllowlistResolvesWhenAlone(t *testing.T) {
	r, repo := structuredChain(t, "", localOnlyPolicy)
	if err := repo.Write(KeyAllowedServers, []string{"from-config"}); err != nil {
		t.Fatal(err)
	}
	servers, source, err := ResolveAllowedServers(r)
	if err != nil || source != SourceLocalAllowlist || !reflect.DeepEqual(servers, []string{"from-local"}) {
		t.Fatalf("ResolveAllowedServers = (%v, %q, %v), want [from-local] shadowing config.toml", servers, source, err)
	}
	p, err := r.ActivePolicy()
	if err != nil || p == nil || p.Source != SourceLocalAllowlist {
		t.Fatalf("ActivePolicy = (%+v, %v), want the local allowlist", p, err)
	}
}

// v0.3.0 compatibility: with neither structured file, config.toml's
// flat list resolves exactly as before and no structured policy is active.
func TestConfigTomlResolvesWhenNoStructuredFile(t *testing.T) {
	r, repo := structuredChain(t, "", "")
	if err := repo.Write(KeyAllowedServers, []string{"from-config"}); err != nil {
		t.Fatal(err)
	}
	servers, source, err := ResolveAllowedServers(r)
	if err != nil || source != SourceRepoConfig || !reflect.DeepEqual(servers, []string{"from-config"}) {
		t.Fatalf("ResolveAllowedServers = (%v, %q, %v), want [from-config] from repo_config", servers, source, err)
	}
	if p, err := r.ActivePolicy(); p != nil || err != nil {
		t.Fatalf("ActivePolicy = (%v, %v), want none", p, err)
	}
}

func TestNoStructuredResolutionWhenNothingIsSet(t *testing.T) {
	r, _ := structuredChain(t, "", "")
	servers, source, err := ResolveAllowedServers(r)
	if err != nil || len(servers) != 0 || source != SourceDefault {
		t.Fatalf("ResolveAllowedServers = (%v, %q, %v), want nothing from default", servers, source, err)
	}
	if p, err := r.ActivePolicy(); p != nil || err != nil {
		t.Fatalf("ActivePolicy = (%v, %v), want none", p, err)
	}
}

func TestMalformedLocalAllowlistFailsClosed(t *testing.T) {
	r, repo := structuredChain(t, "", `{`)
	if err := repo.Write(KeyAllowedServers, []string{"permissive"}); err != nil {
		t.Fatal(err)
	}
	if servers, source, err := ResolveAllowedServers(r); err == nil || !strings.Contains(err.Error(), "local allowlist") {
		t.Fatalf("ResolveAllowedServers = (%v, %q, %v), want an error naming the local allowlist", servers, source, err)
	}
	if p, err := r.ActivePolicy(); err == nil || p != nil {
		t.Fatalf("ActivePolicy = (%v, %v), want an error", p, err)
	}
}

// A broken enterprise policy must not hand control to a valid local
// allowlist, which the user could have written to be more permissive.
func TestMalformedEnterprisePolicyBlocksValidLocalAllowlist(t *testing.T) {
	r, _ := structuredChain(t, `{`, localOnlyPolicy)
	if p, err := r.ActivePolicy(); err == nil || p != nil {
		t.Fatalf("ActivePolicy = (%v, %v), want the enterprise error", p, err)
	}
	if _, _, err := ResolveAllowedServers(r); err == nil || !strings.Contains(err.Error(), "enterprise policy") {
		t.Fatalf("ResolveAllowedServers error = %v, want one naming the enterprise policy", err)
	}
}

func TestPolicyServerLookup(t *testing.T) {
	r, _ := structuredChain(t, "", scopedPolicy)
	p, _ := r.ActivePolicy()
	if s, ok := p.Server("mcp-git"); !ok || s.Server != "mcp-git" {
		t.Fatalf("Server(mcp-git) = (%+v, %v)", s, ok)
	}
	if _, ok := p.Server("other"); ok {
		t.Fatal("Server(other) found an unlisted server")
	}
}

func TestWriteConfigSkipsBothStructuredSources(t *testing.T) {
	r, _ := structuredChain(t, enterpriseOnlyPolicy, localOnlyPolicy)
	source, err := r.WriteConfig(KeyStrictMatching, true)
	if err != nil || source != SourceRepoConfig {
		t.Fatalf("WriteConfig = (%q, %v), want repo_config", source, err)
	}
}
