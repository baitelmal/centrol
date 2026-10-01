package main

import (
	"context"
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
	// end is the one run-scoped "end the run" decision point every exit
	// site below calls through, from here (runID acquisition) onward
	// (Pass 3.9c Final) — see its doc comment in summary.go for the
	// win/lose contract. It is also the only thing that ever calls
	// Governor.Exit for this run.
	end := endRun(g, runID, ledgerPath(root), started, counters, clearMarker)
	// ctx/signalOutcome cover this function's own pre-Run setup window
	// (Pass 3.9c Final correction): every blocking setup step below —
	// including the allowlist prompt, which can block on stdin — runs
	// through runSetup, which blocks THIS goroutine (main's own) on a
	// select against ctx, rather than ever letting the signal watch
	// itself call end/Exit from its own goroutine. See newSignalWatch's
	// and runSetup's doc comments in summary.go for why that distinction
	// matters. Once g.Run is called below, Run registers its own signal
	// watch for its full duration (Pass 3.9c Final, Section 2 "Signals")
	// — unlike centrol guard, Governor.Run manages the target directly
	// with no separate forwarding mechanism to hand off to, so this
	// watch steps aside right before that call instead of racing it.
	ctx, signalOutcome, stopSignalWatch := newSignalWatch(g)
	defer func() {
		if r := recover(); r != nil {
			end("panic", ui.ExitRuntimeError, func() {
				fmt.Fprintf(os.Stderr, "centrol proxy: panic: %v\n", r)
			})
		}
	}()

	var threshold int
	var threshSource string
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		threshold, threshSource, e = policy.ResolveMassMutationThreshold(resolver)
		return e
	}); err != nil {
		end("resolve_mass_mutation_threshold_failed", ui.ExitUserError, func() {
			ui.Errorf(os.Stderr, []string{"run `centrol config` to fix it interactively, or edit .centrol/config.toml / ~/.centrol/config.toml directly"},
				"%v", err)
		})
		return
	}

	contract := policy.DefaultContract(policy.KindProxy, runID, root)
	contract.MassMutationThreshold = threshold
	gc := newGuardedContract(contract)
	_ = threshSource // available for a future --verbose provenance line; not surfaced by default to keep startup output quiet

	var observeFromConfig bool
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		observeFromConfig, _, e = policy.ResolveObserveMode(resolver)
		return e
	}); err != nil {
		end("resolve_observe_mode_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
		})
		return
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
		var allowed []string
		if err := runSetup(ctx, end, signalOutcome, func() error {
			var e error
			allowed, _, e = policy.ResolveAllowedServers(resolver)
			return e
		}); err != nil {
			end("resolve_allowed_servers_failed", 1, func() {
				fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
			})
			return
		}
		var strict bool
		if err := runSetup(ctx, end, signalOutcome, func() error {
			var e error
			strict, _, e = policy.ResolveStrictMatching(resolver)
			return e
		}); err != nil {
			end("resolve_strict_matching_failed", 1, func() {
				fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
			})
			return
		}
		if !observe && !policy.MatchesAllowlist(identity, target, allowed, strict) {
			// promptServerAllowlist blocks on stdin (with its own
			// timeout) — exactly the kind of step runSetup exists for:
			// a signal arriving while a user is being prompted must
			// still interrupt main's own goroutine via ctx, not have the
			// signal watch call end/Exit itself.
			var decision allowlistDecision
			if err := runSetup(ctx, end, signalOutcome, func() error {
				decision = promptServerAllowlist(identity, target)
				return nil
			}); err != nil {
				// promptServerAllowlist's own step func never returns an
				// error; this branch is unreachable in practice, but
				// every runSetup call site handles its error the same
				// way on principle.
				end("allowlist_prompt_failed", 1, func() {
					fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
				})
				return
			}
			switch decision {
			case allowlistDeny:
				end("allowlist_denied", ui.ExitPolicyBlock, func() {
					fmt.Fprintf(os.Stderr, "centrol: denied — %s is not on the MCP server allowlist\n", identity)
				})
				return
			case allowlistAdd:
				var src string
				if err := runSetup(ctx, end, signalOutcome, func() error {
					var e error
					src, e = resolver.WriteConfig(policy.KeyAllowedServers, append(allowed, identity))
					return e
				}); err != nil {
					end("allowlist_add_failed", 1, func() {
						fmt.Fprintf(os.Stderr, "centrol proxy: adding %s to the allowlist: %v\n", identity, err)
					})
					return
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

	var promptTimeoutSec int
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		promptTimeoutSec, _, e = policy.ResolvePromptTimeoutSeconds(resolver)
		return e
	}); err != nil {
		end("resolve_prompt_timeout_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
		})
		return
	}
	var allowSessionAmend bool
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		allowSessionAmend, _, e = policy.ResolveAllowSessionAmend(resolver)
		return e
	}); err != nil {
		end("resolve_allow_session_amend_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
		})
		return
	}
	var additionalBlockPaths []string
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		additionalBlockPaths, _, e = policy.ResolveAdditionalBlockPaths(resolver)
		return e
	}); err != nil {
		end("resolve_additional_block_paths_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
		})
		return
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

	var mcpCallTimeoutSec int
	if err := runSetup(ctx, end, signalOutcome, func() error {
		var e error
		mcpCallTimeoutSec, _, e = policy.ResolveMCPCallTimeoutSeconds(resolver)
		return e
	}); err != nil {
		end("resolve_mcp_call_timeout_failed", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
		})
		return
	}

	// http_timeout_seconds/http_max_retries only govern an HTTP target
	// (transport.HTTPTarget's Timeout/MaxRetries below) — resolved only
	// for useHTTP so a misconfigured value in that section never fails
	// an unrelated stdio run.
	var httpTimeoutSec, httpMaxRetries int
	if useHTTP {
		if err := runSetup(ctx, end, signalOutcome, func() error {
			var e error
			httpTimeoutSec, _, e = policy.ResolveProxyHTTPTimeoutSeconds(resolver)
			return e
		}); err != nil {
			end("resolve_http_timeout_failed", 1, func() {
				fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
			})
			return
		}
		if err := runSetup(ctx, end, signalOutcome, func() error {
			var e error
			httpMaxRetries, _, e = policy.ResolveProxyHTTPMaxRetries(resolver)
			return e
		}); err != nil {
			end("resolve_http_max_retries_failed", 1, func() {
				fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", err)
			})
			return
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

	// Governor.Run registers its own signal watch for the duration of
	// the call (Pass 3.9c Final, Section 2 "Signals") — unlike centrol
	// guard, it manages the target directly with no separate forwarding
	// mechanism to hand signal responsibility off to. So this
	// function's own pre-Run watch steps aside right here, immediately
	// before the call, rather than staying armed alongside Run's.
	stopSignalWatch()
	// checkCancelled AFTER stopSignalWatch, not before: stopSignalWatch
	// (newSignalWatch's own stop, cmd/centrol/summary.go — which settles
	// the underlying WatchSignals watch but deliberately never
	// unregisters it; see that doc comment) blocks until the watch's
	// goroutine has made its final, settled decision about any signal
	// it may have received, so this check sees the complete truth for
	// the entire pre-Run window — including a signal landing in the
	// plain synchronous stretch since the last runSetup call above
	// (building the interceptor, wiring gc.get, nothing ctx.Done()
	// could otherwise interrupt), AND a signal arriving in the exact
	// instant of this handoff itself. Checking before stopSignalWatch
	// returned used to miss that second case: a signal this watch
	// itself had already consumed, but not yet finished processing,
	// could still call onReceived/cancel() strictly after this check
	// had already run and after this function had already moved on
	// into g.Run — a real signal, swallowed by this watch, with no
	// reader left for the cancellation it eventually produced. See
	// Governor.WatchSignals' doc comment in internal/governor/run.go
	// for the fix (and for why this watch's registration is left
	// active rather than unregistered here).
	checkCancelled(end, signalOutcome)
	var runErr error
	if useHTTP {
		httpTarget := &transport.HTTPTarget{
			URL:        targetURL,
			Timeout:    time.Duration(httpTimeoutSec) * time.Second,
			MaxRetries: httpMaxRetries,
		}
		runErr = g.Run(context.Background(), httpTarget, interceptor, os.Stdin, os.Stdout, os.Stderr)
	} else {
		runErr = g.Run(context.Background(), transport.NewStdioTarget(fields[0], fields[1:], os.Stderr), interceptor, os.Stdin, os.Stdout, os.Stderr)
	}
	close(stopScope)

	endPayload := map[string]interface{}{}
	if runErr != nil {
		endPayload["error"] = runErr.Error()
	}
	if errors.Is(runErr, proxy.ErrStreamRead) {
		counters.markStreamError()
	}
	_ = emit(runID, "proxy", "run.end", endPayload)

	// end prints the run summary and clears the marker on every
	// trappable exit from here: normal completion, and the runErr branch
	// below. Never gated by --quiet — printRunSummary writes straight to
	// os.Stderr, not through logger.
	if runErr != nil {
		end("agent_run_error", 1, func() {
			fmt.Fprintf(os.Stderr, "centrol proxy: %v\n", runErr)
		})
		return
	}
	end("agent_exit", 0, nil)
}
