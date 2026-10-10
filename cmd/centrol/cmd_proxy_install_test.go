package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// --- Windows: MSIX-virtualized Claude Desktop ---------------------------

// winLayout builds a fake Windows profile under a temp dir and returns
// the environment plus the MSIX container dir and classic dir paths
// (neither created yet).
func winLayout(t *testing.T) (env windowsEnv, msixDir, classicDir string) {
	t.Helper()
	root := t.TempDir()
	env = windowsEnv{
		Home:         filepath.Join(root, "home"),
		AppData:      filepath.Join(root, "home", "AppData", "Roaming"),
		LocalAppData: filepath.Join(root, "home", "AppData", "Local"),
	}
	msixDir = filepath.Join(env.LocalAppData, "Packages", "Claude_pzs8sxrjxfjjc", "LocalCache", "Roaming", "Claude")
	classicDir = filepath.Join(env.AppData, "Claude")
	return env, msixDir, classicDir
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsResolvesMSIXContainer(t *testing.T) {
	env, msix, _ := winLayout(t)
	mkdir(t, msix)
	got := claudeDesktopWindowsPath(env).Path
	if want := filepath.Join(msix, "claude_desktop_config.json"); got != want {
		t.Fatalf("path = %s, want the MSIX path %s", got, want)
	}
}

// The publisher hash is globbed, not hardcoded.
func TestWindowsMSIXPublisherHashIsGlobbed(t *testing.T) {
	env, _, _ := winLayout(t)
	other := filepath.Join(env.LocalAppData, "Packages", "Claude_zzz999other", "LocalCache", "Roaming", "Claude")
	mkdir(t, other)
	got := claudeDesktopWindowsPath(env).Path
	if want := filepath.Join(other, "claude_desktop_config.json"); got != want {
		t.Fatalf("path = %s, want %s", got, want)
	}
}

func TestWindowsOnlyClassicPathPresent(t *testing.T) {
	env, _, classic := winLayout(t)
	mkdir(t, classic)
	got := claudeDesktopWindowsPath(env).Path
	if want := filepath.Join(classic, "claude_desktop_config.json"); got != want {
		t.Fatalf("path = %s, want the classic path %s", got, want)
	}
}

func TestWindowsBothPresentPrefersMSIX(t *testing.T) {
	env, msix, classic := winLayout(t)
	mkdir(t, msix)
	mkdir(t, classic)
	got := claudeDesktopWindowsPath(env).Path
	if want := filepath.Join(msix, "claude_desktop_config.json"); got != want {
		t.Fatalf("path = %s, want MSIX %s when both exist", got, want)
	}
}

// A Claude_* directory without the Roaming\Claude subtree is not the
// config container.
func TestWindowsIgnoresPackageWithoutConfigDir(t *testing.T) {
	env, _, classic := winLayout(t)
	mkdir(t, filepath.Join(env.LocalAppData, "Packages", "Claude_pzs8sxrjxfjjc", "LocalCache"))
	mkdir(t, classic)
	got := claudeDesktopWindowsPath(env).Path
	if want := filepath.Join(classic, "claude_desktop_config.json"); got != want {
		t.Fatalf("path = %s, want classic %s", got, want)
	}
}

// With neither present the classic path is returned, so the caller's
// "does not exist yet" error fires, and the message names both paths.
func TestWindowsNeitherPresentErrorNamesBothPaths(t *testing.T) {
	env, _, classic := winLayout(t)
	lk := claudeDesktopWindowsPath(env)
	if want := filepath.Join(classic, "claude_desktop_config.json"); lk.Path != want || lk.MSIX {
		t.Fatalf("path = %s (msix %v), want the classic path %s", lk.Path, lk.MSIX, want)
	}
	if len(lk.Checked) != 2 {
		t.Fatalf("checked = %v, want the MSIX pattern and the classic path", lk.Checked)
	}
	msg := missingConfigMessage("claude-desktop", lk)
	for _, want := range []string{"does not exist yet", filepath.Join("Packages", "Claude_*"), lk.Checked[1]} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// End to end through cmdProxyInstall with the Windows branch selected:
// the MSIX file is the one patched, the classic one is left alone, and
// a missing config file inside an existing MSIX dir is read as absent
// rather than created.
func TestProxyInstallPatchesTheMSIXConfig(t *testing.T) {
	env, msix, classic := winLayout(t)
	t.Setenv("HOME", env.Home)
	t.Setenv("USERPROFILE", env.Home)
	t.Setenv("APPDATA", env.AppData)
	t.Setenv("LOCALAPPDATA", env.LocalAppData)
	old := installGOOS
	installGOOS = "windows"
	defer func() { installGOOS = old }()

	doc, _ := json.Marshal(map[string]interface{}{"mcpServers": map[string]interface{}{
		"fs": map[string]interface{}{"command": "npx", "args": []interface{}{"x"}},
	}})
	for _, dir := range []string{msix, classic} {
		mkdir(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "claude_desktop_config.json"), doc, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cmdProxyInstall([]string{"claude-desktop"})

	msixCmd := readServers(t, filepath.Join(msix, "claude_desktop_config.json"))["fs"].(map[string]interface{})["command"]
	classicCmd := readServers(t, filepath.Join(classic, "claude_desktop_config.json"))["fs"].(map[string]interface{})["command"]
	if msixCmd == "npx" {
		t.Error("the MSIX config was not patched")
	}
	if classicCmd != "npx" {
		t.Error("the classic config was patched although the MSIX one exists")
	}
}

// MSIX container present but its config not yet written, and a classic
// config exists: the message must say which one the app reads, why the
// classic one is not patched, and how to use it anyway.
func TestWindowsMSIXWithoutConfigAndClassicPresentExplainsItself(t *testing.T) {
	env, msix, classic := winLayout(t)
	mkdir(t, msix)
	mkdir(t, classic)
	if err := os.WriteFile(filepath.Join(classic, "claude_desktop_config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	lk := claudeDesktopWindowsPath(env)
	if !lk.MSIX || !lk.ClassicExists {
		t.Fatalf("lookup = %+v, want MSIX with an existing classic config", lk)
	}
	msg := missingConfigMessage("claude-desktop", lk)
	for _, want := range []string{
		"Store (MSIX)", filepath.Join(msix, "claude_desktop_config.json"), "open claude-desktop once",
		filepath.Join(classic, "claude_desktop_config.json"), "does not read it", "remove the Store package",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// MSIX alone: no mention of a classic config the user does not have.
func TestWindowsMSIXWithoutConfigAndNoClassicStaysShort(t *testing.T) {
	env, msix, _ := winLayout(t)
	mkdir(t, msix)
	msg := missingConfigMessage("claude-desktop", claudeDesktopWindowsPath(env))
	if !strings.Contains(msg, "open claude-desktop once") || strings.Contains(msg, "does not read it") {
		t.Errorf("unexpected message:\n%s", msg)
	}
}
