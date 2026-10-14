package policy

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The enterprise policy is signed by the Meta-ARK and verified here
// against a pinned public key. Three files cooperate, all in
// ~/.centrol and all in TrustedFiles:
//
//	enterprise-pubkey                  the Meta-ARK public key, base64
//	enterprise-policy.json             the policy, with a top-level "signature"
//	enterprise-policy.verified.json    the last policy whose signature verified
//
// No pubkey means a solo user: no signature is required and the policy
// file is read as before. With a pubkey the policy must verify. A bad
// signature does not lock the station out: the last verified policy,
// always the most recent valid push, stays in force, so a single
// tamper (a botched push, a race, an agent's write) changes nothing.
// Only when that fallback is also gone or invalid does it fail closed.
//
// The pubkey arrives with the first policy, not at enrollment, so an
// enrolled station with nothing pushed yet stays on its solo behavior
// and the move to enterprise-managed is atomic.
//
// Limitation: an agent running as the same user can overwrite all three
// files. A single overwrite is inert (the fallback), and a coordinated
// one needs all three written consistently; every write is recorded by
// the guard as a block-tier policy.violation, so the intent is plain in
// the ledger. This is detection and layering, not prevention;
// prevention is sandboxing, which is out of scope here.

const (
	enterprisePubkeyFile   = "enterprise-pubkey"
	enterpriseVerifiedFile = "enterprise-policy.verified.json"
	signatureField         = "signature"
)

// signedPolicy locates the files beside an enterprise policy file.
type signedPolicy struct {
	pubkeyPath   string
	verifiedPath string
}

// signedPolicyBeside derives the pubkey and verified-store paths from
// the policy's own directory. The default policy path is in
// ~/.centrol, so these agree with TrustedFiles; a test pointing the
// source at a temp directory gets its siblings there.
func signedPolicyBeside(policyPath string) *signedPolicy {
	if policyPath == "" {
		return nil
	}
	dir := filepath.Dir(policyPath)
	return &signedPolicy{
		pubkeyPath:   filepath.Join(dir, enterprisePubkeyFile),
		verifiedPath: filepath.Join(dir, enterpriseVerifiedFile),
	}
}

// load returns (nil, nil) when there is no pubkey, meaning the caller
// reads the policy file unsigned. Otherwise it returns the verified
// policy, the fallback, or an error that fails closed.
func (s *signedPolicy) load(f policyFile) (*StructuredPolicy, error) {
	pubText, err := os.ReadFile(s.pubkeyPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read, so no policy will resolve until it can: %w", s.pubkeyPath, err)
	}
	pub, err := parsePubkey(pubText)
	if err != nil {
		return nil, fmt.Errorf("%s is invalid, so no policy will resolve until it is fixed: %w", s.pubkeyPath, err)
	}

	data, policyErr := os.ReadFile(f.path)
	policyMissing := os.IsNotExist(policyErr)
	var servers []PolicyServer
	if policyErr == nil {
		servers, policyErr = verifyPolicy(data, pub)
	}
	if policyErr == nil {
		s.remember(data)
		return &StructuredPolicy{Source: f.name, Path: f.path, Servers: servers}, nil
	}

	// The policy is missing or does not verify: use the last verified one.
	vdata, err := os.ReadFile(s.verifiedPath)
	if os.IsNotExist(err) {
		if policyMissing {
			// Enterprise-managed, nothing pushed yet: no servers permitted.
			return &StructuredPolicy{Source: f.name, Path: f.path, Servers: []PolicyServer{}}, nil
		}
		return nil, fmt.Errorf("%s does not verify (%v) and there is no %s to fall back to, so no policy will resolve until a verified policy is pushed",
			f.path, policyErr, s.verifiedPath)
	}
	var verr error
	var vservers []PolicyServer
	if err != nil {
		verr = err
	} else {
		vservers, verr = verifyPolicy(vdata, pub)
	}
	if verr != nil {
		return nil, fmt.Errorf("%s (%v) and the last verified policy %s (%v) are both unusable, so no policy will resolve until a verified policy is pushed",
			f.path, policyErr, s.verifiedPath, verr)
	}
	return &StructuredPolicy{Source: f.name, Path: s.verifiedPath, Servers: vservers}, nil
}

// remember stores a verified policy's exact bytes, so the store can be
// verified again later. It writes only when the content changed: the
// guard records every write to this file, and rewriting it on every
// read would bury the real ones. A failure is reported but does not
// drop a policy that did verify.
func (s *signedPolicy) remember(data []byte) {
	if have, err := os.ReadFile(s.verifiedPath); err == nil && bytes.Equal(have, data) {
		return
	}
	if err := writeFileAtomic(s.verifiedPath, data); err != nil {
		fmt.Fprintf(os.Stderr, "centrol: warning: could not record the verified policy in %s: %v\n", s.verifiedPath, err)
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func parsePubkey(text []byte) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("an ed25519 public key is %d bytes, this is %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// verifyPolicy checks the signature over the canonical JSON of the
// document without its "signature" field, then parses the servers with
// the same strict parser the unsigned files use.
func verifyPolicy(data []byte, pub ed25519.PublicKey) ([]PolicyServer, error) {
	doc, err := decodeGeneric(data)
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(map[string]interface{})
	if !ok {
		return nil, errors.New("the policy is not a JSON object")
	}
	sigText, ok := obj[signatureField].(string)
	if !ok {
		return nil, errors.New(`no "signature"`)
	}
	sig, err := base64.StdEncoding.DecodeString(sigText)
	if err != nil {
		return nil, fmt.Errorf("signature is not base64: %w", err)
	}
	delete(obj, signatureField)
	signed, err := canonicalJSON(obj)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(pub, signed, sig) {
		return nil, errors.New("signature does not verify against the pinned key")
	}
	return parsePolicyServers(signed)
}

func decodeGeneric(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected content after the policy object")
	}
	return v, nil
}

// canonicalJSON is the byte form the signature covers: object keys
// sorted, no insignificant whitespace, no HTML escaping, numbers kept
// as written. It is the same form centrol-e's org.canonicalJSON
// produces, so the signing side and this verifier agree, and key order
// or layout in the file does not matter.
func canonicalJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("canonicalizing: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
