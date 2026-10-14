package policy

// localAllowlistFile is the user's own structured allowlist, the same
// schema as the enterprise policy file.
const localAllowlistFile = "allowlist.json"

// LocalAllowlistSource answers policy keys from ~/.centrol/allowlist.json,
// which the user writes by hand. It ranks below the enterprise policy
// and above the repo config: when present it is the allowlist, and the
// flat config.toml list is shadowed. Unlike the enterprise policy its
// values are not locked, because the user owns the file; centrol itself
// never writes it.
type LocalAllowlistSource struct{ policyFile }

// NewLocalAllowlistSource reads ~/.centrol/allowlist.json.
func NewLocalAllowlistSource() *LocalAllowlistSource {
	return NewLocalAllowlistSourceAt(centrolHomeFile(localAllowlistFile))
}

// NewLocalAllowlistSourceAt reads the allowlist from path instead of
// the default location. An empty path never finds a file.
func NewLocalAllowlistSourceAt(path string) *LocalAllowlistSource {
	return &LocalAllowlistSource{policyFile{name: SourceLocalAllowlist, label: "local allowlist", path: path}}
}
