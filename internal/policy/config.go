package policy

import "fmt"

// Config keys, in "section.key" form as the Resolver expects.
const (
	KeyMassMutationThreshold = "proxy.mass_mutation_threshold"
	KeyAllowedServers        = "proxy.allowed_servers"
	KeyStrictMatching        = "proxy.strict_matching"
	KeyPromptTimeoutSeconds  = "gate.prompt_timeout_seconds"
	KeyAllowSessionAmend     = "gate.allow_session_amend"
	KeyAdditionalBlockPaths  = "proxy.additional_block_paths"
	KeyMCPCallTimeoutSeconds = "proxy.mcp_call_timeout_seconds"
	KeyWatcherDebounceMS     = "guard.watcher_debounce_ms"
	KeyLogLevel              = "general.log_level"
	KeyObserveMode           = "general.observe_mode"
)

const (
	DefaultPromptTimeoutSeconds  = 300
	MinPromptTimeoutSeconds      = 30
	DefaultMCPCallTimeoutSeconds = 60
	MinMCPCallTimeoutSeconds     = 5
	DefaultWatcherDebounceMS     = 100
)

// ResolvePromptTimeoutSeconds resolves gate.prompt_timeout_seconds,
// defaulting to DefaultPromptTimeoutSeconds and enforcing the hard
// floor of MinPromptTimeoutSeconds (failing a flag prompt closed after
// a too-short timeout would be indistinguishable from a config typo
// silently denying everything).
func ResolvePromptTimeoutSeconds(r *Resolver) (seconds int, source string, err error) {
	raw, src, found, err := r.Resolve(KeyPromptTimeoutSeconds)
	if err != nil {
		return 0, "", err
	}
	if !found {
		return DefaultPromptTimeoutSeconds, SourceDefault, nil
	}
	n, ok := raw.(int)
	if !ok {
		return 0, "", fmt.Errorf("%s must be an integer, got %v (from %s)", KeyPromptTimeoutSeconds, raw, src)
	}
	if n < MinPromptTimeoutSeconds {
		return 0, "", fmt.Errorf("%s = %d is below the minimum of %d (from %s)", KeyPromptTimeoutSeconds, n, MinPromptTimeoutSeconds, src)
	}
	return n, src, nil
}

// ResolveAllowSessionAmend resolves gate.allow_session_amend, defaulting
// to true. When false, the [Allow session] option must be removed from
// flag prompts — every flagged action prompts individually. This is the
// enterprise-strict setting and cannot be overridden mid-run.
func ResolveAllowSessionAmend(r *Resolver) (allow bool, source string, err error) {
	raw, src, found, err := r.Resolve(KeyAllowSessionAmend)
	if err != nil {
		return false, "", err
	}
	if !found {
		return true, SourceDefault, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, "", fmt.Errorf("%s must be true or false, got %v (from %s)", KeyAllowSessionAmend, raw, src)
	}
	return b, src, nil
}

// ResolveAdditionalBlockPaths resolves proxy.additional_block_paths —
// paths ADDED to the built-in block list, never a replacement for it.
func ResolveAdditionalBlockPaths(r *Resolver) (paths []string, source string, err error) {
	raw, src, found, err := r.Resolve(KeyAdditionalBlockPaths)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, SourceDefault, nil
	}
	list, ok := raw.([]string)
	if !ok {
		return nil, "", fmt.Errorf("%s must be an array of strings, got %v (from %s)", KeyAdditionalBlockPaths, raw, src)
	}
	return list, src, nil
}

// ResolveMCPCallTimeoutSeconds resolves proxy.mcp_call_timeout_seconds,
// defaulting to DefaultMCPCallTimeoutSeconds and enforcing the hard
// floor MinMCPCallTimeoutSeconds.
func ResolveMCPCallTimeoutSeconds(r *Resolver) (seconds int, source string, err error) {
	raw, src, found, err := r.Resolve(KeyMCPCallTimeoutSeconds)
	if err != nil {
		return 0, "", err
	}
	if !found {
		return DefaultMCPCallTimeoutSeconds, SourceDefault, nil
	}
	n, ok := raw.(int)
	if !ok {
		return 0, "", fmt.Errorf("%s must be an integer, got %v (from %s)", KeyMCPCallTimeoutSeconds, raw, src)
	}
	if n < MinMCPCallTimeoutSeconds {
		return 0, "", fmt.Errorf("%s = %d is below the minimum of %d (from %s)", KeyMCPCallTimeoutSeconds, n, MinMCPCallTimeoutSeconds, src)
	}
	return n, src, nil
}

// ResolveWatcherDebounceMS resolves guard.watcher_debounce_ms,
// defaulting to DefaultWatcherDebounceMS.
func ResolveWatcherDebounceMS(r *Resolver) (ms int, source string, err error) {
	raw, src, found, err := r.Resolve(KeyWatcherDebounceMS)
	if err != nil {
		return 0, "", err
	}
	if !found {
		return DefaultWatcherDebounceMS, SourceDefault, nil
	}
	n, ok := raw.(int)
	if !ok {
		return 0, "", fmt.Errorf("%s must be an integer, got %v (from %s)", KeyWatcherDebounceMS, raw, src)
	}
	if n < 0 {
		return 0, "", fmt.Errorf("%s = %d cannot be negative (from %s)", KeyWatcherDebounceMS, n, src)
	}
	return n, src, nil
}

// ResolveLogLevel resolves general.log_level, defaulting to "info", and
// rejects anything other than info/warn/debug.
func ResolveLogLevel(r *Resolver) (level, source string, err error) {
	raw, src, found, err := r.Resolve(KeyLogLevel)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "info", SourceDefault, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", "", fmt.Errorf("%s must be a string, got %v (from %s)", KeyLogLevel, raw, src)
	}
	switch s {
	case "info", "warn", "debug":
		return s, src, nil
	default:
		return "", "", fmt.Errorf("%s must be one of info, warn, debug — got %q (from %s)", KeyLogLevel, s, src)
	}
}

// ResolveObserveMode resolves general.observe_mode, defaulting to
// false. A true value here has the same effect as the --observe flag
// but applies even if the flag is omitted.
func ResolveObserveMode(r *Resolver) (observe bool, source string, err error) {
	raw, src, found, err := r.Resolve(KeyObserveMode)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, SourceDefault, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, "", fmt.Errorf("%s must be true or false, got %v (from %s)", KeyObserveMode, raw, src)
	}
	return b, src, nil
}

// Mass-mutation threshold bounds. The default is a sane starting point;
// the floor and ceiling are sanity limits on user configuration, not
// recommendations — the documented typical range (20-100) lives in the
// README, not enforced here as anything more than "somewhere inside
// [MinMassMutationThreshold, MaxMassMutationThreshold]".
const (
	DefaultMassMutationThreshold = 20
	MinMassMutationThreshold     = 10
	MaxMassMutationThreshold     = 1000
)

// ValidateMassMutationThreshold rejects a configured value outside the
// hard floor/ceiling with a clear, specific error — not a generic
// "invalid config" message.
func ValidateMassMutationThreshold(n int) error {
	if n < MinMassMutationThreshold {
		return fmt.Errorf("%s = %d is below the minimum of %d (typical range: 20-100)", KeyMassMutationThreshold, n, MinMassMutationThreshold)
	}
	if n > MaxMassMutationThreshold {
		return fmt.Errorf("%s = %d is above the maximum of %d (typical range: 20-100)", KeyMassMutationThreshold, n, MaxMassMutationThreshold)
	}
	return nil
}

// ResolveMassMutationThreshold resolves proxy.mass_mutation_threshold
// through the chain, defaulting to DefaultMassMutationThreshold if no
// source sets it, and validates whatever value was found (from any
// source — including the default, so a corrupted built-in default would
// also be caught, though in practice it never will be). A value found
// but out of range is a configuration error the caller should surface
// to the user, not silently clamp.
func ResolveMassMutationThreshold(r *Resolver) (value int, source string, err error) {
	raw, src, found, err := r.Resolve(KeyMassMutationThreshold)
	if err != nil {
		return 0, "", err
	}
	if !found {
		return DefaultMassMutationThreshold, SourceDefault, nil
	}
	n, ok := raw.(int)
	if !ok {
		return 0, "", fmt.Errorf("%s must be an integer, got %v (from %s)", KeyMassMutationThreshold, raw, src)
	}
	if err := ValidateMassMutationThreshold(n); err != nil {
		return 0, "", fmt.Errorf("%w (from %s)", err, src)
	}
	return n, src, nil
}

// ResolveAllowedServers resolves proxy.allowed_servers through the
// chain. An empty/not-found result is not an error — it just means no
// server has been allowlisted yet, so every server is "unknown" until
// allowed once (see EvaluateServerAllowlist).
func ResolveAllowedServers(r *Resolver) (servers []string, source string, err error) {
	raw, src, found, err := r.Resolve(KeyAllowedServers)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, SourceDefault, nil
	}
	list, ok := raw.([]string)
	if !ok {
		return nil, "", fmt.Errorf("%s must be an array of strings, got %v (from %s)", KeyAllowedServers, raw, src)
	}
	return list, src, nil
}

// ResolveStrictMatching resolves proxy.strict_matching, defaulting to
// false (package/binary-name matching, not full-command-string
// matching) when unset.
func ResolveStrictMatching(r *Resolver) (strict bool, source string, err error) {
	raw, src, found, err := r.Resolve(KeyStrictMatching)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, SourceDefault, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, "", fmt.Errorf("%s must be true or false, got %v (from %s)", KeyStrictMatching, raw, src)
	}
	return b, src, nil
}
