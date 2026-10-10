// Command centrol is a local-first agent governance tool: it wraps an
// agent's filesystem access (guard) or its MCP tool calls (proxy),
// keeps an append-only hash-chained record of what happened (the
// ledger, via audit), and lets you roll back a run's changes (undo).
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "guard":
		cmdGuard(os.Args[2:])
	case "proxy":
		cmdProxy(os.Args[2:])
	case "audit":
		cmdAudit(os.Args[2:])
	case "undo":
		cmdUndo(os.Args[2:])
	case "scope":
		cmdScope(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "config":
		cmdConfig(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "--version", "-v":
		fmt.Println(versionString())
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "centrol: unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// versionString describes this build from the build info the Go toolchain
// stamps into the binary: the module version when there is one (a
// `go install pkg@vX.Y.Z` build), and the VCS revision for a build from a
// checkout. Nothing is hardcoded.
func versionString() string {
	info, ok := debug.ReadBuildInfo()
	return formatVersion(info, ok)
}

func formatVersion(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return "centrol (version unknown)"
	}
	version := info.Main.Version
	var rev string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	var parts []string
	if version != "" && version != "(devel)" {
		parts = append(parts, version)
	}
	if rev != "" {
		if modified {
			rev += "+modified"
		}
		parts = append(parts, rev)
	}
	if len(parts) == 0 {
		return "centrol (version unknown)"
	}
	return "centrol " + strings.Join(parts, " ")
}

func printUsage() {
	fmt.Fprint(os.Stderr, `centrol - local-first agent governance

Usage:
  centrol guard [--scope <path>]... [--observe] [--quiet|--verbose] -- <agent command>
                                          wrap an agent run
  centrol proxy [--observe] [--quiet|--verbose] --target <mcp server>   run the MCP gateway
  centrol proxy install <client>         patch a client's MCP config
  centrol audit                          view the ledger
  centrol audit --verify                 verify hash-chain integrity across all segments
  centrol undo                           restore the last snapshot
  centrol undo --list                    list snapshots
  centrol undo --from <snapshot_id>      restore a specific snapshot
  centrol scope +<path>                  widen the current run's contract
  centrol status                         active run, contract, honesty score
  centrol config                         interactive menu: allowlist, gate, proxy, guard, general, thresholds
  centrol --version                      print the build version
  centrol verify                         [reserved for v0.3] validate a change before it lands

Logging:
  --quiet     log_level=warn — suppress step boundaries and prompts;
              errors and the run summary always still print
  --verbose   log_level=debug — also print internal state transitions
              (every governed event, traced to stderr)
  general.log_level in config sets the default (info | warn | debug)
  when neither flag is given.

Exit codes:
  0  success
  1  user error (bad args, missing or invalid config)
  2  runtime error (lock conflict, disk full, and other operational failures)
  3  policy block (the action was denied by policy)

Color is used automatically on a real terminal and dropped when output
is piped or redirected, or when NO_COLOR is set (see https://no-color.org).
Glyphs (⚠ ◇ ✓ ✗) fall back to ASCII (! o + x) on a non-UTF-8 terminal.
`)
}
