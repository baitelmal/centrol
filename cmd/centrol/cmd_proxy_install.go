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

// clientConfigPath returns the MCP config file path for a known client,
// per each client's own documented config location. Best-effort: these
// paths are a moving target across client versions, so `proxy install`
// backs up whatever it finds before touching it (see cmdProxyInstall).
func clientConfigPath(client string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	switch client {
	case "claude-desktop":
		switch runtime.GOOS {
		case "darwin":
			return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
		case "windows":
			appData := os.Getenv("APPDATA")
			if appData == "" {
				appData = filepath.Join(home, "AppData", "Roaming")
			}
			return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
		default: // linux: community builds only, but this is the documented path
			return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
		}
	case "cursor":
		return filepath.Join(home, ".cursor", "mcp.json"), nil
	case "windsurf":
		return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), nil
	default:
		return "", fmt.Errorf("unknown client %q (known: claude-desktop, cursor, windsurf)", client)
	}
}

// cmdProxyInstall patches a client's MCP config so its existing stdio
// servers run through `centrol proxy --target ...` instead of directly.
// Remote (url/serverUrl) servers are left untouched — this proxy only
// fronts stdio targets. A timestamped backup of the original file is
// always written first, and the patch is idempotent: re-running it
// against an already-patched entry is a no-op for that entry.
func cmdProxyInstall(args []string) {
	if len(args) == 0 {
		fatalf("centrol proxy install: usage: centrol proxy install <claude-desktop|cursor|windsurf>")
	}
	client := args[0]
	path, err := clientConfigPath(client)
	if err != nil {
		fatalf("centrol proxy install: %v", err)
	}

	selfPath, err := os.Executable()
	if err != nil {
		fatalf("centrol proxy install: resolving centrol's own path: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fatalf("centrol proxy install: %s does not exist yet — open %s once first so it creates its config, then re-run this", client, client)
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

	backupPath := fmt.Sprintf("%s.centrol-backup-%s", path, time.Now().UTC().Format("20060102T150405"))
	if err := os.WriteFile(backupPath, raw, 0o644); err != nil {
		fatalf("centrol proxy install: writing backup before touching %s: %v", path, err)
	}

	wrapped, skipped := 0, 0
	for name, v := range servers {
		entry, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if _, isRemote := entry["url"]; isRemote {
			skipped++
			continue
		}
		if _, isRemote := entry["serverUrl"]; isRemote {
			skipped++
			continue
		}
		command, _ := entry["command"].(string)
		if command == "" {
			skipped++
			continue
		}
		if command == selfPath || strings.HasSuffix(command, string(filepath.Separator)+"centrol") || command == "centrol" {
			continue // already wrapped by a previous install run
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
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fatalf("centrol proxy install: writing %s: %v", path, err)
	}

	fmt.Fprintf(os.Stderr, "centrol proxy install: patched %s\n", path)
	fmt.Fprintf(os.Stderr, "  %d server(s) wrapped, %d skipped (remote or already wrapped)\n", wrapped, skipped)
	fmt.Fprintf(os.Stderr, "  original backed up to %s\n", backupPath)
	fmt.Fprintf(os.Stderr, "  restart %s for the change to take effect\n", client)
}
