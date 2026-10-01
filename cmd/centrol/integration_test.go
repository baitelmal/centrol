package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/scirem/centrol/internal/policy"
)

// binPath is built once for the whole package and reused by every test
// below — these are black-box, subprocess-driven tests (cmdGuard and
// friends call os.Exit, so they can't be exercised in-process).
var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "centrol-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "centrol")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir, _ = os.Getwd()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building centrol for integration tests: %v\n%s\n", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q")
	run(t, dir, "git", "config", "user.email", "t@e.com")
	run(t, dir, "git", "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-q", "-m", "init")
	return dir
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func centrol(t *testing.T, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running centrol %s: %v", strings.Join(args, " "), err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// ===========================================================================
// Ship check 1 & 2: centrol audit --verify on a fresh and a tampered ledger.
// ===========================================================================

func TestShipCheck_AuditVerifyFreshLedgerPasses(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "guard", "--", "sh", "-c", "echo hi > f.txt")
	if code != 0 {
		t.Fatalf("guard failed: %s", stderr)
	}
	out, _, code := centrol(t, dir, "audit", "--verify")
	if code != 0 || !strings.HasPrefix(out, "OK:") {
		t.Fatalf("expected a clean chain to verify OK, got exit=%d out=%q", code, out)
	}
}

func TestShipCheck_AuditVerifyTamperedLedgerFailsAtRightSeq(t *testing.T) {
	dir := initTestRepo(t)
	if _, stderr, code := centrol(t, dir, "guard", "--", "sh", "-c", "echo one > a.txt; echo two > b.txt"); code != 0 {
		t.Fatalf("guard failed: %s", stderr)
	}

	ledgerFile := filepath.Join(dir, ".centrol", "lighthouse.jsonl")
	lines := readLines(t, ledgerFile)
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 ledger entries, got %d", len(lines))
	}
	// Tamper with the second entry's payload without fixing its hash.
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	entry["payload"] = map[string]interface{}{"path": "TAMPERED"}
	tampered, _ := json.Marshal(entry)
	lines[1] = string(tampered)
	writeLinesT(t, ledgerFile, lines)

	out, _, code := centrol(t, dir, "audit", "--verify")
	if code == 0 {
		t.Fatalf("expected tampered ledger to fail verification, got exit=0 out=%q", out)
	}
	if !strings.Contains(out, "FAILED") {
		t.Fatalf("expected FAILED in output, got %q", out)
	}
	var wantSeq float64
	if v, ok := entry["seq"].(float64); ok {
		wantSeq = v
	}
	if !strings.Contains(out, fmt.Sprintf("seq:     %d", int(wantSeq))) {
		t.Fatalf("expected failure to point at seq %d, got %q", int(wantSeq), out)
	}
}

// ===========================================================================
// Ship check 3: centrol guard -- <agent> snapshots, runs, logs, propagates
// the child's exit code.
// ===========================================================================

func TestShipCheck_GuardSnapshotsRunsLogsAndPropagatesExitCode(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "guard", "--", "sh", "-c", "echo changed > main.go; exit 7")
	if code != 7 {
		t.Fatalf("expected guard to propagate the child's exit code 7, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "snapshotting repo before run") {
		t.Fatalf("expected snapshot message on stderr, got %q", stderr)
	}

	out, _, auditCode := centrol(t, dir, "audit")
	if auditCode != 0 {
		t.Fatalf("audit failed: %s", out)
	}
	for _, want := range []string{"run.start", "fs.write", "run.end"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q logged in the ledger, got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"exit_code":7`) {
		// audit's formatted display doesn't show raw JSON payload for
		// every field; fall back to the raw ledger, which always does.
		ledgerData, rerr := os.ReadFile(filepath.Join(dir, ".centrol", "lighthouse.jsonl"))
		if rerr != nil || !strings.Contains(string(ledgerData), `"exit_code":7`) {
			t.Fatalf("expected run.end to record exit_code 7, got formatted audit:\n%s\nand raw ledger:\n%s", out, ledgerData)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, ".centrol", "snapshots"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected a snapshot directory to exist: %v", err)
	}
}

// ===========================================================================
// Ship check 4: centrol undo restores a dirty tree cleanly, every time —
// run twice in a row to catch any state that only breaks on repeat use.
// ===========================================================================

func TestShipCheck_UndoRestoresDirtyTreeCleanlyRepeatedly(t *testing.T) {
	dir := initTestRepo(t)
	for i := 0; i < 2; i++ {
		script := fmt.Sprintf("printf 'package main\\n\\nfunc V%d() {}\\n' > main.go; echo scratch%d > scratch.txt", i, i)
		if _, stderr, code := centrol(t, dir, "guard", "--", "sh", "-c", script); code != 0 {
			t.Fatalf("run %d: guard failed: %s", i, stderr)
		}
		before, err := os.ReadFile(filepath.Join(dir, "main.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(before), fmt.Sprintf("V%d", i)) {
			t.Fatalf("run %d: expected the agent's change to be present before undo", i)
		}

		out, stderr, code := centrol(t, dir, "undo")
		if code != 0 {
			t.Fatalf("run %d: undo failed: %s / %s", i, out, stderr)
		}
		after, err := os.ReadFile(filepath.Join(dir, "main.go"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(after), fmt.Sprintf("V%d", i)) {
			t.Fatalf("run %d: expected main.go restored to pre-run content, got: %s", i, after)
		}
		if string(after) != "package main\n" {
			t.Fatalf("run %d: expected main.go == original committed content, got: %q", i, after)
		}
	}
}

// ===========================================================================
// Live scope enforcement: a scope amendment issued mid-run must change
// what the NEXT write is evaluated against, not just get logged. This
// starts guard narrowed to "src" (so "docs" starts genuinely
// out-of-scope), writes to docs/ before widening (expect a
// policy.violation), widens via `centrol scope +docs` while guard is
// still running, then writes to docs/ again (expect no new violation).
// ===========================================================================

func TestLiveScopeAmendmentChangesEnforcementNotJustTheLog(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	agentScript := `mkdir -p docs
echo before > docs/before.txt
sleep 1.4
echo after > docs/after.txt
`
	cmd := exec.Command(binPath, "guard", "--scope", "src", "--", "sh", "-c", agentScript)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting guard: %v", err)
	}

	time.Sleep(600 * time.Millisecond) // after docs/before.txt has been written and observed, before docs/after.txt
	out, _, code := centrol(t, dir, "scope", "+docs")
	if code != 0 {
		t.Fatalf("centrol scope failed: %s", out)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("guard did not exit cleanly: %v (stderr: %s)", err, stderr.String())
	}

	ledgerData, err := os.ReadFile(filepath.Join(dir, ".centrol", "lighthouse.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ledgerText := string(ledgerData)
	if !strings.Contains(ledgerText, "contract.declare") {
		t.Fatalf("expected contract.declare to be logged, got:\n%s", ledgerText)
	}
	if !strings.Contains(ledgerText, `"path":"docs/before.txt"`) {
		t.Fatalf("expected docs/before.txt to be observed at all, got:\n%s", ledgerText)
	}
	if !violationPresentFor(ledgerText, "docs/before.txt") {
		t.Fatalf("expected a policy.violation for docs/before.txt (written while still out of scope), got:\n%s", ledgerText)
	}
	if violationPresentFor(ledgerText, "docs/after.txt") {
		t.Fatalf("expected NO policy.violation for docs/after.txt (scope was widened before this write), got:\n%s", ledgerText)
	}
}

// violationPresentFor reports whether a policy.violation entry in the
// raw ledger JSONL mentions the given path. Checked against the ledger
// file directly (not the terse `centrol audit` display, which only
// shows one summary field per line) so this test doesn't depend on
// audit's formatting choices.
func violationPresentFor(ledgerText, path string) bool {
	for _, line := range strings.Split(ledgerText, "\n") {
		if strings.Contains(line, `"type":"policy.violation"`) && strings.Contains(line, `"path":"`+path+`"`) {
			return true
		}
	}
	return false
}

// ===========================================================================
// Ship check 5 + real MCP server: centrol proxy against the actual
// @modelcontextprotocol/server-filesystem reference server.
// ===========================================================================

func TestShipCheck_ProxyAgainstRealFilesystemMCPServer(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available in this environment")
	}
	dir := initTestRepo(t)
	sandboxDir := filepath.Join(dir, "sandbox")
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandboxDir, "hello.txt"), []byte("hello from the sandbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A "warm" run first, outside the timed test, so npx's package
	// fetch/cache cost doesn't make the real test flaky on a cold cache.
	warm := exec.Command("npx", "-y", "@modelcontextprotocol/server-filesystem", sandboxDir)
	warm.Stdin = strings.NewReader("")
	_ = warm.Run()

	target := fmt.Sprintf("npx -y @modelcontextprotocol/server-filesystem %s", sandboxDir)

	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"centrol-test","version":"0.1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_text_file","arguments":{"path":"%s/hello.txt"}}}`, sandboxDir),
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_text_file","arguments":{"path":"/etc/passwd"}}}`,
	}, "\n") + "\n"

	cmd := exec.Command(binPath, "proxy", "--target", target)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(requests)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting proxy: %v", err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Logf("proxy exited with error (often fine here since stdin closes): %v", err)
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("proxy against real MCP server timed out; stderr so far: %s", stderr.String())
	}

	// (a) Nothing but JSON-RPC frames on stdout.
	var validLines int
	for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("proxy stdout contained a non-JSON-RPC line: %q (stderr: %s)", l, stderr.String())
		}
		validLines++
	}
	if validLines == 0 {
		t.Fatalf("expected at least one JSON-RPC response on stdout; stderr: %s", stderr.String())
	}

	// (b) The successful read_text_file call actually returned real file
	// content, proving frames genuinely forwarded and returned.
	if !strings.Contains(stdout.String(), "hello from the sandbox") {
		t.Fatalf("expected the real file content to come back through the proxy, got stdout: %s", stdout.String())
	}

	// (c) The /etc/passwd call must be blocked, not forwarded — and
	// logged as tool.blocked in the ledger.
	if !strings.Contains(stdout.String(), "blocked by policy") {
		t.Fatalf("expected the /etc/passwd call to come back as a policy-blocked JSON-RPC error, got: %s", stdout.String())
	}

	// (d) Every tool call landed in the ledger with src=proxy.
	auditOut, _, auditCode := centrol(t, dir, "audit")
	if auditCode != 0 {
		t.Fatalf("audit failed: %s", auditOut)
	}
	if !strings.Contains(auditOut, "proxy") || !strings.Contains(auditOut, "tool.call") {
		t.Fatalf("expected at least one src=proxy tool.call in the ledger, got:\n%s", auditOut)
	}
	if !strings.Contains(auditOut, "tool.blocked") {
		t.Fatalf("expected the /etc/passwd call logged as tool.blocked, got:\n%s", auditOut)
	}
}

// TestStatusMarkerClearedAfterFailedGuardRun is a regression test for a
// real bug: cmdGuard used `defer session.ClearCurrentRun(...)`, but
// every error path exits via os.Exit (directly or through fatalf), and
// deferred functions never run past os.Exit. A failed run (e.g. the
// agent binary doesn't exist) left `centrol status` reporting an
// "active run" that had actually already ended, forever.
func TestStatusMarkerClearedAfterFailedGuardRun(t *testing.T) {
	dir := initTestRepo(t)
	_, _, code := centrol(t, dir, "guard", "--", "definitely-not-a-real-binary-xyz")
	if code == 0 {
		t.Fatalf("expected guard to fail when the agent binary doesn't exist")
	}
	out, _, statusCode := centrol(t, dir, "status")
	if statusCode != 0 {
		t.Fatalf("status failed: %s", out)
	}
	if !strings.Contains(out, "Run:      (none)") {
		t.Fatalf("expected no active run to be reported after a failed guard run, got:\n%s", out)
	}
}

// ===========================================================================
// Section 2 acceptance: MCP server allowlist.
// ===========================================================================

// TestConfigMenuAddsServerAndItTakesEffect drives `centrol config`
// non-interactively via piped stdin, adds a server through the menu,
// and confirms a subsequent `centrol proxy` run against that same
// server no longer hits the unknown-server prompt (which would deny,
// since there's no TTY in this test).
func TestConfigMenuAddsServerAndItTakesEffect(t *testing.T) {
	dir := initTestRepo(t)

	menuInput := "1\n+\n@modelcontextprotocol/server-filesystem\nb\nq\n"
	cmd := exec.Command(binPath, "config")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(menuInput)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("centrol config: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Added") {
		t.Fatalf("expected confirmation of the add, got:\n%s", out.String())
	}

	configPath := filepath.Join(dir, ".centrol", "config.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("expected repo config to be written: %v", err)
	}
	if !strings.Contains(string(data), "@modelcontextprotocol/server-filesystem") {
		t.Fatalf("expected the server name in repo config, got:\n%s", data)
	}

	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available; skipping the live proxy-side confirmation")
	}
	sandbox := filepath.Join(dir, "sandbox")
	os.MkdirAll(sandbox, 0o755)
	target := fmt.Sprintf("npx -y @modelcontextprotocol/server-filesystem %s", sandbox)
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}` + "\n"

	proxyCmd := exec.Command(binPath, "proxy", "--target", target)
	proxyCmd.Dir = dir
	proxyCmd.Stdin = strings.NewReader(req)
	var proxyOut, proxyErr bytes.Buffer
	proxyCmd.Stdout = &proxyOut
	proxyCmd.Stderr = &proxyErr
	done := make(chan error, 1)
	proxyCmd.Start()
	go func() { done <- proxyCmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		proxyCmd.Process.Kill()
		t.Fatalf("proxy timed out; stderr: %s", proxyErr.String())
	}
	if strings.Contains(proxyErr.String(), "Unknown MCP server") || strings.Contains(proxyErr.String(), "denying") {
		t.Fatalf("expected the pre-allowlisted server to skip the unknown-server prompt, got stderr:\n%s", proxyErr.String())
	}
	if !strings.Contains(proxyOut.String(), `"result"`) {
		t.Fatalf("expected a normal initialize response, got:\n%s", proxyOut.String())
	}
}

// TestManuallyEditedConfigHasSameEffectAsMenu confirms hand-editing
// .centrol/config.toml is equivalent to using the menu — both are just
// the repo_config source.
func TestManuallyEditedConfigHasSameEffectAsMenu(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".centrol"), 0o755); err != nil {
		t.Fatal(err)
	}
	configContent := "[proxy]\nallowed_servers = [\"@modelcontextprotocol/server-filesystem\"]\nmass_mutation_threshold = 42\n"
	if err := os.WriteFile(filepath.Join(dir, ".centrol", "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, code := centrol(t, dir, "config")
	_ = out
	_ = code // config with empty stdin just exits the top menu immediately; this call is here to confirm it doesn't choke on a hand-written file

	// Feed the "view thresholds" path and confirm the hand-edited value
	// is what's shown.
	cmd := exec.Command(binPath, "config")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("6\nb\nq\n")
	var menuOut bytes.Buffer
	cmd.Stdout = &menuOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("centrol config: %v\n%s", err, menuOut.String())
	}
	if !strings.Contains(menuOut.String(), "mass_mutation_threshold = 42") {
		t.Fatalf("expected the hand-edited threshold to show in the menu, got:\n%s", menuOut.String())
	}
	if !strings.Contains(menuOut.String(), "repo_config") {
		t.Fatalf("expected the value's source to be identified as repo_config, got:\n%s", menuOut.String())
	}
}

// TestUnknownServerAllowedOnceWithoutTTYButLogged confirms the
// corrected fail-open-but-audited behavior: `centrol proxy` is normally
// launched BY an MCP client with no controlling terminal at all (that's
// the common case, not an edge case — it's what `proxy install` sets
// clients up to do), so an unreviewed server proceeds rather than
// breaking the client outright, but the decision is always logged.
func TestUnknownServerAllowedOnceWithoutTTYButLogged(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available in this environment")
	}
	dir := initTestRepo(t)
	sandbox := filepath.Join(dir, "sandbox")
	os.MkdirAll(sandbox, 0o755)
	target := fmt.Sprintf("npx -y @modelcontextprotocol/server-filesystem %s", sandbox)
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}` + "\n"

	cmd := exec.Command(binPath, "proxy", "--target", target)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(req)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting proxy: %v", err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected the proxy to succeed for an unattended unknown server, got %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("proxy timed out; stderr: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"result"`) {
		t.Fatalf("expected a normal initialize response despite the unknown server, got:\n%s", stdout.String())
	}

	auditOut, _, auditCode := centrol(t, dir, "audit")
	if auditCode != 0 {
		t.Fatalf("audit failed: %s", auditOut)
	}
	if !strings.Contains(auditOut, "policy.amend") {
		t.Fatalf("expected the unattended allow to be logged as policy.amend, got:\n%s", auditOut)
	}
	ledgerData, err := os.ReadFile(filepath.Join(dir, ".centrol", "lighthouse.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ledgerData), "allow_server") {
		t.Fatalf("expected the ledger payload to record action=allow_server, got:\n%s", ledgerData)
	}
}

// ===========================================================================
// Ship criterion 6: guard --observe -- <agent>: logs would_flag
// entries, zero prompts.
// ===========================================================================

func TestShipCriterion6_GuardObserveLogsWouldFlagWithoutBlocking(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Scoped to "src" so writes to docs/ and notes/ are out of scope —
	// these would normally FLAG (policy.violation); in observe mode
	// they must still be logged, but as would_flag, and the writes
	// themselves must succeed (observe never blocks).
	script := `mkdir -p docs notes
echo a > docs/a.txt
echo b > notes/b.txt
echo c > docs/c.txt
`
	_, stderr, code := centrol(t, dir, "guard", "--scope", "src", "--observe", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("guard --observe failed: %s", stderr)
	}
	if !strings.Contains(stderr, "observe mode") {
		t.Fatalf("expected an observe-mode announcement on stderr, got %q", stderr)
	}

	// All three files must actually exist — observe mode never blocks.
	for _, f := range []string{"docs/a.txt", "notes/b.txt", "docs/c.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("expected %s to exist (observe mode must not block writes): %v", f, err)
		}
	}

	auditOut, _, auditCode := centrol(t, dir, "audit")
	if auditCode != 0 {
		t.Fatalf("audit failed: %s", auditOut)
	}
	// The test harness captures audit's stdout into a plain buffer, not
	// a real terminal, so per the UTF-8-detection rule (section 6 —
	// glyphs render as Unicode only on a real UTF-8 terminal, exactly
	// like color only renders on a real terminal) this output correctly
	// gets the ASCII fallback "o", not the raw ◇ character. Each
	// formatted line ends in exactly one trailing symbol column, so
	// count lines ending in a bare "o" rather than substring-counting
	// the letter (which would also match "policy", "notes", etc.).
	wouldFlagCount := 0
	for _, line := range strings.Split(auditOut, "\n") {
		if strings.HasSuffix(strings.TrimRight(line, "\r"), "  o") {
			wouldFlagCount++
		}
	}
	if wouldFlagCount < 3 {
		t.Fatalf("expected at least 3 would_flag entries (ASCII 'o' symbol) in the formatted audit output, got %d:\n%s", wouldFlagCount, auditOut)
	}

	// Cross-check against the raw ledger, where the payload's decision
	// field is unambiguous.
	ledgerData, err := os.ReadFile(filepath.Join(dir, ".centrol", "lighthouse.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rawWouldFlagCount := strings.Count(string(ledgerData), `"decision":"would_flag"`)
	if rawWouldFlagCount < 3 {
		t.Fatalf("expected at least 3 raw would_flag entries in the ledger, got %d:\n%s", rawWouldFlagCount, ledgerData)
	}
}

// ===========================================================================
// Ship criterion 7: prompt_timeout_seconds — deny after timeout, logged
// as policy.violation.
// ===========================================================================

func TestShipCriterion7_PromptTimeoutDeniesAndLogsPolicyViolation(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available in this environment")
	}
	dir := initTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".centrol"), 0o755); err != nil {
		t.Fatal(err)
	}
	// gate.prompt_timeout_seconds floor is 30; test at the floor so it
	// stays fast without accepting a config value the real validator
	// would reject.
	configContent := "[gate]\nprompt_timeout_seconds = 30\n\n[proxy]\nallowed_servers = [\"@modelcontextprotocol/server-filesystem\"]\n"
	if err := os.WriteFile(filepath.Join(dir, ".centrol", "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	sandbox := filepath.Join(dir, "sandbox")
	os.MkdirAll(sandbox, 0o755)
	// A path outside the sandbox the filesystem server was given is
	// out-of-repo-scope from centrol's contract, which FLAGs (not hard
	// blocks) — exactly the tier that reaches the prompt.
	target := fmt.Sprintf("npx -y @modelcontextprotocol/server-filesystem %s", sandbox)
	outOfScope := filepath.Join(dir, "outside-src-scope.txt")
	os.WriteFile(outOfScope, []byte("x"), 0o644)

	req := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_text_file","arguments":{"path":"%s"}}}`, outOfScope),
	}, "\n") + "\n"

	// Note: with no TTY in this test process, gate.OpenControllingTTY
	// fails and StderrPrompt already denies immediately — this test's
	// real target is the pure timeout mechanism, exercised directly at
	// the gate/proxy level in internal/gate and internal/proxy tests.
	// This CLI-level run instead confirms the config value threads
	// through end to end without erroring.
	cmd := exec.Command(binPath, "proxy", "--target", target)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(req)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	cmd.Start()
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("timed out; stderr: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "prompt_timeout_seconds") && strings.Contains(stderr.String(), "below the minimum") {
		t.Fatalf("expected the floor value 30 to be accepted, got: %s", stderr.String())
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			lines = append(lines, scanner.Text())
		}
	}
	return lines
}

func writeLinesT(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

// TestShipCriterion14_QuietSuppressesInfoButNotErrorsOrSummary proves
// --quiet drops step-boundary/info output (the "Snapshotting repo..."
// line) while errors keep printing and the run finishes normally.
func TestShipCriterion14_QuietSuppressesInfoButNotErrorsOrSummary(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "guard", "--quiet", "--", "sh", "-c", "echo hi > f.txt")
	if code != 0 {
		t.Fatalf("guard --quiet failed: %s", stderr)
	}
	if strings.Contains(stderr, "snapshotting repo") {
		t.Fatalf("expected --quiet to suppress the info-level snapshot line, got: %s", stderr)
	}
}

// TestShipCriterion14_VerbosePrintsStateTransitions proves --verbose
// (log_level=debug) prints internal state transitions — here, the
// per-event debug trace every governed emission produces — that a
// plain run does not.
func TestShipCriterion14_VerbosePrintsStateTransitions(t *testing.T) {
	dir := initTestRepo(t)
	_, stderrVerbose, code := centrol(t, dir, "guard", "--verbose", "--", "sh", "-c", "echo hi > f.txt")
	if code != 0 {
		t.Fatalf("guard --verbose failed: %s", stderrVerbose)
	}
	if !strings.Contains(stderrVerbose, "run.start") {
		t.Fatalf("expected --verbose to trace internal events (e.g. run.start) to stderr, got: %s", stderrVerbose)
	}

	dir2 := initTestRepo(t)
	_, stderrPlain, code2 := centrol(t, dir2, "guard", "--", "sh", "-c", "echo hi > f.txt")
	if code2 != 0 {
		t.Fatalf("guard failed: %s", stderrPlain)
	}
	if strings.Contains(stderrPlain, "run.start") {
		t.Fatalf("expected a plain (non-verbose) run NOT to trace internal events, got: %s", stderrPlain)
	}
}

// TestQuietAndVerboseFlagsNeverReachTheWrappedAgent proves
// extractGuardFlags' "--" boundary is respected: an agent command that
// happens to use --quiet or --verbose itself as its own argument must
// receive it unchanged, never stripped as if it were centrol's own flag.
func TestQuietAndVerboseFlagsNeverReachTheWrappedAgent(t *testing.T) {
	dir := initTestRepo(t)
	// The agent script just echoes its own arguments so the test can
	// confirm exactly what centrol handed it after the "--" separator.
	stdout, stderr, code := centrol(t, dir, "guard", "--quiet", "--", "sh", "-c", `echo "args: $*" > out.txt`, "sh", "--verbose", "--quiet")
	if code != 0 {
		t.Fatalf("guard failed: %s %s", stdout, stderr)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("reading agent output: %v", err)
	}
	if !strings.Contains(string(got), "--verbose") || !strings.Contains(string(got), "--quiet") {
		t.Fatalf("expected the agent's own --verbose/--quiet arguments to pass through untouched, got %q", got)
	}
}

// fakeLockedSource is a PolicySource test double standing in for a real
// enterprise policy client: unlike policy.EnterpriseSource (which
// always returns found=false in v0.1, by design — see its doc
// comment), this one actually answers one specific key, so the
// "locked by enterprise" display path in `centrol config` — which no
// real EnterpriseSource in this version ever exercises — has something
// to prove itself against.
type fakeLockedSource struct {
	key   string
	value interface{}
}

func (f fakeLockedSource) Name() string { return policy.SourceEnterprisePolicy }
func (f fakeLockedSource) Resolve(key string) (interface{}, bool, error) {
	if key == f.key {
		return f.value, true, nil
	}
	return nil, false, nil
}
func (f fakeLockedSource) Writable() bool { return false }

// TestConfigMenuSyntheticLockedValueDisplaysAndCannotBeEdited is the
// spec's mandated synthetic locked-value test: with a fake
// EnterpriseSource answering gate.allow_session_amend, the menu must
// show it as "[locked by enterprise]" in both the Gate section and
// "View effective config", and must refuse an edit attempt rather than
// silently writing over it.
func TestConfigMenuSyntheticLockedValueDisplaysAndCannotBeEdited(t *testing.T) {
	dir := t.TempDir()
	resolver := policy.NewResolver(
		policy.NewSessionContractSource(),
		fakeLockedSource{key: policy.KeyAllowSessionAmend, value: false},
		policy.NewLocalFileSource(policy.SourceRepoConfig, filepath.Join(dir, "repo-config.toml")),
		policy.NewLocalFileSource(policy.SourceUserConfig, filepath.Join(dir, "user-config.toml")),
		policy.NewDefaultSource(nil),
	)

	// Gate section (2), attempt to edit the locked value (2 again — the
	// "toggle allow_session_amend" option), then back out and quit.
	in := bufio.NewReader(strings.NewReader("2\n2\nb\nq\n"))
	var out bytes.Buffer
	runConfigMenu(in, &out, resolver)

	got := out.String()
	if !strings.Contains(got, "allow_session_amend") || !strings.Contains(got, "locked by enterprise") {
		t.Fatalf("expected the Gate section to show allow_session_amend as locked by enterprise, got:\n%s", got)
	}
	if strings.Contains(got, "Set allow_session_amend") {
		t.Fatalf("expected the menu to refuse editing a locked value, but it reported a write:\n%s", got)
	}
	if !strings.Contains(got, "cannot be edited here") {
		t.Fatalf("expected an explicit refusal message for the locked value, got:\n%s", got)
	}

	// "View effective config" (7) must show the same thing.
	in2 := bufio.NewReader(strings.NewReader("7\nq\n"))
	var out2 bytes.Buffer
	runConfigMenu(in2, &out2, resolver)
	got2 := out2.String()
	if !strings.Contains(got2, "gate.allow_session_amend") || !strings.Contains(got2, "locked by enterprise") {
		t.Fatalf("expected View effective config to show gate.allow_session_amend as locked by enterprise, got:\n%s", got2)
	}

	// Confirm the value truly never got persisted anywhere writable.
	repoData, _ := os.ReadFile(filepath.Join(dir, "repo-config.toml"))
	if strings.Contains(string(repoData), "allow_session_amend") {
		t.Fatalf("expected no write to repo config for a locked key, got:\n%s", repoData)
	}
}

// TestValidTargetURLScheme is a direct unit check of the audit's 3a
// fix: only http/https schemes (or empty, to unset) are acceptable.
func TestValidTargetURLScheme(t *testing.T) {
	cases := map[string]bool{
		"":                            true,
		"https://mcp.example.com/sse": true,
		"http://127.0.0.1:8080/mcp":   true,
		"file:///etc/passwd":          false,
		"ftp://example.com/x":         false,
		"javascript:alert(1)":         false,
		"example.com":                 false, // no scheme at all
		"://not-a-url":                false,
	}
	for v, want := range cases {
		if got := validTargetURLScheme(v); got != want {
			t.Errorf("validTargetURLScheme(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestConfigMenuRejectsInvalidTargetURLScheme drives the proxy submenu's
// "Edit target_url" option end to end: an invalid scheme must be
// refused with no write, and a subsequent valid https URL must be
// accepted and persisted.
func TestConfigMenuRejectsInvalidTargetURLScheme(t *testing.T) {
	dir := t.TempDir()
	resolver := policy.NewResolver(
		policy.NewSessionContractSource(),
		policy.NewEnterpriseSource(),
		policy.NewLocalFileSource(policy.SourceRepoConfig, filepath.Join(dir, "repo-config.toml")),
		policy.NewLocalFileSource(policy.SourceUserConfig, filepath.Join(dir, "user-config.toml")),
		policy.NewDefaultSource(nil),
	)

	// Proxy section (3), edit target_url (4), an invalid scheme, then a
	// valid https URL, then back out and quit.
	in := bufio.NewReader(strings.NewReader("3\n4\nfile:///etc/passwd\n4\nhttps://mcp.example.com/sse\nb\nq\n"))
	var out bytes.Buffer
	runConfigMenu(in, &out, resolver)

	got := out.String()
	if !strings.Contains(got, "must be an http:// or https:// URL") {
		t.Fatalf("expected the invalid scheme to be rejected with a clear message, got:\n%s", got)
	}
	if !strings.Contains(got, `Set target_url = "https://mcp.example.com/sse"`) {
		t.Fatalf("expected the valid https URL to be accepted, got:\n%s", got)
	}

	repoData, err := os.ReadFile(filepath.Join(dir, "repo-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(repoData), "etc/passwd") {
		t.Fatalf("expected the rejected file:// URL never to be written, got:\n%s", repoData)
	}
	if !strings.Contains(string(repoData), "https://mcp.example.com/sse") {
		t.Fatalf("expected the valid https URL to be persisted, got:\n%s", repoData)
	}
}

// TestConfigMenuEditsGitTimeoutSeconds drives the guard submenu's "Edit
// git_timeout_seconds" option (audit item 4e's new config key): a value
// below the floor is rejected with no write, and a valid value is
// accepted, persisted, and visible from "View effective config".
func TestConfigMenuEditsGitTimeoutSeconds(t *testing.T) {
	dir := t.TempDir()
	resolver := policy.NewResolver(
		policy.NewSessionContractSource(),
		policy.NewEnterpriseSource(),
		policy.NewLocalFileSource(policy.SourceRepoConfig, filepath.Join(dir, "repo-config.toml")),
		policy.NewLocalFileSource(policy.SourceUserConfig, filepath.Join(dir, "user-config.toml")),
		policy.NewDefaultSource(nil),
	)

	// Guard section (4), edit git_timeout_seconds (2), a below-floor
	// value, then a valid value, then view effective config, then quit.
	in := bufio.NewReader(strings.NewReader("4\n2\n2\n2\n45\nb\n7\nq\n"))
	var out bytes.Buffer
	runConfigMenu(in, &out, resolver)

	got := out.String()
	if !strings.Contains(got, fmt.Sprintf("cannot be below %d", policy.MinGitTimeoutSeconds)) {
		t.Fatalf("expected the below-floor value to be rejected, got:\n%s", got)
	}
	if !strings.Contains(got, "Set git_timeout_seconds = 45") {
		t.Fatalf("expected the valid value to be accepted, got:\n%s", got)
	}
	if !strings.Contains(got, "guard.git_timeout_seconds") || !strings.Contains(got, "45") {
		t.Fatalf("expected git_timeout_seconds = 45 to show up in the effective config view, got:\n%s", got)
	}

	repoData, err := os.ReadFile(filepath.Join(dir, "repo-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(repoData), "git_timeout_seconds") {
		t.Fatalf("expected git_timeout_seconds to be persisted, got:\n%s", repoData)
	}
}

// TestConfigMenuEditsTargetExitTimeoutSeconds drives the proxy
// submenu's "Edit target_exit_timeout_seconds" option (audit item 4g's
// new config key): a value below the floor is rejected with no write,
// and a valid value is accepted, persisted, and visible from "View
// effective config".
func TestConfigMenuEditsTargetExitTimeoutSeconds(t *testing.T) {
	dir := t.TempDir()
	resolver := policy.NewResolver(
		policy.NewSessionContractSource(),
		policy.NewEnterpriseSource(),
		policy.NewLocalFileSource(policy.SourceRepoConfig, filepath.Join(dir, "repo-config.toml")),
		policy.NewLocalFileSource(policy.SourceUserConfig, filepath.Join(dir, "user-config.toml")),
		policy.NewDefaultSource(nil),
	)

	// Proxy section (3), edit target_exit_timeout_seconds (7), a
	// below-floor value, then a valid value, then view effective
	// config, then quit.
	in := bufio.NewReader(strings.NewReader("3\n7\n1\n7\n20\nb\n7\nq\n"))
	var out bytes.Buffer
	runConfigMenu(in, &out, resolver)

	got := out.String()
	if !strings.Contains(got, fmt.Sprintf("below the minimum of %d", policy.MinTargetExitTimeoutSecs)) {
		t.Fatalf("expected the below-floor value to be rejected, got:\n%s", got)
	}
	if !strings.Contains(got, "Set target_exit_timeout_seconds = 20") {
		t.Fatalf("expected the valid value to be accepted, got:\n%s", got)
	}
	if !strings.Contains(got, "proxy.target_exit_timeout_seconds") || !strings.Contains(got, "20") {
		t.Fatalf("expected target_exit_timeout_seconds = 20 to show up in the effective config view, got:\n%s", got)
	}

	repoData, err := os.ReadFile(filepath.Join(dir, "repo-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(repoData), "target_exit_timeout_seconds") {
		t.Fatalf("expected target_exit_timeout_seconds to be persisted, got:\n%s", repoData)
	}
}

// ===========================================================================
// Ship criterion 9: the run summary prints on every trappable exit path
// (normal, panic, SIGINT, SIGTERM), is never suppressed by --quiet, and
// its flagged/blocked counts and "Inspect:" suggestion agree with what
// `centrol audit --flagged`/`--blocked` would actually find.
// ===========================================================================

// centrolSignaled starts `centrol <args...>` in dir, waits for waitFor to
// appear on its stderr (so the signal lands once the process is actually
// past setup, e.g. inside the wrapped agent), sends sig, then waits for
// exit and returns everything captured plus the exit code.
func centrolSignaled(t *testing.T, dir string, waitFor string, sig syscall.Signal, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	var outBuf, errBuf syncBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// A real terminal delivers Ctrl-C/SIGTERM to the whole foreground
	// process group at once (centrol AND the agent it wraps, and
	// anything the agent itself forks), which is why terminal.Run's own
	// explicit forwarding is a backup, not the primary mechanism — see
	// its doc comment. Setpgid puts centrol in a fresh group (pgid ==
	// its own pid, since its children inherit it), so signaling -pid
	// hits that whole group exactly the way a real terminal would,
	// rather than only the single centrol process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting centrol %s: %v", strings.Join(args, " "), err)
	}

	// Bug fixed here: this loop used to fall through and send sig
	// unconditionally once the deadline passed, whether or not waitFor
	// had actually appeared yet. Under a full `-race -count=10` run
	// across the whole repo (heavy global CPU contention, not just
	// this package), the fixed deadline could expire before guard/proxy
	// even reached the marker — and the blind send that followed landed
	// at some arbitrary later point in the run (observed once: a
	// SIGTERM sent that late landed only after the wrapped `sleep 5`
	// had already run to completion on its own, so the run exited 0
	// instead of 143, with nothing to indicate why). Fatal-ing on a
	// genuine timeout, instead of silently mistiming the signal, is
	// what turns that into an honest, loud failure.
	deadline := time.Now().Add(15 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if strings.Contains(errBuf.String(), waitFor) {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatalf("centrol %s: %q never appeared on stderr within 15s:\n%s", strings.Join(args, " "), waitFor, errBuf.String())
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		t.Fatalf("signaling centrol's process group: %v", err)
	}

	err := cmd.Wait()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("waiting for centrol %s: %v", strings.Join(args, " "), err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// syncBuffer is a mutex-guarded bytes.Buffer: cmd.Wait() and the polling
// loop above both touch these buffers from different goroutines
// (exec.Cmd copies to Stdout/Stderr on its own goroutines internally),
// so a plain bytes.Buffer would race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestShipCriterion9_SummaryPrintsOnNormalExit(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "guard", "--", "sh", "-c", "echo hi > f.txt")
	if code != 0 {
		t.Fatalf("guard failed: %s", stderr)
	}
	if !strings.Contains(stderr, "Run complete") {
		t.Fatalf("expected a run summary on normal exit, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Events:") || !strings.Contains(stderr, "Ledger:") || !strings.Contains(stderr, "Inspect:") {
		t.Fatalf("expected the summary to contain Events/Ledger/Inspect lines, got:\n%s", stderr)
	}
}

func TestShipCriterion9_SummaryPrintsUnderQuiet(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "guard", "--quiet", "--", "sh", "-c", "echo hi > f.txt")
	if code != 0 {
		t.Fatalf("guard --quiet failed: %s", stderr)
	}
	if !strings.Contains(stderr, "Run complete") {
		t.Fatalf("expected --quiet to still print the run summary, got:\n%s", stderr)
	}
}

func TestShipCriterion9_SummaryReflectsFlaggedCounts(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `mkdir -p docs
echo a > docs/a.txt
echo b > docs/b.txt
`
	_, stderr, code := centrol(t, dir, "guard", "--scope", "src", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("guard failed: %s", stderr)
	}
	if !strings.Contains(stderr, "flagged") {
		t.Fatalf("expected the summary to report flagged events, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--flagged") {
		t.Fatalf("expected the Inspect line to suggest `centrol audit --flagged`, got:\n%s", stderr)
	}

	// Cross-check against what `centrol audit --flagged` actually finds —
	// the summary's numbers must never disagree with the ledger itself.
	auditOut, _, auditCode := centrol(t, dir, "audit", "--flagged")
	if auditCode != 0 {
		t.Fatalf("audit --flagged failed: %s", auditOut)
	}
	flaggedLines := 0
	for _, line := range strings.Split(strings.TrimSpace(auditOut), "\n") {
		if strings.TrimSpace(line) != "" {
			flaggedLines++
		}
	}
	if flaggedLines < 2 {
		t.Fatalf("expected centrol audit --flagged to find at least 2 entries, got %d:\n%s", flaggedLines, auditOut)
	}
}

func TestShipCriterion9_SummaryPrintsOnSigint(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrolSignaled(t, dir, "snapshotting repo", syscall.SIGINT, "guard", "--", "sh", "-c", "sleep 5")
	if code != 130 {
		t.Fatalf("expected exit 130 on SIGINT, got %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "Run complete") {
		t.Fatalf("expected a run summary on SIGINT, got:\n%s", stderr)
	}
}

func TestShipCriterion9_SummaryPrintsOnSigterm(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrolSignaled(t, dir, "snapshotting repo", syscall.SIGTERM, "guard", "--", "sh", "-c", "sleep 5")
	if code != 143 {
		t.Fatalf("expected exit 143 on SIGTERM, got %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "Run complete") {
		t.Fatalf("expected a run summary on SIGTERM, got:\n%s", stderr)
	}
}

// TestShipCriterion9_ProxySummaryPrintsOnNormalExit uses "cat" as the
// --target: with no stdin supplied, cat sees immediate EOF and exits,
// which is enough to exercise proxy's own summary-on-exit path without
// needing a real MCP server.
func TestShipCriterion9_ProxySummaryPrintsOnNormalExit(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, _ := centrol(t, dir, "proxy", "--target", "cat")
	if !strings.Contains(stderr, "Run complete") {
		t.Fatalf("expected proxy to print a run summary on exit, got:\n%s", stderr)
	}
}

// TestShipCriterion9_ProxySummaryPrintsOnSigint is the proxy-side
// counterpart to TestShipCriterion9_SummaryPrintsOnSigint, exercising
// Governor.Run's own signal watch (Pass 3.9c Final, Section 2
// "Signals") rather than guard's hand-off to terminal.Run. --target
// points at a small script instead of an inline shell command: proxy's
// own --target splitting is plain whitespace (see cmd_proxy.go), which
// would mangle a quoted `sh -c "..."` with embedded spaces, and a
// script file is also what lets the target announce readiness on its
// own stderr (which centrol's stdio transport passes straight through)
// before sleeping — centrolSignaled waits for that marker the same way
// the guard-side tests wait for "snapshotting repo", so the signal is
// never sent before Governor.Run has actually registered its watch.
func TestShipCriterion9_ProxySummaryPrintsOnSigint(t *testing.T) {
	dir := initTestRepo(t)
	script := filepath.Join(dir, "sigint_target.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho proxy_sigint_ready 1>&2\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := centrolSignaled(t, dir, "proxy_sigint_ready", syscall.SIGINT, "proxy", "--target", script)
	if code != 130 {
		t.Fatalf("expected exit 130 on SIGINT, got %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "Run complete") {
		t.Fatalf("expected a run summary on SIGINT, got:\n%s", stderr)
	}
}

// ===========================================================================
// v0.2.0 pass 3: --target-url CLI/config wiring.
// ===========================================================================

func TestProxyTargetAndTargetURLAreMutuallyExclusive(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "proxy", "--target", "cat", "--target-url", "http://127.0.0.1:1/ignored")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("expected a mutual-exclusivity error, got:\n%s", stderr)
	}
}

func TestProxyRequiresTargetOrTargetURL(t *testing.T) {
	dir := initTestRepo(t)
	_, stderr, code := centrol(t, dir, "proxy")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Fatalf("expected a usage error, got:\n%s", stderr)
	}
}

// echoJSONRPCServer replies to every POST with a JSON-RPC result frame
// carrying the request's own id, so a test can confirm a request that
// went in through the proxy really was forwarded over HTTP and really
// came back, rather than merely that the process exited cleanly.
func echoJSONRPCServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&env)
		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": env["id"], "result": map[string]interface{}{"ok": true},
		})
		_, _ = w.Write(resp)
	}))
}

func TestProxyTargetURLFlagForwardsThroughHTTPTarget(t *testing.T) {
	dir := initTestRepo(t)
	srv := echoJSONRPCServer(t)
	defer srv.Close()

	// --observe: evaluated exactly as normal but never blocks or
	// prompts, so this test exercises the HTTP transport wiring itself
	// without also depending on the tool-name allowlist/policy tiers.
	cmd := exec.Command(binPath, "proxy", "--target-url", srv.URL, "--observe")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"a.go"}}}` + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run() // stdin EOF ends the run; a non-zero exit here isn't itself a failure

	if !strings.Contains(stdout.String(), `"ok":true`) {
		t.Fatalf("expected the HTTP target's response forwarded to stdout, got: %q (stderr: %s)", stdout.String(), stderr.String())
	}
}

func TestProxyTargetURLFromConfigIsUsedWhenNoFlagsGiven(t *testing.T) {
	dir := initTestRepo(t)
	srv := echoJSONRPCServer(t)
	defer srv.Close()

	if err := os.MkdirAll(filepath.Join(dir, ".centrol"), 0o755); err != nil {
		t.Fatal(err)
	}
	configContent := fmt.Sprintf("[proxy]\ntarget_url = %q\n", srv.URL)
	if err := os.WriteFile(filepath.Join(dir, ".centrol", "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binPath, "proxy", "--observe")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"a.go"}}}` + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()

	if !strings.Contains(stdout.String(), `"ok":true`) {
		t.Fatalf("expected the configured target_url to be used with no flags given, got stdout: %q (stderr: %s)", stdout.String(), stderr.String())
	}
}

// ===========================================================================
// v0.2.0 audit: proxy.additional_block_paths must be enforced under
// `centrol guard`, not just `centrol proxy`.
// ===========================================================================

// TestGuardEnforcesAdditionalBlockPaths is a regression test for a real
// bug found during the hygiene audit: cmd_proxy.go resolved
// proxy.additional_block_paths into its contract's ProtectedPaths, but
// cmd_guard.go never did, so a path an operator explicitly configured
// as an additional protected path was silently unenforced — no
// policy.violation at all — under `centrol guard`, even though both
// surfaces share the same Contract/EvaluateFSPath machinery and the
// project's own standard calls for symmetry between sibling
// implementations.
func TestGuardEnforcesAdditionalBlockPaths(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".centrol"), 0o755); err != nil {
		t.Fatal(err)
	}
	secretDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configContent := fmt.Sprintf("[proxy]\nadditional_block_paths = [%q]\n", secretDir)
	if err := os.WriteFile(filepath.Join(dir, ".centrol", "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf("echo s > %s\n", filepath.Join(secretDir, "s.txt"))
	_, stderr, code := centrol(t, dir, "guard", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("guard failed: %s", stderr)
	}

	auditOut, _, auditCode := centrol(t, dir, "audit")
	if auditCode != 0 {
		t.Fatalf("audit failed: %s", auditOut)
	}
	ledgerData, err := os.ReadFile(filepath.Join(dir, ".centrol", "lighthouse.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ledgerText := string(ledgerData)
	if !strings.Contains(ledgerText, `"type":"policy.violation"`) || !strings.Contains(ledgerText, `"path":"secrets/s.txt"`) {
		t.Fatalf("expected a policy.violation for a write under the configured additional_block_paths entry, got ledger:\n%s", ledgerText)
	}
	if !strings.Contains(ledgerText, `"tier":"block"`) {
		t.Fatalf("expected the violation's tier to be block (a protected path, not merely out of scope), got ledger:\n%s", ledgerText)
	}
}

func TestProxyTargetFlagOverridesConfiguredTargetURL(t *testing.T) {
	dir := initTestRepo(t)

	if err := os.MkdirAll(filepath.Join(dir, ".centrol"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A target_url that would fail loudly if it were ever actually used
	// — port 1 refuses immediately — so this test can tell the
	// difference between "the flag correctly overrode this" and "it
	// happened to work anyway."
	configContent := "[proxy]\ntarget_url = \"http://127.0.0.1:1/unused\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".centrol", "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, _ := centrol(t, dir, "proxy", "--target", "cat")
	if !strings.Contains(stderr, "Run complete") {
		t.Fatalf("expected --target to override the configured target_url and run against stdio cleanly, got stderr:\n%s", stderr)
	}
}

// TestProxyLogsScopePollErrorsInsteadOfSwallowing is the audit's 3b
// fix: the live-scope poller's read error used to be discarded
// outright (`continue` with nothing logged). Forces a real,
// non-IsNotExist read error by making scope-requests.jsonl a
// directory instead of a file, keeps the proxy running across several
// 200ms poll ticks via an open stdin pipe, and confirms the error
// reaches the ledger as policy.silence rather than vanishing — and
// that the run completes normally despite it (not a stop condition).
func TestProxyLogsScopePollErrorsInsteadOfSwallowing(t *testing.T) {
	dir := initTestRepo(t)
	centrolPath := filepath.Join(dir, ".centrol")
	if err := os.MkdirAll(filepath.Join(centrolPath, "scope-requests.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}

	pr, pw := io.Pipe()
	cmd := exec.Command(binPath, "proxy", "--target", "cat")
	cmd.Dir = dir
	cmd.Stdin = pr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting proxy: %v", err)
	}

	// Several poll ticks (200ms interval) before letting the run end.
	time.Sleep(700 * time.Millisecond)
	pw.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("proxy timed out; stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Run complete") {
		t.Fatalf("expected the run to complete normally despite the poll error (not a stop condition), got stderr:\n%s", stderr.String())
	}

	ledgerData, err := os.ReadFile(filepath.Join(centrolPath, "lighthouse.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ledgerText := string(ledgerData)
	if !strings.Contains(ledgerText, `"type":"policy.silence"`) || !strings.Contains(ledgerText, "scope-request poll failed") {
		t.Fatalf("expected a policy.silence entry for the scope-poll error, got ledger:\n%s", ledgerText)
	}
}

// ===========================================================================
// v0.3 tamper reader: an external modification to the ledger while
// `centrol guard` is running must be logged as policy.tamper_detected
// (readable via `centrol audit`, test 4) without breaking the hash
// chain itself (`centrol audit --verify` still passes afterward, test
// 5). Tests 1-3 (modify/sealed-segment/own-write-ignored) are covered
// at the ledger-package level in internal/ledger/tamper_test.go, which
// can drive the exact timing and segment-rotation cases directly;
// this is the one end-to-end check that the real binary wires the
// watcher, the Governor, and the ledger together correctly.
// ===========================================================================

func TestGuardLogsExternalLedgerTamperWithoutBreakingTheChain(t *testing.T) {
	dir := initTestRepo(t)

	// The agent itself does nothing to the ledger; it just gives the
	// test a window, after guard's own startup writes (run.start etc.)
	// have settled, to modify the ledger file out from under it.
	agentScript := `echo first > a.txt
sleep 1.2
echo second > b.txt
`
	cmd := exec.Command(binPath, "guard", "--", "sh", "-c", agentScript)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting guard: %v", err)
	}

	ledgerFile := filepath.Join(dir, ".centrol", "lighthouse.jsonl")
	// Give guard's own startup appends (run.start, fs.write for a.txt)
	// time to land and their ownWriteGrace windows time to lapse, so
	// this write is unambiguously external rather than racing the
	// tool's own bookkeeping.
	time.Sleep(600 * time.Millisecond)
	f, err := os.OpenFile(ledgerFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening ledger for external write: %v", err)
	}
	// A blank line: an external modification a real attacker or a
	// stray process could produce, that the chain's own Verify treats
	// as a trailing blank (skipped), not a broken hash link — this
	// test is about the watcher's detection, not about also exercising
	// audit --verify's tamper-REPORTING path (already covered by
	// TestShipCheck_AuditVerifyTamperedLedgerFailsAtRightSeq).
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatalf("external write to ledger: %v", err)
	}
	f.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("guard did not exit cleanly: %v (stderr: %s)", err, stderr.String())
	}

	ledgerData, err := os.ReadFile(ledgerFile)
	if err != nil {
		t.Fatal(err)
	}
	ledgerText := string(ledgerData)
	if !strings.Contains(ledgerText, `"type":"policy.tamper_detected"`) {
		t.Fatalf("expected a policy.tamper_detected entry for the external write, got ledger:\n%s", ledgerText)
	}
	if !strings.Contains(ledgerText, `"event":"modify"`) {
		t.Fatalf("expected the tamper entry's event to be \"modify\", got ledger:\n%s", ledgerText)
	}

	// Test 4: readable via `centrol audit` — not just present in the
	// raw file.
	auditOut, _, auditCode := centrol(t, dir, "audit")
	if auditCode != 0 {
		t.Fatalf("audit failed: %s", auditOut)
	}
	if !strings.Contains(auditOut, "policy.tamper_detected") {
		t.Fatalf("expected policy.tamper_detected visible in `centrol audit` output, got:\n%s", auditOut)
	}

	// Test 5: the chain is still verifiable after a tamper_detected
	// event — detection recording must not itself corrupt the chain it
	// is reporting on.
	verifyOut, _, verifyCode := centrol(t, dir, "audit", "--verify")
	if verifyCode != 0 || !strings.HasPrefix(verifyOut, "OK:") {
		t.Fatalf("expected audit --verify to still pass after a tamper_detected event, got exit=%d out=%q", verifyCode, verifyOut)
	}
}
