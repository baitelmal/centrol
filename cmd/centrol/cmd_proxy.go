package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/scirem/centrol/internal/policy"
	"github.com/scirem/centrol/internal/proxy"
	"github.com/scirem/centrol/internal/proxy/transport"
	"github.com/scirem/centrol/internal/session"
	"github.com/scirem/centrol/internal/ui"
)

// cmdProxy dispatches `centrol proxy --target <mcp server>` and
// `centrol proxy install <client>`.
func cmdProxy(args []string) {
	if len(args) > 0 && args[0] == "install" {
		cmdProxyInstall(args[1:])
		return
	}
	cmdProxyRun(args)
}

func cmdProxyRun(args []string) {
	target := ""
	targetGiven := false
	targetURL := ""
	targetURLGiven := false
	observeFlag := false
	quietFlag := false
	verboseFlag := false
	for i, a := range args {
		switch {
		case a == "--target" && i+1 < len(args):
			target = args[i+1]
			targetGiven = true
		case strings.HasPrefix(a, "--target="):
			target = strings.TrimPrefix(a, "--target=")
			targetGiven = true
		case a == "--target-url" && i+1 < len(args):
			targetURL = args[i+1]
			targetURLGiven = true
		case strings.HasPrefix(a, "--target-url="):
			targetURL = strings.TrimPrefix(a, "--target-url=")
			targetURLGiven = true
		case a == "--observe":
			observeFlag = true
		case a == "--quiet":
			quietFlag = true
		case a == "--verbose":
			verboseFlag = true
		}
	}
	if targetGiven && targetURLGiven {
		fatalf("centrol proxy: --target and --target-url are mutually exclusive — pick one transport")
	}

	// repoRoot is best-effort for proxy: an MCP server doesn't have to
	// front a git repo the way guard's rollback does, so a missing repo
	// is not fatal — the contract just governs relative to cwd instead.
	root, err := repoRoot()
	if err != nil {
		if cwd, cerr := os.Getwd(); cerr == nil {
			root = cwd
		}
		fmt.Fprintf(os.Stderr, "centrol proxy: warning: not inside a git repo; scoping the contract to %s\n", root)
	}
	resolver := newResolver(root)

	// Which transport to use: an explicit --target always means stdio
	// and wins outright, ignoring any configured proxy.target_url
	// entirely — that's the "--target overrides config target_url if
	// both are set" rule, a precedence rule between a CLI flag and a
	// config value, distinct from the mutual-exclusivity check above
	// (which is only between the two CLI flags). Otherwise,
	// --target-url, if given, or else the configured proxy.target_url,
	// selects an HTTP target. Neither present at all is a usage error.
	useHTTP := false
	if !targetGiven {
		if targetURLGiven {
			useHTTP = true
		} else {
			configURL, _, cerr := policy.ResolveProxyTargetURL(resolver)
			if cerr != nil {
				fatalf("centrol proxy: %v", cerr)
			}
			if configURL != "" {
				targetURL = configURL
				useHTTP = true
			}
		}
	}
	if !targetGiven && !useHTTP {
		fatalf("centrol proxy: usage: centrol proxy --target \"<mcp server command>\" or centrol proxy --target-url \"<http url>\" (or set [proxy] target_url in config)")
	}

	var fields []string
	if !useHTTP {
		// v0.1 argument splitting is plain whitespace — no quoting
		// support yet. Good enough for the common `npx @pkg/name` case;
		// a target command needing quoted arguments should be wrapped
		// in a small shell script and pointed at that instead.
		fields = strings.Fields(target)
		if len(fields) == 0 {
			fatalf("centrol proxy: empty --target command")
		}
	}
	targetDesc := target
	if useHTTP {
		targetDesc = targetURL
	}

	g, err := openGovernorAt(root)
	if err != nil {
		fatalf("centrol proxy: %v", err)
	}
	emit := emitFunc(g)

	logLevel := resolveLogLevel(resolver, quietFlag, verboseFlag)
	logger := ui.NewLogger(os.Stderr, logLevel)

	runID := newRunID()
	started := time.Now()
	counters := newRunCounters()
	emit = counters.wrap(emit)
	emit = debugTraceEmit(logger, emit)

	// clearMarker and the early-signal/panic handlers are set up before
	// the run marker exists (and before the allowlist prompt, which can
	// block on stdin) so a signal or panic anywhere from here on still
	// prints a summary and exits cleanly. session.ClearCurrentRun on a
	// marker that was never written is a safe no-op.
	clearMarker := func() {
		if err := session.ClearCurrentRun(centrolDir(root)); err != nil {
			fmt.Fprintf(os.Stderr, "centrol proxy: warning: could not clear run marker: %v\n", err)
		}
	}
	stopEarlySignal := earlySignalHandler(func(code int) {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
		os.Exit(code)
	})
	defer func() {
		if r := recover(); r != nil {
			printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
			clearMarker()
			fmt.Fprintf(os.Stderr, "centrol proxy: panic: %v\n", r)
			os.Exit(ui.ExitRuntimeError)
		}
	}()
	// printSummaryAndClear is the shared "about to exit early" sequence
	// for every config-resolution fatalf/os.Exit below — each one is a
	// trappable exit path per section 5, even ones that fire before the
	// wrapped MCP server ever starts.
	printSummaryAndClear := func() {
		printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)
		clearMarker()
	}

	threshold, threshSource, err := policy.ResolveMassMutationThreshold(resolver)
	if err != nil {
		printSummaryAndClear()
		fatalHint(ui.ExitUserError, []string{"run `centrol config` to fix it interactively, or edit .centrol/config.toml / ~/.centrol/config.toml directly"},
			"%v", err)
	}

	contract := policy.DefaultContract(policy.KindProxy, runID, root)
	contract.MassMutationThreshold = threshold
	gc := newGuardedContract(contract)
	_ = threshSource // available for a future --verbose provenance line; not surfaced by default to keep startup output quiet

	observeFromConfig, _, cerr := policy.ResolveObserveMode(resolver)
	if cerr != nil {
		printSummaryAndClear()
		fatalf("centrol proxy: %v", cerr)
	}
	observe := observeFlag || observeFromConfig
	if observe {
		logger.Infof("centrol: observe mode — evaluating policy, never prompting, never blocking\n")
	}

	// MCP server allowlist pre-flight: an unknown server (by
	// package/binary identity, unless strict_matching is on) prompts
	// once before the session starts rather than blocking silently.
	// Observe mode skips this entirely — no prompts, no gate — and
	// simply proceeds, matching the same "never prompt, never block"
	// contract as the tool-call-level policy evaluation below.
	//
	// This whole pre-flight is stdio-only: it exists to vet an
	// arbitrary local subprocess (an npx/uvx package, say) by
	// package/binary identity before spawning it. An HTTP target is a
	// URL the operator configured directly (--target-url or the
	// persisted proxy.target_url) — there is no subprocess identity
	// here for the allowlist to match against, so there is nothing for
	// this step to do for useHTTP.
	if !useHTTP {
		identity := policy.ServerIdentity(fields[0], fields[1:])
		allowed, _, err := policy.ResolveAllowedServers(resolver)
		if err != nil {
			printSummaryAndClear()
			fatalf("centrol proxy: %v", err)
		}
		strict, _, err := policy.ResolveStrictMatching(resolver)
		if err != nil {
			printSummaryAndClear()
			fatalf("centrol proxy: %v", err)
		}
		if !observe && !policy.MatchesAllowlist(identity, target, allowed, strict) {
			decision := promptServerAllowlist(identity, target)
			switch decision {
			case allowlistDeny:
				fmt.Fprintf(os.Stderr, "centrol: denied — %s is not on the MCP server allowlist\n", identity)
				printSummaryAndClear()
				os.Exit(ui.ExitPolicyBlock)
			case allowlistAdd:
				src, err := resolver.WriteConfig(policy.KeyAllowedServers, append(allowed, identity))
				if err != nil {
					printSummaryAndClear()
					fatalf("centrol proxy: adding %s to the allowlist: %v", identity, err)
				}
				_ = emit(runID, "proxy", "policy.amend", map[string]interface{}{
					"action": "add_to_allowlist", "server": identity, "source": src,
				})
				logger.Infof("centrol: added %s to the allowlist (%s)\n", identity, src)
			case allowlistAllowOnce, allowlistAllowOnceUnattended, allowlistAllowSession:
				_ = emit(runID, "proxy", "policy.amend", map[string]interface{}{
					"action": "allow_server", "server": identity, "scope": decision.String(), "source": policy.SourceSessionContract,
				})
			}
		} else if observe && !policy.MatchesAllowlist(identity, target, allowed, strict) {
			_ = emit(runID, "proxy", "policy.amend", map[string]interface{}{
				"action": "allow_server", "server": identity, "decision": "would_flag", "source": policy.SourceSessionContract,
			})
		}
	}

	if err := session.WriteCurrentRun(centrolDir(root), session.RunMarker{
		RunID: runID, Kind: "proxy", RepoRoot: root, StartedAt: time.Now().UTC(),
		AllowedPaths: contract.AllowedPaths,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "centrol proxy: warning: could not write run marker: %v\n", err)
	}
	// clearMarker is defined earlier (before the allowlist prompt) so
	// the early-signal/panic handlers above can call it too. Still not
	// a defer — fatalf below calls os.Exit, which skips deferred
	// functions; clearMarker is called explicitly on every exit path
	// past this point instead.

	_ = emit(runID, "proxy", "run.start", map[string]interface{}{"target": targetDesc})

	stopScope := make(chan struct{})
	go func() {
		var offset int64
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopScope:
				return
			case <-ticker.C:
				reqs, newOffset, err := session.PollScopeRequests(centrolDir(root), offset)
				if err != nil {
					continue
				}
				offset = newOffset
				for _, r := range reqs {
					gc.amend(r.Path)
					_ = emit(runID, "proxy", "contract.declare", map[string]interface{}{"amend": r.Path, "requested_at": r.TS})
				}
			}
		}
	}()

	promptTimeoutSec, _, err := policy.ResolvePromptTimeoutSeconds(resolver)
	if err != nil {
		printSummaryAndClear()
		fatalf("centrol proxy: %v", err)
	}
	allowSessionAmend, _, err := policy.ResolveAllowSessionAmend(resolver)
	if err != nil {
		printSummaryAndClear()
		fatalf("centrol proxy: %v", err)
	}
	additionalBlockPaths, _, err := policy.ResolveAdditionalBlockPaths(resolver)
	if err != nil {
		printSummaryAndClear()
		fatalf("centrol proxy: %v", err)
	}
	if len(additionalBlockPaths) > 0 {
		// Canonicalized ONCE here, at config load, exactly like the
		// built-in defaults inside policy.DefaultContract — never
		// per-event. See policy.CanonicalizeBlockPath for what that
		// buys: macOS's /etc -> /private/etc, "../../../etc"-style
		// relative entries, and a configured path that is itself a
		// symlink all resolve to the same canonical form the proxy's
		// per-call comparison expects.
		contract.ProtectedPaths = append(contract.ProtectedPaths, policy.CanonicalizeBlockPaths(additionalBlockPaths)...)
		gc = newGuardedContract(contract)
	}

	mcpCallTimeoutSec, _, err := policy.ResolveMCPCallTimeoutSeconds(resolver)
	if err != nil {
		printSummaryAndClear()
		fatalf("centrol proxy: %v", err)
	}

	// http_timeout_seconds/http_max_retries only govern an HTTP target
	// (transport.HTTPTarget's Timeout/MaxRetries below) — resolved only
	// for useHTTP so a misconfigured value in that section never fails
	// an unrelated stdio run.
	var httpTimeoutSec, httpMaxRetries int
	if useHTTP {
		httpTimeoutSec, _, err = policy.ResolveProxyHTTPTimeoutSeconds(resolver)
		if err != nil {
			printSummaryAndClear()
			fatalf("centrol proxy: %v", err)
		}
		httpMaxRetries, _, err = policy.ResolveProxyHTTPMaxRetries(resolver)
		if err != nil {
			printSummaryAndClear()
			fatalf("centrol proxy: %v", err)
		}
	}

	prompt := proxy.StderrPrompt(os.Stderr, time.Duration(promptTimeoutSec)*time.Second)
	interceptor := proxy.NewInterceptor(runID, gc.get(), emit, prompt, allowSessionAmend)
	interceptor.Observe = observe
	interceptor.MCPCallTimeout = time.Duration(mcpCallTimeoutSec) * time.Second
	// ContractFunc makes tools/call evaluation consult gc fresh on every
	// call, the same way guard's contractAwareEmit (contract_runtime.go)
	// calls gc.get() fresh on every fs event — so a mid-run `centrol
	// scope +<path>` takes effect on the very next tools/call here, just
	// as it already does for guard's watcher path. The NewInterceptor
	// call above still passes gc.get() as Contract's initial value,
	// which only matters if ContractFunc is ever unset; with it set,
	// every evaluation in this run reads through gc instead.
	interceptor.ContractFunc = gc.get

	// Unlike centrol guard (which hands signal responsibility to
	// terminal.Run's own forwarding once the wrapped agent starts),
	// neither proxy.Run nor proxy.RunTarget has signal handling of its
	// own — they manage the target directly. So the early handler stays
	// armed for the whole call instead of handing off partway through.
	var runErr error
	if useHTTP {
		httpTarget := &transport.HTTPTarget{
			URL:        targetURL,
			Timeout:    time.Duration(httpTimeoutSec) * time.Second,
			MaxRetries: httpMaxRetries,
		}
		runErr = proxy.RunTarget(httpTarget, interceptor, os.Stdin, os.Stdout, os.Stderr)
	} else {
		runErr = proxy.Run(proxy.Target{Command: fields[0], Args: fields[1:]}, interceptor, os.Stdin, os.Stdout, os.Stderr)
	}
	stopEarlySignal()
	close(stopScope)

	endPayload := map[string]interface{}{}
	if runErr != nil {
		endPayload["error"] = runErr.Error()
	}
	if errors.Is(runErr, proxy.ErrStreamRead) {
		counters.markStreamError()
	}
	_ = emit(runID, "proxy", "run.end", endPayload)
	clearMarker()

	// Printed on every trappable exit from here: normal completion and
	// the runErr path below. Never gated by --quiet — printRunSummary
	// writes straight to os.Stderr, not through logger.
	printRunSummary(os.Stderr, runID, ledgerPath(root), started, counters)

	if runErr != nil {
		fatalf("centrol proxy: %v", runErr)
	}
}
