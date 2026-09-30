package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/scirem/centrol/internal/governor"
	"github.com/scirem/centrol/internal/ledger"
	"github.com/scirem/centrol/internal/policy"
	"github.com/scirem/centrol/internal/ui"
)

// repoRoot resolves the git repository root for the current working
// directory. Every command that touches the ledger, snapshots, or
// contracts is rooted here rather than at the raw cwd, so it behaves
// the same whether invoked from the repo root or a subdirectory.
func repoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository (centrol needs one): %s", strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func centrolDir(root string) string {
	return filepath.Join(root, ".centrol")
}

func ledgerPath(root string) string {
	return filepath.Join(centrolDir(root), "lighthouse.jsonl")
}

func snapshotsDir(root string) string {
	return filepath.Join(centrolDir(root), "snapshots")
}

// openGovernorAt opens the ledger + Governor rooted at repoRoot's
// .centrol/ directory.
func openGovernorAt(root string) (*governor.Governor, error) {
	l, err := ledger.Open(ledgerPath(root), ledger.DefaultRotateAtMB)
	if err != nil {
		return nil, err
	}
	return governor.New(l)
}

// newRunID mints a fresh, unique per-run identifier: a timestamp for
// readability in `centrol audit` output, plus a short random suffix so
// two runs started in the same second never collide.
func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("run-%s-%s", time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

// emitFunc adapts governor.Governor.Emit to the plain EmitFunc shape
// guard/proxy/policy code expects, without those packages needing to
// import governor (or know about ErrSilenced) directly. A silenced
// emission is not an error the caller needs to see — the Governor has
// already recorded it fully; guard/proxy just move on.
func emitFunc(g *governor.Governor) func(run, src, typ string, payload map[string]interface{}) error {
	return func(run, src, typ string, payload map[string]interface{}) error {
		_, err := g.Emit(run, src, typ, payload)
		if err == governor.ErrSilenced {
			return nil
		}
		return err
	}
}

func userConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "" // resolved to a LocalFileSource that will just never find/write anything usable; not fatal on its own
	}
	return filepath.Join(home, ".centrol", "config.toml")
}

func repoConfigPath(root string) string {
	return filepath.Join(centrolDir(root), "config.toml")
}

// newResolver builds the standard v0.1 chain: session contract (empty —
// nothing in the CLI sets per-run overrides yet, but the seam exists),
// enterprise policy (a real stub, see internal/policy.EnterpriseSource),
// repo config, user config, then whatever built-in defaults the
// specific Resolve* helper being called already knows.
func newResolver(root string) *policy.Resolver {
	return policy.NewResolver(
		policy.NewSessionContractSource(),
		policy.NewEnterpriseSource(),
		policy.NewLocalFileSource(policy.SourceRepoConfig, repoConfigPath(root)),
		policy.NewLocalFileSource(policy.SourceUserConfig, userConfigPath()),
		policy.NewDefaultSource(nil),
	)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// fatalHint is fatalf with one or more "next command" hint lines and an
// explicit exit code (see internal/ui's ExitUserError/ExitRuntimeError/
// ExitPolicyBlock) — every error in this codebase names what to run
// next rather than stopping at "what went wrong".
func fatalHint(code int, hints []string, format string, args ...interface{}) {
	ui.Errorf(os.Stderr, hints, format, args...)
	os.Exit(code)
}

// resolveLogLevel resolves general.log_level through the config chain
// and applies the --quiet/--verbose CLI overrides on top: --quiet is
// exactly log_level="warn", --verbose is exactly log_level="debug", per
// spec section 2f. Neither flag set falls back to whatever log_level
// resolved to (config, or the built-in "info" default). Both flags set
// (a user error, but not one worth refusing over) prefers --verbose,
// since showing more is the safer failure than hiding it.
func resolveLogLevel(resolver *policy.Resolver, quiet, verbose bool) ui.Level {
	levelStr, _, err := policy.ResolveLogLevel(resolver)
	if err != nil {
		fatalf("centrol: %v", err)
	}
	level, _ := ui.ParseLevel(levelStr) // policy.ResolveLogLevel already validated the enum; ok=false can't happen here
	if verbose {
		return ui.LevelDebug
	}
	if quiet {
		return ui.LevelWarn
	}
	return level
}

// debugTraceEmit wraps an EmitFunc so every governed emission is also
// traced to stderr at debug level (--verbose only; a no-op under info
// or warn) — the "internal state transitions" section 2f calls for,
// without threading a logger through governor/guard/proxy/policy
// themselves. The ledger write always happens first and is unaffected
// either way; this only adds an extra, optional stderr echo of it.
func debugTraceEmit(logger ui.Logger, inner func(run, src, typ string, payload map[string]interface{}) error) func(run, src, typ string, payload map[string]interface{}) error {
	return func(run, src, typ string, payload map[string]interface{}) error {
		err := inner(run, src, typ, payload)
		logger.Debugf("centrol: [%s] %s %s %v\n", src, run, typ, payload)
		return err
	}
}
