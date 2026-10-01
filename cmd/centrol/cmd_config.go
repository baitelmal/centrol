package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/scirem/centrol/internal/policy"
	"github.com/scirem/centrol/internal/ui"
)

// cmdConfig implements `centrol config`: a line-based (no TUI) menu for
// editing every key this project made configurable. Every write goes
// through the same Resolver.WriteConfig path a `centrol proxy`
// pre-flight "Add to allowlist" choice uses, so provenance (repo_config
// vs user_config) is identical either way.
func cmdConfig(args []string) {
	root, err := repoRoot()
	if err != nil {
		if cwd, cerr := os.Getwd(); cerr == nil {
			root = cwd
		} else {
			fatalf("centrol config: %v", err)
		}
	}
	runConfigMenu(bufio.NewReader(os.Stdin), os.Stdout, newResolver(root))
}

// runConfigMenu is cmdConfig's actual logic, separated out so tests can
// drive it against an arbitrary in/out and — critically — an arbitrary
// *policy.Resolver, including one whose EnterpriseSource is swapped for
// a synthetic test double that returns a real value. That is the only
// way to exercise the "locked by enterprise" display path at all in
// v0.1: the real EnterpriseSource always returns found=false, by
// design (see internal/policy.EnterpriseSource).
func runConfigMenu(in *bufio.Reader, out io.Writer, resolver *policy.Resolver) {
	for {
		fmt.Fprint(out, `
Centrol config

1) MCP Server Allowlist
2) Gate (prompt timeout, allow-session)
3) Proxy (block paths, call timeout, strict matching)
4) Guard (watcher debounce)
5) General (log level, observe mode)
6) Mass-mutation threshold
7) View effective config
8) Save and exit

> `)
		choice := readMenuLine(in)
		switch choice {
		case "1":
			configAllowlistMenu(in, out, resolver)
		case "2":
			configGateMenu(in, out, resolver)
		case "3":
			configProxyMenu(in, out, resolver)
		case "4":
			configGuardMenu(in, out, resolver)
		case "5":
			configGeneralMenu(in, out, resolver)
		case "6":
			configThresholdsMenu(in, out, resolver)
		case "7":
			configViewEffective(out, resolver)
		case "8", "save", "q", "quit", "":
			// Every edit in every section below already persisted
			// immediately via resolver.WriteConfig at the moment it was
			// made — there is nothing left to flush here. "Save and
			// exit" and "Quit" are the same action for that reason;
			// both names are offered because the spec calls for the
			// former and habit reaches for the latter.
			return
		default:
			fmt.Fprintln(out, "Not a recognized option.")
		}
	}
}

func readMenuLine(in *bufio.Reader) string {
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// lockedByEnterprise reports whether a resolved value's source is the
// enterprise policy source — the one case, per spec, where the menu
// must display the value but refuse to offer editing it. Every
// Resolve* helper already returns this same source string from the
// standard chain, so a key that an EnterpriseSource actually answers
// (only possible in tests in v0.1 — see runConfigMenu's doc comment)
// is indistinguishable from any other resolved value except by source.
func lockedByEnterprise(source string) bool {
	return source == policy.SourceEnterprisePolicy
}

// printLockable renders one "key = value (source)" menu line, appending
// "[locked by enterprise]" instead of an edit hint when the value came
// from the enterprise source.
func printLockable(out io.Writer, label string, value interface{}, source string) {
	if lockedByEnterprise(source) {
		fmt.Fprintf(out, "  %-28s %-12v [locked by enterprise]\n", label, value)
		return
	}
	fmt.Fprintf(out, "  %-28s %-12v [%s]\n", label, value, source)
}

func configAllowlistMenu(in *bufio.Reader, out io.Writer, resolver *policy.Resolver) {
	for {
		editable, source, err := policy.ResolveAllowedServers(resolver)
		if err != nil {
			fmt.Fprintf(out, "error reading allowlist: %v\n", err)
			return
		}
		locked := lockedByEnterprise(source)

		fmt.Fprintln(out, "\nMCP Server Allowlist")
		for _, s := range editable {
			if locked {
				fmt.Fprintf(out, "  %s %-45s [locked by enterprise]\n", ui.Glyph(out, ui.GlyphOK), s)
			} else {
				fmt.Fprintf(out, "  %s %-45s [editable]\n", ui.Glyph(out, ui.GlyphOK), s)
			}
		}
		if !locked {
			fmt.Fprintln(out, "  + Add server...")
		}
		fmt.Fprintln(out, "  b) Back")
		fmt.Fprint(out, "> ")

		choice := readMenuLine(in)
		switch choice {
		case "b", "back", "":
			return
		case "+", "add", "a":
			if locked {
				fmt.Fprintln(out, "the allowlist is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprint(out, "Package or binary name to allow: ")
			name := readMenuLine(in)
			if name == "" {
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyAllowedServers, append(editable, name))
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Added %q to the allowlist (%s). Effective on the next `centrol proxy` run.\n", name, src)
		default:
			fmt.Fprintln(out, "Not a recognized option.")
		}
	}
}

func configGateMenu(in *bufio.Reader, out io.Writer, resolver *policy.Resolver) {
	for {
		timeout, timeoutSrc, err := policy.ResolvePromptTimeoutSeconds(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		allowSession, allowSessionSrc, err := policy.ResolveAllowSessionAmend(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}

		fmt.Fprintln(out, "\nGate")
		printLockable(out, "prompt_timeout_seconds", timeout, timeoutSrc)
		printLockable(out, "allow_session_amend", allowSession, allowSessionSrc)
		fmt.Fprintln(out, "  1) Edit prompt_timeout_seconds")
		fmt.Fprintln(out, "  2) Toggle allow_session_amend")
		fmt.Fprintln(out, "  b) Back")
		fmt.Fprint(out, "> ")

		choice := readMenuLine(in)
		switch choice {
		case "b", "back", "":
			return
		case "1":
			if lockedByEnterprise(timeoutSrc) {
				fmt.Fprintln(out, "prompt_timeout_seconds is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprintf(out, "New value (seconds, floor %d): ", policy.MinPromptTimeoutSeconds)
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if v < policy.MinPromptTimeoutSeconds {
				fmt.Fprintf(out, "error: prompt_timeout_seconds = %d is below the minimum of %d\n", v, policy.MinPromptTimeoutSeconds)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyPromptTimeoutSeconds, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set prompt_timeout_seconds = %d (%s). Effective on the next `centrol proxy` run.\n", v, src)
		case "2":
			if lockedByEnterprise(allowSessionSrc) {
				fmt.Fprintln(out, "allow_session_amend is locked by enterprise policy and cannot be edited here")
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyAllowSessionAmend, !allowSession)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set allow_session_amend = %v (%s).\n", !allowSession, src)
		default:
			fmt.Fprintln(out, "Not a recognized option.")
		}
	}
}

// validTargetURLScheme reports whether v is acceptable as
// proxy.target_url: either empty (unsetting the value — HTTP mode
// turned off) or a URL whose scheme is exactly "http" or "https", the
// only two schemes internal/proxy/transport.HTTPTarget ever uses.
// Audit fix (3a): this used to be written to config.toml unvalidated,
// so a typo'd scheme, a bare host with none at all, or something like
// file:// or javascript: would be silently accepted here and only
// fail (or not) much later, inside the HTTP transport.
func validTargetURLScheme(v string) bool {
	if v == "" {
		return true
	}
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func configProxyMenu(in *bufio.Reader, out io.Writer, resolver *policy.Resolver) {
	for {
		blockPaths, blockPathsSrc, err := policy.ResolveAdditionalBlockPaths(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		callTimeout, callTimeoutSrc, err := policy.ResolveMCPCallTimeoutSeconds(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		strict, strictSrc, err := policy.ResolveStrictMatching(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		targetURL, targetURLSrc, err := policy.ResolveProxyTargetURL(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		httpTimeout, httpTimeoutSrc, err := policy.ResolveProxyHTTPTimeoutSeconds(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		httpMaxRetries, httpMaxRetriesSrc, err := policy.ResolveProxyHTTPMaxRetries(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		targetExitTimeout, targetExitTimeoutSrc, err := policy.ResolveTargetExitTimeoutSeconds(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}

		fmt.Fprintln(out, "\nProxy")
		printLockable(out, "additional_block_paths", fmt.Sprintf("%v", blockPaths), blockPathsSrc)
		printLockable(out, "mcp_call_timeout_seconds", callTimeout, callTimeoutSrc)
		printLockable(out, "strict_matching", strict, strictSrc)
		printLockable(out, "target_url", targetURL, targetURLSrc)
		printLockable(out, "http_timeout_seconds", httpTimeout, httpTimeoutSrc)
		printLockable(out, "http_max_retries", httpMaxRetries, httpMaxRetriesSrc)
		printLockable(out, "target_exit_timeout_seconds", targetExitTimeout, targetExitTimeoutSrc)
		fmt.Fprintln(out, "  1) Add a path to additional_block_paths")
		fmt.Fprintln(out, "  2) Edit mcp_call_timeout_seconds")
		fmt.Fprintln(out, "  3) Toggle strict_matching")
		fmt.Fprintln(out, "  4) Edit target_url")
		fmt.Fprintln(out, "  5) Edit http_timeout_seconds")
		fmt.Fprintln(out, "  6) Edit http_max_retries")
		fmt.Fprintln(out, "  7) Edit target_exit_timeout_seconds")
		fmt.Fprintln(out, "  b) Back")
		fmt.Fprint(out, "> ")

		choice := readMenuLine(in)
		switch choice {
		case "b", "back", "":
			return
		case "1":
			if lockedByEnterprise(blockPathsSrc) {
				fmt.Fprintln(out, "additional_block_paths is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprint(out, "Path to block (absolute or ~-relative): ")
			p := readMenuLine(in)
			if p == "" {
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyAdditionalBlockPaths, append(blockPaths, p))
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Added %q to additional_block_paths (%s). Effective on the next `centrol proxy` run.\n", p, src)
		case "2":
			if lockedByEnterprise(callTimeoutSrc) {
				fmt.Fprintln(out, "mcp_call_timeout_seconds is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprintf(out, "New value (seconds, floor %d): ", policy.MinMCPCallTimeoutSeconds)
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if v < policy.MinMCPCallTimeoutSeconds {
				fmt.Fprintf(out, "error: mcp_call_timeout_seconds = %d is below the minimum of %d\n", v, policy.MinMCPCallTimeoutSeconds)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyMCPCallTimeoutSeconds, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set mcp_call_timeout_seconds = %d (%s). Effective on the next `centrol proxy` run.\n", v, src)
		case "3":
			if lockedByEnterprise(strictSrc) {
				fmt.Fprintln(out, "strict_matching is locked by enterprise policy and cannot be edited here")
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyStrictMatching, !strict)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set strict_matching = %v (%s).\n", !strict, src)
		case "4":
			if lockedByEnterprise(targetURLSrc) {
				fmt.Fprintln(out, "target_url is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprint(out, "New value (HTTP target URL, empty to unset): ")
			v := readMenuLine(in)
			if !validTargetURLScheme(v) {
				fmt.Fprintf(out, "error: target_url must be an http:// or https:// URL (or empty to unset), got %q\n", v)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyProxyTargetURL, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set target_url = %q (%s). Effective on the next `centrol proxy` run.\n", v, src)
		case "5":
			if lockedByEnterprise(httpTimeoutSrc) {
				fmt.Fprintln(out, "http_timeout_seconds is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprintf(out, "New value (seconds, floor %d): ", policy.MinHTTPTimeoutSeconds)
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if v < policy.MinHTTPTimeoutSeconds {
				fmt.Fprintf(out, "error: http_timeout_seconds = %d is below the minimum of %d\n", v, policy.MinHTTPTimeoutSeconds)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyProxyHTTPTimeoutSecs, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set http_timeout_seconds = %d (%s). Effective on the next `centrol proxy` run.\n", v, src)
		case "6":
			if lockedByEnterprise(httpMaxRetriesSrc) {
				fmt.Fprintln(out, "http_max_retries is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprintf(out, "New value (floor %d): ", policy.MinHTTPMaxRetries)
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if v < policy.MinHTTPMaxRetries {
				fmt.Fprintf(out, "error: http_max_retries = %d is below the minimum of %d\n", v, policy.MinHTTPMaxRetries)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyProxyHTTPMaxRetries, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set http_max_retries = %d (%s). Effective on the next `centrol proxy` run.\n", v, src)
		case "7":
			if lockedByEnterprise(targetExitTimeoutSrc) {
				fmt.Fprintln(out, "target_exit_timeout_seconds is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprintf(out, "New value (seconds, floor %d): ", policy.MinTargetExitTimeoutSecs)
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if v < policy.MinTargetExitTimeoutSecs {
				fmt.Fprintf(out, "error: target_exit_timeout_seconds = %d is below the minimum of %d\n", v, policy.MinTargetExitTimeoutSecs)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyTargetExitTimeoutSecs, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set target_exit_timeout_seconds = %d (%s). Effective on the next `centrol proxy` run.\n", v, src)
		default:
			fmt.Fprintln(out, "Not a recognized option.")
		}
	}
}

func configGuardMenu(in *bufio.Reader, out io.Writer, resolver *policy.Resolver) {
	for {
		debounce, debounceSrc, err := policy.ResolveWatcherDebounceMS(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		gitTimeout, gitTimeoutSrc, err := policy.ResolveGitTimeoutSeconds(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}

		fmt.Fprintln(out, "\nGuard")
		printLockable(out, "watcher_debounce_ms", debounce, debounceSrc)
		printLockable(out, "git_timeout_seconds", gitTimeout, gitTimeoutSrc)
		fmt.Fprintln(out, "  1) Edit watcher_debounce_ms")
		fmt.Fprintln(out, "  2) Edit git_timeout_seconds")
		fmt.Fprintln(out, "  b) Back")
		fmt.Fprint(out, "> ")

		choice := readMenuLine(in)
		switch choice {
		case "b", "back", "":
			return
		case "1":
			if lockedByEnterprise(debounceSrc) {
				fmt.Fprintln(out, "watcher_debounce_ms is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprint(out, "New value (milliseconds, >= 0): ")
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if v < 0 {
				fmt.Fprintln(out, "error: watcher_debounce_ms cannot be negative")
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyWatcherDebounceMS, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set watcher_debounce_ms = %d (%s). Effective on the next `centrol guard` run.\n", v, src)
		case "2":
			if lockedByEnterprise(gitTimeoutSrc) {
				fmt.Fprintln(out, "git_timeout_seconds is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprintf(out, "New value (seconds, >= %d): ", policy.MinGitTimeoutSeconds)
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if v < policy.MinGitTimeoutSeconds {
				fmt.Fprintf(out, "error: git_timeout_seconds cannot be below %d\n", policy.MinGitTimeoutSeconds)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyGitTimeoutSeconds, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set git_timeout_seconds = %d (%s). Effective on the next `centrol guard`/`centrol undo` run.\n", v, src)
		default:
			fmt.Fprintln(out, "Not a recognized option.")
		}
	}
}

func configGeneralMenu(in *bufio.Reader, out io.Writer, resolver *policy.Resolver) {
	for {
		level, levelSrc, err := policy.ResolveLogLevel(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		observe, observeSrc, err := policy.ResolveObserveMode(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}

		fmt.Fprintln(out, "\nGeneral")
		printLockable(out, "log_level", level, levelSrc)
		printLockable(out, "observe_mode", observe, observeSrc)
		fmt.Fprintln(out, "  1) Set log_level (info | warn | debug)")
		fmt.Fprintln(out, "  2) Toggle observe_mode")
		fmt.Fprintln(out, "  b) Back")
		fmt.Fprint(out, "> ")

		choice := readMenuLine(in)
		switch choice {
		case "b", "back", "":
			return
		case "1":
			if lockedByEnterprise(levelSrc) {
				fmt.Fprintln(out, "log_level is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprint(out, "New value (info | warn | debug): ")
			raw := readMenuLine(in)
			if _, ok := ui.ParseLevel(raw); !ok {
				fmt.Fprintf(out, "error: %q must be one of info, warn, debug\n", raw)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyLogLevel, raw)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set log_level = %s (%s).\n", raw, src)
		case "2":
			if lockedByEnterprise(observeSrc) {
				fmt.Fprintln(out, "observe_mode is locked by enterprise policy and cannot be edited here")
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyObserveMode, !observe)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set observe_mode = %v (%s).\n", !observe, src)
		default:
			fmt.Fprintln(out, "Not a recognized option.")
		}
	}
}

func configThresholdsMenu(in *bufio.Reader, out io.Writer, resolver *policy.Resolver) {
	for {
		n, source, err := policy.ResolveMassMutationThreshold(resolver)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			return
		}
		fmt.Fprintln(out, "\nThresholds")
		if lockedByEnterprise(source) {
			fmt.Fprintf(out, "  mass_mutation_threshold = %d  [locked by enterprise]\n", n)
		} else {
			fmt.Fprintf(out, "  mass_mutation_threshold = %d  (from %s; typical range 20-100, hard range %d-%d)\n",
				n, source, policy.MinMassMutationThreshold, policy.MaxMassMutationThreshold)
		}
		fmt.Fprintln(out, "  e) Edit mass_mutation_threshold")
		fmt.Fprintln(out, "  b) Back")
		fmt.Fprint(out, "> ")

		choice := readMenuLine(in)
		switch choice {
		case "b", "back", "":
			return
		case "e", "edit":
			if lockedByEnterprise(source) {
				fmt.Fprintln(out, "mass_mutation_threshold is locked by enterprise policy and cannot be edited here")
				continue
			}
			fmt.Fprint(out, "New value: ")
			raw := readMenuLine(in)
			v, err := strconv.Atoi(raw)
			if err != nil {
				fmt.Fprintf(out, "error: %q is not an integer\n", raw)
				continue
			}
			if err := policy.ValidateMassMutationThreshold(v); err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			src, err := resolver.WriteConfig(policy.KeyMassMutationThreshold, v)
			if err != nil {
				fmt.Fprintf(out, "error: %v\n", err)
				continue
			}
			fmt.Fprintf(out, "Set mass_mutation_threshold = %d (%s). Effective on the next `centrol proxy` run.\n", v, src)
		default:
			fmt.Fprintln(out, "Not a recognized option.")
		}
	}
}

// configViewEffective implements section 7, "View effective config":
// every configurable key, its currently resolved value, and which
// source in the priority chain answered it — the whole point being
// that a user staring at unexpected behavior can see AT A GLANCE
// whether it's their repo config, their user config, an enterprise
// policy, or just the built-in default.
func configViewEffective(out io.Writer, resolver *policy.Resolver) {
	fmt.Fprintln(out, "\nEffective config (resolution order: session > enterprise > repo > user > default)")

	printRow := func(label string, value interface{}, source string) {
		if lockedByEnterprise(source) {
			fmt.Fprintf(out, "  %-28s %-20v [locked by enterprise]\n", label, value)
			return
		}
		fmt.Fprintf(out, "  %-28s %-20v [%s]\n", label, value, source)
	}

	if v, s, err := policy.ResolveMassMutationThreshold(resolver); err == nil {
		printRow("proxy.mass_mutation_threshold", v, s)
	}
	if v, s, err := policy.ResolveAllowedServers(resolver); err == nil {
		printRow("proxy.allowed_servers", v, s)
	}
	if v, s, err := policy.ResolveStrictMatching(resolver); err == nil {
		printRow("proxy.strict_matching", v, s)
	}
	if v, s, err := policy.ResolveAdditionalBlockPaths(resolver); err == nil {
		printRow("proxy.additional_block_paths", v, s)
	}
	if v, s, err := policy.ResolveMCPCallTimeoutSeconds(resolver); err == nil {
		printRow("proxy.mcp_call_timeout_seconds", v, s)
	}
	if v, s, err := policy.ResolveProxyTargetURL(resolver); err == nil {
		printRow("proxy.target_url", v, s)
	}
	if v, s, err := policy.ResolveProxyHTTPTimeoutSeconds(resolver); err == nil {
		printRow("proxy.http_timeout_seconds", v, s)
	}
	if v, s, err := policy.ResolveProxyHTTPMaxRetries(resolver); err == nil {
		printRow("proxy.http_max_retries", v, s)
	}
	if v, s, err := policy.ResolveTargetExitTimeoutSeconds(resolver); err == nil {
		printRow("proxy.target_exit_timeout_seconds", v, s)
	}
	if v, s, err := policy.ResolvePromptTimeoutSeconds(resolver); err == nil {
		printRow("gate.prompt_timeout_seconds", v, s)
	}
	if v, s, err := policy.ResolveAllowSessionAmend(resolver); err == nil {
		printRow("gate.allow_session_amend", v, s)
	}
	if v, s, err := policy.ResolveWatcherDebounceMS(resolver); err == nil {
		printRow("guard.watcher_debounce_ms", v, s)
	}
	if v, s, err := policy.ResolveGitTimeoutSeconds(resolver); err == nil {
		printRow("guard.git_timeout_seconds", v, s)
	}
	if v, s, err := policy.ResolveLogLevel(resolver); err == nil {
		printRow("general.log_level", v, s)
	}
	if v, s, err := policy.ResolveObserveMode(resolver); err == nil {
		printRow("general.observe_mode", v, s)
	}
}
