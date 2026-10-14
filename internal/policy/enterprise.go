package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// enterprisePolicyFile is the one place the enterprise policy's name is
// spelled; it lives beside the user config, in ~/.centrol.
const enterprisePolicyFile = "enterprise-policy.json"

// EnterpriseServer is one allowed MCP server and, optionally, the
// narrower scope it is allowed within. A nil Paths or Tools means the
// field was absent, so the server is unrestricted on that axis; a
// non-nil empty slice means it was present and empty, which is a scope
// of nothing. Both fields are read here and not yet enforced.
type EnterpriseServer struct {
	Server string   `json:"server"`
	Paths  []string `json:"paths"`
	Tools  []string `json:"tools"`
}

type enterprisePolicy struct {
	Servers []EnterpriseServer `json:"servers"`
}

// EnterpriseSource answers policy keys from the enterprise policy file,
// which the enterprise tier writes and this tier only reads. It sits
// above the repo and user configs in the chain, so a key it answers is
// shown and treated as locked by enterprise.
//
// The file is read on every call, like the local config files, so a
// newly written policy applies to the next resolution. An absent file
// means no enterprise policy: nothing resolves and standalone use is
// unaffected. A file that exists but cannot be read or understood fails
// closed: every Resolve returns an error, so no lower, more permissive
// source can answer in its place.
type EnterpriseSource struct {
	path string
}

// NewEnterpriseSource reads ~/.centrol/enterprise-policy.json. If the
// home directory cannot be resolved there is no file to find, and the
// source behaves as if it were absent.
func NewEnterpriseSource() *EnterpriseSource {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return NewEnterpriseSourceAt("")
	}
	return NewEnterpriseSourceAt(filepath.Join(home, ".centrol", enterprisePolicyFile))
}

// NewEnterpriseSourceAt reads the policy from path instead of the
// default location. An empty path never finds a file.
func NewEnterpriseSourceAt(path string) *EnterpriseSource {
	return &EnterpriseSource{path: path}
}

func (e *EnterpriseSource) Name() string { return SourceEnterprisePolicy }

func (e *EnterpriseSource) Writable() bool { return false }

// Servers returns the structured allowlist. found is false, with a nil
// list and no error, when there is no policy file.
func (e *EnterpriseSource) Servers() (servers []EnterpriseServer, found bool, err error) {
	p, err := e.load()
	if err != nil || p == nil {
		return nil, false, err
	}
	return p.Servers, true, nil
}

// Resolve answers proxy.allowed_servers with the policy's server names.
// Every other key is not found, but a policy that fails to load is an
// error for every key, not only that one.
func (e *EnterpriseSource) Resolve(key string) (interface{}, bool, error) {
	p, err := e.load()
	if err != nil || p == nil || key != KeyAllowedServers {
		return nil, false, err
	}
	names := make([]string, len(p.Servers))
	for i, s := range p.Servers {
		names[i] = s.Server
	}
	return names, true, nil
}

// load returns a nil policy when the file does not exist.
func (e *EnterpriseSource) load() (*enterprisePolicy, error) {
	if e.path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(e.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("enterprise policy %s cannot be read, so no policy will resolve until it can: %w", e.path, err)
	}
	p, err := parseEnterprisePolicy(data)
	if err != nil {
		return nil, fmt.Errorf("enterprise policy %s is invalid, so no policy will resolve until it is fixed: %w", e.path, err)
	}
	return p, nil
}

// parseEnterprisePolicy is strict because the file is a restriction: a
// field this version does not know is a rule it cannot honor, and
// ignoring it would silently loosen the policy.
func parseEnterprisePolicy(data []byte) (*enterprisePolicy, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p enterprisePolicy
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected content after the policy object")
	}
	if p.Servers == nil {
		return nil, fmt.Errorf(`"servers" is required (use an empty list to allow no servers)`)
	}
	seen := make(map[string]bool, len(p.Servers))
	for i, s := range p.Servers {
		if strings.TrimSpace(s.Server) == "" {
			return nil, fmt.Errorf(`servers[%d]: "server" must not be empty`, i)
		}
		if seen[s.Server] {
			return nil, fmt.Errorf("servers[%d]: %q is listed more than once", i, s.Server)
		}
		seen[s.Server] = true
	}
	return &p, nil
}
