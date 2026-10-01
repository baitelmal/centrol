package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Source identifiers, used both as PolicySource.Name() and as the
// "source" field on ledger payloads for policy decisions — so an audit
// can distinguish "the user allowed this" from "the org required this".
const (
	SourceSessionContract  = "session_contract"
	SourceEnterprisePolicy = "enterprise_policy"
	SourceRepoConfig       = "repo_config"
	SourceUserConfig       = "user_config"
	SourceDefault          = "default"
)

// PolicySource is one place a policy value can come from. Every
// resolved value carries the Name() of the source it came from, so
// resolution provenance is always known, not just the value.
type PolicySource interface {
	Name() string
	Resolve(key string) (value interface{}, found bool, err error)
	Writable() bool
}

// SessionContractSource is the highest-priority source: per-run
// overrides that live only in memory for the duration of one run and
// are never persisted (consistent with the contract itself being
// ephemeral and purged at run end). Nothing sets these yet in v0.1 —
// the seam exists so a future per-run override (e.g. a CLI flag) has
// somewhere to plug in without changing the resolution chain's shape.
type SessionContractSource struct {
	mu     sync.RWMutex
	values map[string]interface{}
}

func NewSessionContractSource() *SessionContractSource {
	return &SessionContractSource{values: map[string]interface{}{}}
}

func (s *SessionContractSource) Name() string { return SourceSessionContract }

func (s *SessionContractSource) Resolve(key string) (interface{}, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok, nil
}

func (s *SessionContractSource) Writable() bool { return true }

// Set installs a per-run override. Not persisted anywhere — purged with
// the rest of the session contract when the run ends.
func (s *SessionContractSource) Set(key string, value interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
}

// EnterpriseSource is a real stub, not a placeholder: it is instantiated
// and takes part in the resolution chain like any other source, but
// returns found=false for everything and is never writable. No network
// calls are made. Swapping this for a real implementation later changes
// nothing at any call site — every call site talks to the PolicySource
// interface, never to this type.
type EnterpriseSource struct{}

func NewEnterpriseSource() *EnterpriseSource { return &EnterpriseSource{} }

func (e *EnterpriseSource) Name() string { return SourceEnterprisePolicy }

func (e *EnterpriseSource) Resolve(key string) (interface{}, bool, error) {
	return nil, false, nil
}

func (e *EnterpriseSource) Writable() bool { return false }

// LocalFileSource reads (and, if writable, writes) one centrol config
// file in the [section] key = value subset of TOML this package
// supports. Two instances exist in the normal chain: one for the repo
// config (.centrol/config.toml) and one for the user config
// (~/.centrol/config.toml) — they are separate PolicySources, not one
// source reading two files, because they have different priority and a
// different Name() for provenance.
type LocalFileSource struct {
	name string
	path string
}

// NewLocalFileSource builds a source over one config file. name should
// be SourceRepoConfig or SourceUserConfig so provenance in the ledger
// is meaningful; path may not exist yet (Resolve then just finds
// nothing, Write creates it).
func NewLocalFileSource(name, path string) *LocalFileSource {
	return &LocalFileSource{name: name, path: path}
}

func (l *LocalFileSource) Name() string { return l.name }

func (l *LocalFileSource) Writable() bool { return true }

// splitKey splits a "section.key" resolution key. Every config key in
// this package uses that shape (e.g. "proxy.mass_mutation_threshold").
func splitKey(key string) (section, k string, err error) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("policy key %q must be of the form \"section.key\"", key)
}

func (l *LocalFileSource) load() (tomlDoc, error) {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return tomlDoc{}, nil
		}
		return nil, err
	}
	return parseTOMLSubset(data)
}

func (l *LocalFileSource) Resolve(key string) (interface{}, bool, error) {
	section, k, err := splitKey(key)
	if err != nil {
		return nil, false, err
	}
	doc, err := l.load()
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", l.path, err)
	}
	sec, ok := doc[section]
	if !ok {
		return nil, false, nil
	}
	v, ok := sec[k]
	return v, ok, nil
}

// Write sets one key in this config file, creating the file and its
// directory if needed, and preserving every other key already there.
func (l *LocalFileSource) Write(key string, value interface{}) error {
	section, k, err := splitKey(key)
	if err != nil {
		return err
	}
	doc, err := l.load()
	if err != nil {
		return fmt.Errorf("reading %s: %w", l.path, err)
	}
	if doc[section] == nil {
		doc[section] = map[string]interface{}{}
	}
	doc[section][k] = value

	// 0700/0600: config.toml can carry an operator-entered proxy.target_url
	// with embedded credentials (e.g. a token in the URL's query string or
	// userinfo) — the .centrol-rooted directory and the file itself must
	// not be world-readable, matching the same bar applied to the ledger
	// and session markers (internal/ledger.Open, internal/session).
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, writeTOMLSubset(doc), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// DefaultSource is the lowest-priority source: centrol's own built-in
// defaults. Always found, never writable.
type DefaultSource struct {
	values map[string]interface{}
}

func NewDefaultSource(values map[string]interface{}) *DefaultSource {
	return &DefaultSource{values: values}
}

func (d *DefaultSource) Name() string { return SourceDefault }

func (d *DefaultSource) Resolve(key string) (interface{}, bool, error) {
	v, ok := d.values[key]
	return v, ok, nil
}

func (d *DefaultSource) Writable() bool { return false }

// Resolver walks a fixed priority chain of PolicySources and returns
// the first value found, along with which source it came from — the
// provenance every ledger entry for a policy decision needs.
type Resolver struct {
	sources []PolicySource
}

// NewResolver builds a resolution chain from sources in priority order,
// highest first. The standard v0.1 chain is: session contract,
// enterprise policy, repo config, user config, built-in defaults.
func NewResolver(sources ...PolicySource) *Resolver {
	return &Resolver{sources: sources}
}

// Resolve returns the first found value plus the Name() of the source
// it came from. If nothing in the chain has the key, found is false.
func (r *Resolver) Resolve(key string) (value interface{}, source string, found bool, err error) {
	for _, s := range r.sources {
		v, ok, serr := s.Resolve(key)
		if serr != nil {
			return nil, "", false, fmt.Errorf("resolving %q from %s: %w", key, s.Name(), serr)
		}
		if ok {
			return v, s.Name(), true, nil
		}
	}
	return nil, "", false, nil
}

// WritableSource returns the highest-priority writable, non-enterprise
// source in the chain — where "Add to allowlist" and similar
// menu-driven writes land by default (repo config, ahead of user
// config, ahead of nothing if neither is configured writable).
func (r *Resolver) WritableSource() (PolicySource, bool) {
	for _, s := range r.sources {
		if s.Name() == SourceSessionContract || s.Name() == SourceEnterprisePolicy {
			continue // session contract isn't persisted; enterprise is never writable
		}
		if s.Writable() {
			return s, true
		}
	}
	return nil, false
}

// WriteConfig persists key=value to the highest-priority persisted,
// writable source in the chain (normally repo config, then user
// config) and returns that source's Name() for the caller to log as
// provenance. Only *LocalFileSource actually knows how to persist a
// "section.key" write in v0.1 — if the resolved writable source isn't
// one (shouldn't happen with the standard chain), this reports a clear
// error rather than silently doing nothing.
func (r *Resolver) WriteConfig(key string, value interface{}) (source string, err error) {
	s, ok := r.WritableSource()
	if !ok {
		return "", fmt.Errorf("no writable config source available (repo and user config are both unavailable)")
	}
	lf, ok := s.(*LocalFileSource)
	if !ok {
		return "", fmt.Errorf("writable source %s does not support persisted config writes", s.Name())
	}
	if err := lf.Write(key, value); err != nil {
		return "", err
	}
	return s.Name(), nil
}
