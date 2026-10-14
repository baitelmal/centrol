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

// PolicyServer is one allowed MCP server and, optionally, the narrower
// scope it is allowed within. A nil Paths or Tools means the field was
// absent, so the server is unrestricted on that axis; a non-nil empty
// slice means it was present and empty, which is a scope of nothing.
type PolicyServer struct {
	Server string   `json:"server"`
	Paths  []string `json:"paths"`
	Tools  []string `json:"tools"`
}

// StructuredPolicy is a parsed structured MCP policy file, tagged with
// the source that supplied it so callers can say whose policy is in
// force. Enterprise and local policies share this schema.
type StructuredPolicy struct {
	Source  string // SourceEnterprisePolicy or SourceLocalAllowlist
	Path    string
	Servers []PolicyServer
}

// Server returns the entry for a server identity.
func (p *StructuredPolicy) Server(identity string) (PolicyServer, bool) {
	for _, s := range p.Servers {
		if s.Server == identity {
			return s, true
		}
	}
	return PolicyServer{}, false
}

// StructuredPolicySource is a policy source backed by a structured
// policy file. Policy returns nil, with no error, when there is no file.
type StructuredPolicySource interface {
	PolicySource
	Policy() (*StructuredPolicy, error)
}

// ActivePolicy returns the structured policy in force: the first source
// in the chain, highest priority first, whose file is present. Nil means
// none is active and the flat config.toml allowlist applies. A source
// whose file is present but invalid is an error, never a skip, so a
// broken higher-priority policy cannot hand control to a more
// permissive lower one.
func (r *Resolver) ActivePolicy() (*StructuredPolicy, error) {
	for _, s := range r.sources {
		sp, ok := s.(StructuredPolicySource)
		if !ok {
			continue
		}
		p, err := sp.Policy()
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	return nil, nil
}

// policyFile reads one structured policy file. The file is read on
// every call, like the local config files, so a newly written policy
// applies to the next resolution. An absent file means no policy from
// this source. A file that exists but cannot be read or understood
// fails closed: every Resolve returns an error, so no lower, more
// permissive source can answer in its place.
type policyFile struct {
	name  string // the Name() of the source, for provenance
	label string // how errors refer to the file, e.g. "enterprise policy"
	path  string
}

func (f policyFile) Name() string { return f.name }

// Writable is false: these files belong to someone else (the enterprise
// tier, or the user editing by hand), never to centrol's own writes.
func (f policyFile) Writable() bool { return false }

func (f policyFile) Policy() (*StructuredPolicy, error) {
	if f.path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s cannot be read, so no policy will resolve until it can: %w", f.label, f.path, err)
	}
	servers, err := parsePolicyServers(data)
	if err != nil {
		return nil, fmt.Errorf("%s %s is invalid, so no policy will resolve until it is fixed: %w", f.label, f.path, err)
	}
	return &StructuredPolicy{Source: f.name, Path: f.path, Servers: servers}, nil
}

// Resolve answers proxy.allowed_servers with the policy's server names.
// Every other key is not found, but a policy that fails to load is an
// error for every key, not only that one.
func (f policyFile) Resolve(key string) (interface{}, bool, error) {
	p, err := f.Policy()
	if err != nil || p == nil || key != KeyAllowedServers {
		return nil, false, err
	}
	names := make([]string, len(p.Servers))
	for i, s := range p.Servers {
		names[i] = s.Server
	}
	return names, true, nil
}

// parsePolicyServers is strict because the file is a restriction: a
// field this version does not know is a rule it cannot honor, and
// ignoring it would silently loosen the policy.
func parsePolicyServers(data []byte) ([]PolicyServer, error) {
	var doc struct {
		Servers []PolicyServer `json:"servers"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected content after the policy object")
	}
	if doc.Servers == nil {
		return nil, fmt.Errorf(`"servers" is required (use an empty list to allow no servers)`)
	}
	seen := make(map[string]bool, len(doc.Servers))
	for i, s := range doc.Servers {
		if strings.TrimSpace(s.Server) == "" {
			return nil, fmt.Errorf(`servers[%d]: "server" must not be empty`, i)
		}
		if seen[s.Server] {
			return nil, fmt.Errorf("servers[%d]: %q is listed more than once", i, s.Server)
		}
		seen[s.Server] = true
	}
	return doc.Servers, nil
}

// centrolHomeFile returns ~/.centrol/<name>, or "" when the home
// directory cannot be resolved. An unresolvable HOME behaves as if no
// file is present: a missing home is trusted rather than treated as a
// fault, so standalone users in minimal containers keep working. The
// consequence is that anyone who controls this process's HOME controls
// which policy files it sees.
func centrolHomeFile(name string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".centrol", name)
}

// userConfigFile is the user-level config.toml, beside the policy files.
const userConfigFile = "config.toml"

// UserConfigPath is ~/.centrol/config.toml, or "" when the home
// directory cannot be resolved.
func UserConfigPath() string { return centrolHomeFile(userConfigFile) }

// TrustedHomeFiles are the files under ~/.centrol that decide what the
// agent may do: the enterprise policy, the user's own allowlist and the
// user config. They are the contents of the default protected set (an
// MCP call to one is refused) and what the guard watches (a direct
// write is recorded). Nothing here is user-tunable. It is empty when
// the home directory cannot be resolved.
func TrustedHomeFiles() []string {
	var files []string
	for _, name := range []string{enterprisePolicyFile, localAllowlistFile, userConfigFile} {
		if path := centrolHomeFile(name); path != "" {
			files = append(files, path)
		}
	}
	return files
}
