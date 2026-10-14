package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLocalAllowlistSourceIdentity(t *testing.T) {
	l := NewLocalAllowlistSourceAt(writePolicyFile(t, "allowlist.json", scopedPolicy))
	if l.Writable() {
		t.Fatal("the free tier never writes the local allowlist")
	}
	if l.Name() != SourceLocalAllowlist {
		t.Fatalf("Name = %q, want %q", l.Name(), SourceLocalAllowlist)
	}
}

func TestLocalAllowlistSourceDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if p, err := NewLocalAllowlistSource().Policy(); p != nil || err != nil {
		t.Fatalf("with no file, Policy = (%v, %v), want (nil, nil)", p, err)
	}
	dir := filepath.Join(home, ".centrol")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "allowlist.json"), []byte(scopedPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewLocalAllowlistSource().Policy()
	if err != nil || p == nil || len(p.Servers) != 2 || p.Source != SourceLocalAllowlist {
		t.Fatalf("Policy = (%+v, %v), want the two servers from ~/.centrol/allowlist.json", p, err)
	}
}

func TestLocalAllowlistSourceSharesTheEnterpriseSchema(t *testing.T) {
	l := NewLocalAllowlistSourceAt(writePolicyFile(t, "allowlist.json", scopedPolicy))
	v, found, err := l.Resolve(KeyAllowedServers)
	if err != nil || !found || !reflect.DeepEqual(v, []string{"server-filesystem", "mcp-git"}) {
		t.Fatalf("Resolve = (%v, %v, %v), want the server names", v, found, err)
	}
	if v, found, err := l.Resolve(KeyStrictMatching); v != nil || found || err != nil {
		t.Fatalf("an unrelated key must not resolve, got (%v, %v, %v)", v, found, err)
	}
	// The same strictness: an unknown field is a rule this version cannot honor.
	bad := NewLocalAllowlistSourceAt(writePolicyFile(t, "allowlist.json", `{"servers":[{"server":"a","teams":[]}]}`))
	if _, _, err := bad.Resolve(KeyAllowedServers); err == nil {
		t.Fatal("expected a malformed local allowlist to be an error")
	}
}
