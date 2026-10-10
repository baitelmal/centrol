package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// installGOOS is runtime.GOOS; a variable so tests can exercise the
// Windows path resolution on any platform.
var installGOOS = runtime.GOOS

const claudeDesktopConfigFile = "claude_desktop_config.json"

// windowsEnv is the Windows environment the Claude Desktop lookup reads.
type windowsEnv struct {
	Home         string // the user's home directory
	AppData      string // %APPDATA%, may be empty
	LocalAppData string // %LOCALAPPDATA%, may be empty
}

// configLookup is where a client's config was looked for.
type configLookup struct {
	Path    string   // the config file to use
	Checked []string // every location considered, in order
	// MSIX is true when Path is inside a Store (MSIX) package container.
	MSIX bool
	// ClassicPath is the classic installer's config path when MSIX is
	// true, and ClassicExists reports whether that file exists. Both
	// feed the error for the case where both installs are present.
	ClassicPath   string
	ClassicExists bool
}

// claudeDesktopWindowsPath picks Claude Desktop's config path on Windows.
//
// The Store (MSIX) build of Claude Desktop runs in a per-package
// container, and Windows redirects its %APPDATA% writes to
//
//	%LOCALAPPDATA%\Packages\Claude_<publisher hash>\LocalCache\Roaming\Claude\
//
// so that is the file the running app reads. The publisher hash is
// globbed, never hardcoded. If that directory exists it wins, because
// patching the classic file would have no effect on the app; whether
// the config file inside it exists yet is the caller's business. The
// classic installer's %APPDATA%\Claude\ is the fallback, and is the
// answer when neither exists so the caller's "does not exist yet"
// error names it.
func claudeDesktopWindowsPath(e windowsEnv) configLookup {
	localAppData := e.LocalAppData
	if localAppData == "" {
		localAppData = filepath.Join(e.Home, "AppData", "Local")
	}
	appData := e.AppData
	if appData == "" {
		appData = filepath.Join(e.Home, "AppData", "Roaming")
	}

	msixDirPattern := filepath.Join(localAppData, "Packages", "Claude_*", "LocalCache", "Roaming", "Claude")
	classic := filepath.Join(appData, "Claude", claudeDesktopConfigFile)
	lk := configLookup{
		Checked:     []string{filepath.Join(msixDirPattern, claudeDesktopConfigFile), classic},
		ClassicPath: classic,
	}
	if fi, err := os.Stat(classic); err == nil && !fi.IsDir() {
		lk.ClassicExists = true
	}
	if matches, err := filepath.Glob(msixDirPattern); err == nil {
		for _, dir := range matches { // Glob returns them sorted
			if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
				lk.Path, lk.MSIX = filepath.Join(dir, claudeDesktopConfigFile), true
				return lk
			}
		}
	}
	lk.Path = classic
	return lk
}

// clientConfigPath returns the MCP config file for a known client,
// per each client's own documented config location, and every location
// it considered. Best-effort: these paths are a moving target across
// client versions, so `proxy install` backs up whatever it finds before
// touching it (see cmdProxyInstall).
func clientConfigPath(client string) (configLookup, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return configLookup{}, fmt.Errorf("resolving home directory: %w", err)
	}
	one := func(p string) (configLookup, error) { return configLookup{Path: p, Checked: []string{p}}, nil }
	switch client {
	case "claude-desktop":
		switch installGOOS {
		case "darwin":
			return one(filepath.Join(home, "Library", "Application Support", "Claude", claudeDesktopConfigFile))
		case "windows":
			return claudeDesktopWindowsPath(windowsEnv{Home: home, AppData: os.Getenv("APPDATA"), LocalAppData: os.Getenv("LOCALAPPDATA")}), nil
		default: // linux: community builds only, but this is the documented path
			return one(filepath.Join(home, ".config", "Claude", claudeDesktopConfigFile))
		}
	case "cursor":
		return one(filepath.Join(home, ".cursor", "mcp.json"))
	case "windsurf":
		return one(filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"))
	default:
		return configLookup{}, fmt.Errorf("unknown client %q (known: claude-desktop, cursor, windsurf)", client)
	}
}

// missingConfigMessage is the error for a client whose config file does
// not exist, naming every location that was checked so the next user is
// not left guessing where the tool looked.
//
// When the Store (MSIX) install of Claude Desktop is present, that is
// the app that runs, so its config is the only one worth patching. The
// message says so, and, if a classic config file also exists, says why
// that one is not used and how to use it anyway.
func missingConfigMessage(client string, lk configLookup) string {
	if lk.MSIX {
		msg := fmt.Sprintf("centrol proxy install: %s does not exist yet. The Store (MSIX) version of Claude Desktop is installed, and it reads its config from:\n  %s\nopen %s once so it creates that file, then re-run this.", client, lk.Path, client)
		if lk.ClassicExists {
			msg += fmt.Sprintf("\n\nA classic config also exists at:\n  %s\nThe Store app does not read it, so patching it would change nothing. To use the classic install instead, remove the Store package (Settings > Apps) and re-run this.", lk.ClassicPath)
		}
		return msg
	}
	msg := fmt.Sprintf("centrol proxy install: %s does not exist yet. Looked for its config at:", client)
	for _, p := range lk.Checked {
		msg += "\n  " + p
	}
	return msg + fmt.Sprintf("\nopen %s once first so it creates its config, then re-run this", client)
}

// remoteURL reports the remote server URL an entry declares, checking
// both field names different MCP clients use for the same concept —
// "url" and "serverUrl" — since there is no single standard key.
// ok is false when neither field is present, or present but not a
// non-empty string (e.g. a stdio entry with no url field at all).
func remoteURL(entry map[string]interface{}) (string, bool) {
	if u, ok := entry["url"].(string); ok && u != "" {
		return u, true
	}
	if u, ok := entry["serverUrl"].(string); ok && u != "" {
		return u, true
	}
	return "", false
}

// cmdProxyInstall patches a client's MCP config so its existing
// servers run through centrol proxy instead of directly: a stdio
// entry (command/args) through `centrol proxy --target ...`, a remote
// entry (url or serverUrl — different clients use different field
// names for the same thing) through `centrol proxy --target-url ...`
// (Pass 4, item 4 — HTTP support used to stop here at a skip, back
// when centrol proxy had no --target-url to wrap it through; see
// remoteURL and its call site below). A timestamped backup of the
// original file is always written first, and the patch is idempotent:
// re-running it against an already-patched entry (stdio or remote
// alike, both now indistinguishable from centrol's point of view —
// see the "already wrapped" check below) is a no-op for that entry.
func cmdProxyInstall(args []string) {
	if len(args) == 0 {
		fatalf("centrol proxy install: usage: centrol proxy install <claude-desktop|cursor|windsurf>")
	}
	client := args[0]
	lookup, err := clientConfigPath(client)
	if err != nil {
		fatalf("centrol proxy install: %v", err)
	}

	path := lookup.Path

	selfPath, err := os.Executable()
	if err != nil {
		fatalf("centrol proxy install: resolving centrol's own path: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fatalf("%s", missingConfigMessage(client, lookup))
		}
		fatalf("centrol proxy install: reading %s: %v", path, err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		fatalf("centrol proxy install: %s is not valid JSON, refusing to touch it: %v", path, err)
	}

	servers, _ := doc["mcpServers"].(map[string]interface{})
	if servers == nil {
		fmt.Fprintf(os.Stderr, "centrol proxy install: %s has no mcpServers configured yet — nothing to wrap\n", path)
		return
	}

	// 0600: an MCP client config can list a remote server's url with an
	// embedded token/credential (see remoteURL below) — both the backup
	// and the rewritten file keep whatever permissions the client's own
	// config had, but centrol itself writes both at the restrictive bar
	// rather than defaulting to world-readable.
	backupPath := fmt.Sprintf("%s.centrol-backup-%s", path, time.Now().UTC().Format("20060102T150405"))
	if err := os.WriteFile(backupPath, raw, 0o600); err != nil {
		fatalf("centrol proxy install: writing backup before touching %s: %v", path, err)
	}

	wrapped, skipped := 0, 0
	for name, v := range servers {
		entry, ok := v.(map[string]interface{})
		if !ok {
			continue
		}

		// "Already wrapped" is checked first, and by command alone,
		// so it catches both kinds of previously-wrapped entry the
		// same way: a stdio entry's own command/args are gone once
		// wrapped (replaced by centrol's), and a remote entry's url/
		// serverUrl field is deleted once wrapped (below) — so on a
		// second install run, a previously-wrapped remote entry looks
		// exactly like a previously-wrapped stdio entry to this check,
		// and both correctly fall through as a no-op.
		command, _ := entry["command"].(string)
		if command != "" && (command == selfPath || strings.HasSuffix(command, string(filepath.Separator)+"centrol") || command == "centrol") {
			continue // already wrapped by a previous install run
		}

		if urlStr, ok := remoteURL(entry); ok {
			// Pass 4, item 4: centrol proxy gained --target-url
			// support, so a remote entry is wrapped exactly like a
			// stdio one — same backup-then-rewrite pattern, same
			// idempotency check above — instead of being skipped.
			// url/serverUrl is deleted, not left alongside the new
			// command/args: leaving it would make this entry look
			// like both a stdio AND a remote server to the next
			// client that reads it (and to the "already wrapped"
			// check above, which must see a clean command-only entry
			// on the next install run).
			entry["command"] = selfPath
			entry["args"] = []interface{}{"proxy", "--target-url", urlStr}
			delete(entry, "url")
			delete(entry, "serverUrl")
			servers[name] = entry
			wrapped++
			continue
		}

		if command == "" {
			skipped++
			continue
		}

		var argStrs []string
		if rawArgs, ok := entry["args"].([]interface{}); ok {
			for _, a := range rawArgs {
				if s, ok := a.(string); ok {
					argStrs = append(argStrs, s)
				}
			}
		}
		targetCmd := strings.TrimSpace(command + " " + strings.Join(argStrs, " "))

		entry["command"] = selfPath
		entry["args"] = []interface{}{"proxy", "--target", targetCmd}
		servers[name] = entry
		wrapped++
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fatalf("centrol proxy install: encoding patched config: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		fatalf("centrol proxy install: writing %s: %v", path, err)
	}

	fmt.Fprintf(os.Stderr, "centrol proxy install: patched %s\n", path)
	fmt.Fprintf(os.Stderr, "  %d server(s) wrapped, %d skipped (remote or already wrapped)\n", wrapped, skipped)
	fmt.Fprintf(os.Stderr, "  original backed up to %s\n", backupPath)
	fmt.Fprintf(os.Stderr, "  restart %s for the change to take effect\n", client)
}
