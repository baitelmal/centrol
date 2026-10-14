package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const scopedPolicy = `{
  "servers": [
    {"server": "server-filesystem", "paths": ["src/**", "docs/**"], "tools": ["read_file", "list_directory"]},
    {"server": "mcp-git"}
  ]
}`

func TestEnterpriseSourceIdentity(t *testing.T) {
	e := NewEnterpriseSourceAt(writePolicyFile(t, "enterprise-policy.json", scopedPolicy))
	if e.Writable() {
		t.Fatal("the enterprise source must never be writable")
	}
	if e.Name() != SourceEnterprisePolicy {
		t.Fatalf("Name = %q, want %q", e.Name(), SourceEnterprisePolicy)
	}
}

func TestEnterpriseSourceAbsentFileFindsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for name, e := range map[string]*EnterpriseSource{
		"missing file":     NewEnterpriseSourceAt(filepath.Join(t.TempDir(), "enterprise-policy.json")),
		"unresolved home":  NewEnterpriseSourceAt(""),
		"default location": NewEnterpriseSource(),
	} {
		v, found, err := e.Resolve(KeyAllowedServers)
		if v != nil || found || err != nil {
			t.Errorf("%s: Resolve = (%v, %v, %v), want (nil, false, nil)", name, v, found, err)
		}
		if p, err := e.Policy(); p != nil || err != nil {
			t.Errorf("%s: Policy = (%v, %v), want (nil, nil)", name, p, err)
		}
	}
}

func TestEnterpriseSourceDefaultPathIsUnderCentrolDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".centrol")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enterprise-policy.json"), []byte(scopedPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewEnterpriseSource().Policy()
	if err != nil || p == nil || len(p.Servers) != 2 || p.Source != SourceEnterprisePolicy {
		t.Fatalf("Policy = (%+v, %v), want the two servers from ~/.centrol/enterprise-policy.json", p, err)
	}
}

func TestEnterpriseSourceResolvesServerNames(t *testing.T) {
	e := NewEnterpriseSourceAt(writePolicyFile(t, "enterprise-policy.json", scopedPolicy))
	v, found, err := e.Resolve(KeyAllowedServers)
	if err != nil || !found {
		t.Fatalf("Resolve = (%v, %v, %v), want the server names", v, found, err)
	}
	if want := []string{"server-filesystem", "mcp-git"}; !reflect.DeepEqual(v, want) {
		t.Fatalf("Resolve = %v, want %v", v, want)
	}
	// Other keys stay with the lower sources.
	for _, key := range []string{KeyStrictMatching, KeyAdditionalBlockPaths, KeyPromptTimeoutSeconds} {
		if v, found, err := e.Resolve(key); v != nil || found || err != nil {
			t.Errorf("Resolve(%q) = (%v, %v, %v), want not found", key, v, found, err)
		}
	}
}

func TestEnterprisePolicyCarriesScope(t *testing.T) {
	path := writePolicyFile(t, "enterprise-policy.json", scopedPolicy)
	p, err := NewEnterpriseSourceAt(path).Policy()
	if err != nil || p == nil {
		t.Fatalf("Policy = (%v, %v)", p, err)
	}
	if p.Path != path {
		t.Errorf("Path = %q, want %q", p.Path, path)
	}
	servers := p.Servers
	want := []PolicyServer{
		{Server: "server-filesystem", Paths: []string{"src/**", "docs/**"}, Tools: []string{"read_file", "list_directory"}},
		{Server: "mcp-git"},
	}
	if !reflect.DeepEqual(servers, want) {
		t.Fatalf("Servers = %+v, want %+v", servers, want)
	}
}

// Absent paths/tools mean unrestricted; present-but-empty means nothing
// is allowed. The two must stay distinguishable for enforcement.
func TestPolicyServerScopeFieldsAreOptional(t *testing.T) {
	p, err := NewEnterpriseSourceAt(writePolicyFile(t, "enterprise-policy.json",
		`{"servers":[{"server":"a"},{"server":"b","paths":[],"tools":[]}]}`)).Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	servers := p.Servers
	if servers[0].Paths != nil || servers[0].Tools != nil {
		t.Errorf("absent fields must stay nil, got %+v", servers[0])
	}
	if servers[1].Paths == nil || servers[1].Tools == nil || len(servers[1].Paths)+len(servers[1].Tools) != 0 {
		t.Errorf("empty fields must stay non-nil and empty, got %+v", servers[1])
	}
}

func TestEnterpriseSourceEmptyServerListAllowsNone(t *testing.T) {
	e := NewEnterpriseSourceAt(writePolicyFile(t, "enterprise-policy.json", `{"servers": []}`))
	v, found, err := e.Resolve(KeyAllowedServers)
	if err != nil || !found {
		t.Fatalf("Resolve = (%v, %v, %v), want found with an empty list", v, found, err)
	}
	if names, ok := v.([]string); !ok || len(names) != 0 {
		t.Fatalf("Resolve = %#v, want an empty []string", v)
	}
}

func TestEnterpriseSourceMalformedPolicyFailsClosed(t *testing.T) {
	cases := map[string]string{
		"not json":          `servers: [a]`,
		"empty file":        ``,
		"truncated":         `{"servers": [{"server": "a"}`,
		"servers missing":   `{}`,
		"servers null":      `{"servers": null}`,
		"wrong type":        `{"servers": "a"}`,
		"unknown field":     `{"servers": [{"server": "a", "teams": ["x"]}]}`,
		"unknown top-level": `{"servers": [], "version": 2}`,
		"empty server name": `{"servers": [{"server": " "}]}`,
		"missing name":      `{"servers": [{"paths": ["src/**"]}]}`,
		"duplicate server":  `{"servers": [{"server": "a"}, {"server": "a"}]}`,
		"trailing content":  `{"servers": []} {"servers": []}`,
		"paths not strings": `{"servers": [{"server": "a", "paths": [1]}]}`,
	}
	for name, content := range cases {
		e := NewEnterpriseSourceAt(writePolicyFile(t, "enterprise-policy.json", content))
		for _, key := range []string{KeyAllowedServers, KeyStrictMatching} {
			v, found, err := e.Resolve(key)
			if err == nil || found || v != nil {
				t.Errorf("%s: Resolve(%q) = (%v, %v, %v), want an error and no value", name, key, v, found, err)
			}
		}
		if p, err := e.Policy(); err == nil || p != nil {
			t.Errorf("%s: Policy = (%v, %v), want an error", name, p, err)
		}
	}
}

func TestEnterpriseSourceUnreadablePolicyFailsClosed(t *testing.T) {
	// A directory where the file should be is readable as neither
	// "missing" nor "valid".
	path := filepath.Join(t.TempDir(), "enterprise-policy.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, found, err := NewEnterpriseSourceAt(path).Resolve(KeyAllowedServers); err == nil || found {
		t.Fatalf("Resolve = found %v, err %v, want an error", found, err)
	}
}

// A malformed enterprise policy must stop resolution outright rather
// than let the user's own, more permissive allowlist answer.
func TestMalformedEnterprisePolicyDoesNotFallThroughTheChain(t *testing.T) {
	dir := t.TempDir()
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "user.toml"))
	if err := user.Write(KeyAllowedServers, []string{"anything"}); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(
		NewSessionContractSource(),
		NewEnterpriseSourceAt(writePolicyFile(t, "enterprise-policy.json", `{`)),
		NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "repo.toml")),
		user,
		NewDefaultSource(nil),
	)
	servers, source, err := ResolveAllowedServers(r)
	if err == nil {
		t.Fatalf("ResolveAllowedServers = (%v, %q), want an error", servers, source)
	}
	if !strings.Contains(err.Error(), SourceEnterprisePolicy) || !strings.Contains(err.Error(), "enterprise-policy.json") {
		t.Fatalf("error should name the source and the file, got: %v", err)
	}
}

func TestEnterprisePolicyWinsTheChainForAllowedServers(t *testing.T) {
	dir := t.TempDir()
	session := NewSessionContractSource()
	repo := NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "repo.toml"))
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "user.toml"))
	r := NewResolver(
		session,
		NewEnterpriseSourceAt(writePolicyFile(t, "enterprise-policy.json", `{"servers":[{"server":"approved"}]}`)),
		repo, user, NewDefaultSource(nil),
	)
	if err := repo.Write(KeyAllowedServers, []string{"repo-choice"}); err != nil {
		t.Fatal(err)
	}
	if err := user.Write(KeyStrictMatching, true); err != nil {
		t.Fatal(err)
	}

	servers, source, err := ResolveAllowedServers(r)
	if err != nil || source != SourceEnterprisePolicy || !reflect.DeepEqual(servers, []string{"approved"}) {
		t.Fatalf("ResolveAllowedServers = (%v, %q, %v), want [approved] from %s", servers, source, err, SourceEnterprisePolicy)
	}
	// A key the enterprise policy does not answer still falls through.
	strict, source, err := ResolveStrictMatching(r)
	if err != nil || !strict || source != SourceUserConfig {
		t.Fatalf("ResolveStrictMatching = (%v, %q, %v), want true from user_config", strict, source, err)
	}
	// Session overrides still outrank the enterprise source.
	session.Set(KeyAllowedServers, []string{"session"})
	if servers, source, _ = ResolveAllowedServers(r); source != SourceSessionContract {
		t.Fatalf("source = %q (%v), want session_contract above enterprise", source, servers)
	}
}

// Writes skip the enterprise source, so the policy file is never
// touched by the free tier.
func TestWriteConfigNeverTargetsTheEnterprisePolicy(t *testing.T) {
	dir := t.TempDir()
	path := writePolicyFile(t, "enterprise-policy.json", scopedPolicy)
	r := NewResolver(
		NewSessionContractSource(),
		NewEnterpriseSourceAt(path),
		NewLocalFileSource(SourceRepoConfig, filepath.Join(dir, "repo.toml")),
		NewDefaultSource(nil),
	)
	source, err := r.WriteConfig(KeyStrictMatching, true)
	if err != nil || source != SourceRepoConfig {
		t.Fatalf("WriteConfig = (%q, %v), want repo_config", source, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != scopedPolicy {
		t.Fatalf("enterprise policy file was modified:\n%s", got)
	}
}
