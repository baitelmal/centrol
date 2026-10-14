package policy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signedDir is a ~/.centrol stand-in with a freshly generated Meta-ARK
// key. The tests sign with the private half, as the enterprise tier does.
type signedDir struct {
	t    *testing.T
	dir  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSignedDir(t *testing.T) *signedDir {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signedDir{t: t, dir: t.TempDir(), pub: pub, priv: priv}
}

func (d *signedDir) path(name string) string { return filepath.Join(d.dir, name) }

func (d *signedDir) write(name, content string) {
	d.t.Helper()
	if err := os.WriteFile(d.path(name), []byte(content), 0o600); err != nil {
		d.t.Fatal(err)
	}
}

func (d *signedDir) read(name string) string {
	d.t.Helper()
	b, err := os.ReadFile(d.path(name))
	if err != nil {
		d.t.Fatal(err)
	}
	return string(b)
}

func (d *signedDir) pin(pub ed25519.PublicKey) {
	d.write("enterprise-pubkey", base64.StdEncoding.EncodeToString(pub)+"\n")
}

// signed renders body (a JSON object without its closing brace stripped
// of the signature) with a valid signature over its canonical form.
func (d *signedDir) signed(body string) string {
	d.t.Helper()
	doc, err := decodeGeneric([]byte(body))
	if err != nil {
		d.t.Fatal(err)
	}
	canon, err := canonicalJSON(doc)
	if err != nil {
		d.t.Fatal(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(d.priv, canon))
	return strings.TrimSuffix(strings.TrimSpace(body), "}") + `, "signature": "` + sig + `"}`
}

func (d *signedDir) source() *EnterpriseSource {
	return NewEnterpriseSourceAt(d.path("enterprise-policy.json"))
}

func serverNames(p *StructuredPolicy) []string {
	var names []string
	for _, s := range p.Servers {
		names = append(names, s.Server)
	}
	return names
}

const (
	policyA = `{"servers":[{"server":"mcp-git"}]}`
	policyB = `{"servers":[{"server":"mcp-git"},{"server":"server-filesystem"}]}`
)

func wantServers(t *testing.T, e *EnterpriseSource, want ...string) *StructuredPolicy {
	t.Helper()
	p, err := e.Policy()
	if err != nil || p == nil {
		t.Fatalf("Policy = (%v, %v), want servers %v", p, err, want)
	}
	got := serverNames(p)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("servers = %v, want %v", got, want)
	}
	return p
}

func TestValidSignatureActivatesThePolicyAndWritesTheVerifiedStore(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	file := d.signed(policyB)
	d.write("enterprise-policy.json", file)

	wantServers(t, d.source(), "mcp-git", "server-filesystem")
	if got := d.read("enterprise-policy.verified.json"); got != file {
		t.Fatalf("verified store = %q, want the signed file verbatim %q", got, file)
	}
}

func TestVerifiedStoreIsRewrittenOnlyWhenTheContentChanges(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	d.write("enterprise-policy.json", d.signed(policyA))
	wantServers(t, d.source(), "mcp-git")
	store := d.path("enterprise-policy.verified.json")
	old := mustStat(t, store)
	wantServers(t, d.source(), "mcp-git")
	if !mustStat(t, store).ModTime().Equal(old.ModTime()) {
		t.Fatal("an unchanged policy rewrote the verified store; the guard would record every read")
	}
	d.write("enterprise-policy.json", d.signed(policyB))
	wantServers(t, d.source(), "mcp-git", "server-filesystem")
	if d.read("enterprise-policy.verified.json") != d.read("enterprise-policy.json") {
		t.Fatal("a new valid push must replace the verified store")
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// pushA then tamper: the station keeps the last verified policy, and
// does not lock out.
func TestContentTamperFallsBackToTheLastVerifiedPolicy(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	d.write("enterprise-policy.json", d.signed(policyA))
	wantServers(t, d.source(), "mcp-git")

	// Widen the policy by hand, leaving the old signature in place.
	tampered := strings.Replace(d.read("enterprise-policy.json"), `"mcp-git"`, `"mcp-git"},{"server":"evil"`, 1)
	d.write("enterprise-policy.json", tampered)

	p := wantServers(t, d.source(), "mcp-git")
	if p.Path != d.path("enterprise-policy.verified.json") {
		t.Errorf("Path = %q, want the verified store, so the fallback is visible", p.Path)
	}
}

func TestSignatureTamperFallsBack(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	signedA := d.signed(policyA)
	d.write("enterprise-policy.json", signedA)
	wantServers(t, d.source(), "mcp-git")

	// A different, wider policy carrying A's signature: well-formed, wrong.
	sigA := signedA[strings.Index(signedA, `"signature": "`)+len(`"signature": "`):]
	sigA = sigA[:strings.Index(sigA, `"`)]
	d.write("enterprise-policy.json", strings.TrimSuffix(policyB, "}")+`, "signature": "`+sigA+`"}`)
	wantServers(t, d.source(), "mcp-git")
}

func TestMissingPolicyFallsBackToTheVerifiedStore(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	d.write("enterprise-policy.json", d.signed(policyB))
	wantServers(t, d.source(), "mcp-git", "server-filesystem")
	if err := os.Remove(d.path("enterprise-policy.json")); err != nil {
		t.Fatal(err)
	}
	wantServers(t, d.source(), "mcp-git", "server-filesystem")
}

func TestMissingPolicyAndMissingStoreFailsClosedToNoServers(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	p, err := d.source().Policy()
	if err != nil || p == nil || len(p.Servers) != 0 {
		t.Fatalf("Policy = (%+v, %v), want an active policy that permits no servers", p, err)
	}
	// And it is active: it shadows a lower source rather than yielding to it.
	r := NewResolver(d.source())
	if active, err := r.ActivePolicy(); err != nil || active == nil || len(active.Servers) != 0 {
		t.Fatalf("ActivePolicy = (%+v, %v), want the empty enterprise policy", active, err)
	}
}

func TestBadPolicyAndMissingStoreFailsClosedNamingBothFiles(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	d.write("enterprise-policy.json", `{"servers":[{"server":"evil"}], "signature":"AAAA"}`)
	_, err := d.source().Policy()
	assertNamesBoth(t, err, d)
}

func TestCoordinatedTamperFailsClosedNamingBothFiles(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	d.write("enterprise-policy.json", d.signed(policyA))
	wantServers(t, d.source(), "mcp-git")

	bad := `{"servers":[{"server":"evil"}], "signature":"AAAA"}`
	d.write("enterprise-policy.json", bad)
	d.write("enterprise-policy.verified.json", bad)
	p, err := d.source().Policy()
	if p != nil {
		t.Fatalf("a coordinated tamper produced a policy: %+v", p)
	}
	assertNamesBoth(t, err, d)
	if _, found, err := d.source().Resolve(KeyAllowedServers); found || err == nil {
		t.Fatalf("Resolve = (found %v, %v), want an error for every key", found, err)
	}
}

func assertNamesBoth(t *testing.T, err error, d *signedDir) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got none")
	}
	for _, n := range []string{"enterprise-policy.json", "enterprise-policy.verified.json"} {
		if !strings.Contains(err.Error(), d.path(n)) {
			t.Errorf("error %q does not name %s", err, n)
		}
	}
}

func TestNoPubkeyMeansNoSignatureCheck(t *testing.T) {
	d := newSignedDir(t)
	d.write("enterprise-policy.json", policyB) // unsigned
	wantServers(t, d.source(), "mcp-git", "server-filesystem")
	if _, err := os.Stat(d.path("enterprise-policy.verified.json")); !os.IsNotExist(err) {
		t.Fatal("the solo path must not create a verified store")
	}
}

func TestSignedPolicyWithoutAPubkeyFailsClosed(t *testing.T) {
	d := newSignedDir(t)
	d.write("enterprise-policy.json", d.signed(policyA))
	if _, err := d.source().Policy(); err == nil {
		t.Fatal("a signed policy with no pinned key must fail closed, not be read unsigned")
	}
}

func TestUnsignedPolicyUnderAPubkeyIsInvalid(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	d.write("enterprise-policy.json", d.signed(policyA))
	wantServers(t, d.source(), "mcp-git")
	d.write("enterprise-policy.json", policyB)
	wantServers(t, d.source(), "mcp-git")
}

func TestWrongPinnedKeyIsInvalidAndFallsBack(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	d.write("enterprise-policy.json", d.signed(policyA))
	wantServers(t, d.source(), "mcp-git")

	other, _, _ := ed25519.GenerateKey(rand.Reader)
	d.pin(other) // nothing verifies now, policy or store
	_, err := d.source().Policy()
	assertNamesBoth(t, err, d)
}

func TestMalformedPubkeyFailsClosed(t *testing.T) {
	d := newSignedDir(t)
	d.write("enterprise-pubkey", "not a key")
	d.write("enterprise-policy.json", d.signed(policyA))
	_, err := d.source().Policy()
	if err == nil || !strings.Contains(err.Error(), d.path("enterprise-pubkey")) {
		t.Fatalf("err = %v, want an error naming enterprise-pubkey", err)
	}
}

func TestKeyOrderAndLayoutDoNotBreakTheSignature(t *testing.T) {
	d := newSignedDir(t)
	d.pin(d.pub)
	file := d.signed(`{"servers":[{"server":"s","paths":["a/**"],"tools":["t"]}]}`)
	// Same document, keys reordered and re-indented, signature first.
	sig := file[strings.Index(file, `"signature": "`)+len(`"signature": "`):]
	sig = sig[:strings.Index(sig, `"`)]
	reordered := "{\n  \"signature\": \"" + sig + "\",\n  \"servers\": [\n    {\"tools\": [\"t\"], \"paths\": [\"a/**\"], \"server\": \"s\"}\n  ]\n}\n"
	d.write("enterprise-policy.json", reordered)
	p := wantServers(t, d.source(), "s")
	if p.Path != d.path("enterprise-policy.json") {
		t.Fatalf("Path = %q: a reordered file must verify itself, not fall back", p.Path)
	}
}

// The verified store lives beside the policy, and the default location
// agrees with the protected set, so the reader and the guard cannot
// disagree about which files matter.
func TestSignedFilePathsAgreeWithTheTrustedSet(t *testing.T) {
	home := trustedHome(t)
	s := signedPolicyBeside(centrolHomeFile(enterprisePolicyFile))
	trusted := map[string]bool{}
	for _, f := range TrustedFiles("") {
		trusted[f] = true
	}
	for _, p := range []string{s.pubkeyPath, s.verifiedPath} {
		if !trusted[p] || filepath.Dir(p) != filepath.Join(home, ".centrol") {
			t.Errorf("%s is read by the signed loader but not in TrustedFiles", p)
		}
	}
}
