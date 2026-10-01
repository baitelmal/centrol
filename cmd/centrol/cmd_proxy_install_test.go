package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeClaudeDesktopConfig points HOME at a fresh temp dir and writes
// the given mcpServers map as that client's config file, returning the
// config path so the test can read it back after cmdProxyInstall runs.
// "claude-desktop" is used throughout (rather than cursor/windsurf)
// because its path resolution is exercised elsewhere; item 4's wrapping
// logic itself is client-agnostic, so one client is enough to cover it.
func writeClaudeDesktopConfig(t *testing.T, servers map[string]interface{}) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config", "Claude")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "claude_desktop_config.json")
	doc := map[string]interface{}{"mcpServers": servers}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// readServers reads back the patched config's mcpServers map.
func readServers(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("patched config is not valid JSON: %v\n%s", err, raw)
	}
	servers, ok := doc["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("patched config has no mcpServers map:\n%s", raw)
	}
	return servers
}

// TestProxyInstallWrapsStdioEntry is regression coverage for the
// pre-existing stdio-wrap behavior (cmd_proxy_install.go had no test
// file at all before Pass 4 item 4).
func TestProxyInstallWrapsStdioEntry(t *testing.T) {
	path := writeClaudeDesktopConfig(t, map[string]interface{}{
		"fs": map[string]interface{}{
			"command": "npx",
			"args":    []interface{}{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		},
	})

	cmdProxyInstall([]string{"claude-desktop"})

	entry, ok := readServers(t, path)["fs"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected fs entry to survive as a map")
	}
	selfPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if entry["command"] != selfPath {
		t.Fatalf("expected command rewritten to centrol's own path %q, got %v", selfPath, entry["command"])
	}
	argsList, ok := entry["args"].([]interface{})
	if !ok || len(argsList) != 3 || argsList[0] != "proxy" || argsList[1] != "--target" {
		t.Fatalf("expected args [\"proxy\" \"--target\" \"npx -y ...\"], got %v", entry["args"])
	}
	if argsList[2] != "npx -y @modelcontextprotocol/server-filesystem /tmp" {
		t.Fatalf("expected the original command+args folded into one --target string, got %q", argsList[2])
	}
}

// TestProxyInstallWrapsURLEntry covers item 4: an entry declaring a
// remote server via "url" is wrapped through --target-url instead of
// being skipped, and the url field is removed so the entry doesn't
// look like both a stdio and a remote server afterward.
func TestProxyInstallWrapsURLEntry(t *testing.T) {
	path := writeClaudeDesktopConfig(t, map[string]interface{}{
		"remote": map[string]interface{}{
			"url": "https://mcp.example.com/sse",
		},
	})

	cmdProxyInstall([]string{"claude-desktop"})

	entry, ok := readServers(t, path)["remote"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected remote entry to survive as a map")
	}
	selfPath, _ := os.Executable()
	if entry["command"] != selfPath {
		t.Fatalf("expected command rewritten to centrol's own path, got %v", entry["command"])
	}
	argsList, ok := entry["args"].([]interface{})
	if !ok || len(argsList) != 3 || argsList[0] != "proxy" || argsList[1] != "--target-url" || argsList[2] != "https://mcp.example.com/sse" {
		t.Fatalf("expected args [\"proxy\" \"--target-url\" \"https://mcp.example.com/sse\"], got %v", entry["args"])
	}
	if _, present := entry["url"]; present {
		t.Fatalf("expected the url field to be deleted after wrapping, got %v", entry["url"])
	}
}

// TestProxyInstallWrapsServerUrlEntry covers the other field name
// different MCP clients use for the same concept.
func TestProxyInstallWrapsServerUrlEntry(t *testing.T) {
	path := writeClaudeDesktopConfig(t, map[string]interface{}{
		"remote": map[string]interface{}{
			"serverUrl": "https://mcp.example.com/sse",
		},
	})

	cmdProxyInstall([]string{"claude-desktop"})

	entry := readServers(t, path)["remote"].(map[string]interface{})
	argsList, ok := entry["args"].([]interface{})
	if !ok || argsList[2] != "https://mcp.example.com/sse" {
		t.Fatalf("expected serverUrl to be wrapped the same way as url, got args %v", entry["args"])
	}
	if _, present := entry["serverUrl"]; present {
		t.Fatalf("expected the serverUrl field to be deleted after wrapping, got %v", entry["serverUrl"])
	}
}

// TestProxyInstallIsIdempotentForWrappedRemoteEntry proves a second
// install run against an already-wrapped remote entry is a no-op: the
// "already wrapped" check keys only on the command field, which is why
// url/serverUrl must be deleted on first wrap (see the doc comment at
// the call site) — otherwise the second run would see a command it
// doesn't recognize as its own, or a stray url field confusing the
// entry's identity.
func TestProxyInstallIsIdempotentForWrappedRemoteEntry(t *testing.T) {
	path := writeClaudeDesktopConfig(t, map[string]interface{}{
		"remote": map[string]interface{}{
			"url": "https://mcp.example.com/sse",
		},
	})

	cmdProxyInstall([]string{"claude-desktop"})
	firstPass := readServers(t, path)["remote"].(map[string]interface{})

	cmdProxyInstall([]string{"claude-desktop"})
	secondPass := readServers(t, path)["remote"].(map[string]interface{})

	firstJSON, _ := json.Marshal(firstPass)
	secondJSON, _ := json.Marshal(secondPass)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("expected a second install run to leave an already-wrapped remote entry unchanged, got %s vs %s", firstJSON, secondJSON)
	}
}

// TestProxyInstallSkipsEntryWithNeitherCommandNorURL confirms an entry
// that is neither stdio (no command) nor remote (no url/serverUrl) is
// left alone and counted as skipped, not wrapped into a broken
// "proxy --target-url " with no argument.
func TestProxyInstallSkipsEntryWithNeitherCommandNorURL(t *testing.T) {
	path := writeClaudeDesktopConfig(t, map[string]interface{}{
		"empty": map[string]interface{}{},
	})

	cmdProxyInstall([]string{"claude-desktop"})

	entry := readServers(t, path)["empty"].(map[string]interface{})
	if _, present := entry["command"]; present {
		t.Fatalf("expected an entry with no command and no url to be left untouched, got %v", entry)
	}
}

func TestRemoteURL(t *testing.T) {
	cases := []struct {
		name    string
		entry   map[string]interface{}
		wantURL string
		wantOK  bool
	}{
		{"url field", map[string]interface{}{"url": "https://a"}, "https://a", true},
		{"serverUrl field", map[string]interface{}{"serverUrl": "https://b"}, "https://b", true},
		{"both present prefers url", map[string]interface{}{"url": "https://a", "serverUrl": "https://b"}, "https://a", true},
		{"neither field", map[string]interface{}{"command": "npx"}, "", false},
		{"empty url string", map[string]interface{}{"url": ""}, "", false},
		{"url wrong type", map[string]interface{}{"url": 42}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, gotOK := remoteURL(tc.entry)
			if gotURL != tc.wantURL || gotOK != tc.wantOK {
				t.Fatalf("remoteURL(%v) = (%q, %v), want (%q, %v)", tc.entry, gotURL, gotOK, tc.wantURL, tc.wantOK)
			}
		})
	}
}
