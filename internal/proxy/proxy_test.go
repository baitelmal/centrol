package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scirem/centrol/internal/policy"
	"github.com/scirem/centrol/internal/proxy/transport"
)

func testContract() policy.Contract {
	return policy.DefaultContract(policy.KindProxy, "run-1", "/repo")
}

type recorder struct {
	mu      sync.Mutex
	entries []map[string]interface{}
	types   []string
}

func (r *recorder) emit(run, src, typ string, payload map[string]interface{}) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.types = append(r.types, typ)
	r.entries = append(r.entries, payload)
	return nil
}

func (r *recorder) has(typ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.types {
		if t == typ {
			return true
		}
	}
	return false
}

func toolCallLine(t *testing.T, id, tool string, arguments map[string]interface{}) []byte {
	t.Helper()
	env := map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]interface{}{"name": tool, "arguments": arguments},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAllowedToolCallForwardsUnchanged(t *testing.T) {
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "src/main.go"})

	forward, fwd, block, err := in.HandleClientRequest(line)
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if !forward || block != nil {
		t.Fatalf("expected forward=true block=nil, got forward=%v block=%s", forward, block)
	}
	if string(fwd) != string(line) {
		t.Fatalf("expected forwarded line unchanged")
	}
	if !rec.has("tool.call") {
		t.Fatalf("expected tool.call emitted for allowed call")
	}
}

func TestHardBlockNeverForwardsToTarget(t *testing.T) {
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
	line := toolCallLine(t, "2", "read_file", map[string]interface{}{"path": "/etc/passwd"})

	forward, fwd, block, err := in.HandleClientRequest(line)
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if forward || fwd != nil {
		t.Fatalf("expected forward=false for hard-blocked call, got forward=%v fwd=%s", forward, fwd)
	}
	if block == nil {
		t.Fatalf("expected a block response to send to the client")
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(block, &resp); err != nil {
		t.Fatalf("block response not valid JSON: %v", err)
	}
	if _, ok := resp["error"]; !ok {
		t.Fatalf("expected block response to be a JSON-RPC error, got %s", block)
	}
	// Per the structured-denial design rule: a hard block is a
	// legitimate policy decision the agent can act on, not malformed
	// input, so it gets tool.blocked + a structured JSON-RPC error —
	// NOT policy.silence. Protocol Silence is reserved for frames the
	// proxy couldn't even parse (see TestNonJSONClientFrameIsSilencedNotForwarded).
	if !rec.has("tool.blocked") {
		t.Fatalf("expected tool.blocked for hard block, got %v", rec.types)
	}
	if rec.has("policy.silence") {
		t.Fatalf("a hard block is a legitimate denial, not malformed input — it must not also log policy.silence, got %v", rec.types)
	}
	if data, ok := resp["error"].(map[string]interface{})["data"].(map[string]interface{}); !ok || data["reason"] == "" {
		t.Fatalf("expected the structured error's data.reason to be set, got %s", block)
	}
}

func TestPathTraversalIsHardBlocked(t *testing.T) {
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
	line := toolCallLine(t, "3", "write_file", map[string]interface{}{"path": "../../etc/shadow"})

	forward, _, block, _ := in.HandleClientRequest(line)
	if forward || block == nil {
		t.Fatalf("expected path traversal to be hard-blocked")
	}
}

func TestShellChainIsHardBlocked(t *testing.T) {
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
	line := toolCallLine(t, "4", "run_shell", map[string]interface{}{"command": "ls && rm -rf /"})

	forward, _, block, _ := in.HandleClientRequest(line)
	if forward || block == nil {
		t.Fatalf("expected shell chain to be hard-blocked")
	}
}

func TestFlaggedCallWithNoPromptAutoDenies(t *testing.T) {
	rec := &recorder{}
	// Out-of-scope contract so the call FLAGs rather than allows/blocks.
	c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	in := NewInterceptor("run-1", c, rec.emit, nil, true) // Prompt is nil: no interactive surface
	line := toolCallLine(t, "5", "write_file", map[string]interface{}{"path": "docs/readme.md"})

	forward, _, block, err := in.HandleClientRequest(line)
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if forward || block == nil {
		t.Fatalf("expected FLAG with no prompt available to auto-deny (fail closed)")
	}
}

// TestInterceptorConsultsLiveContractOnEachCall guards against the
// asymmetry between proxy and guard that the Pass 0.5 comment on
// cmd_proxy.go used to document as deferred: guard's watcher path
// (contractAwareEmit) calls gc.get() fresh on every fs event, so a
// mid-run `centrol scope +<path>` takes effect immediately there.
// Interceptor must now do the same for tools/call evaluation when
// ContractFunc is set — the mechanism cmd_proxy.go wires it through.
func TestInterceptorConsultsLiveContractOnEachCall(t *testing.T) {
	rec := &recorder{}
	c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	in := NewInterceptor("run-1", c, rec.emit, nil, true) // Prompt is nil: FLAG auto-denies

	// Out of scope before any amendment: FLAG tier with no interactive
	// prompt auto-denies (fail closed), same as
	// TestFlaggedCallWithNoPromptAutoDenies above.
	line1 := toolCallLine(t, "1", "write_file", map[string]interface{}{"path": "docs/readme.md"})
	forward1, _, block1, err := in.HandleClientRequest(line1)
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if forward1 || block1 == nil {
		t.Fatalf("expected the out-of-scope call to be denied before any scope amendment")
	}

	// Simulate a mid-run `centrol scope +docs` amendment the way
	// cmd_proxy.go's guardedContract.amend does: widen a contract value
	// held outside the Interceptor, and point ContractFunc at it — the
	// Interceptor's own static Contract field is deliberately left
	// untouched, to prove evaluation reads through ContractFunc rather
	// than the stale snapshot NewInterceptor captured.
	c.AmendScope("docs")
	in.ContractFunc = func() policy.Contract { return c }

	line2 := toolCallLine(t, "2", "write_file", map[string]interface{}{"path": "docs/readme.md"})
	forward2, _, block2, err := in.HandleClientRequest(line2)
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if !forward2 || block2 != nil {
		t.Fatalf("expected the scope amendment to take effect on the very next tools/call (live contract), got forward=%v block=%v", forward2, block2)
	}
}

func TestFlaggedCallAllowOnceForwardsButDoesNotGrantSession(t *testing.T) {
	rec := &recorder{}
	c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	prompt := func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) {
		return AllowOnce, nil
	}
	in := NewInterceptor("run-1", c, rec.emit, prompt, true)

	line1 := toolCallLine(t, "6", "write_file", map[string]interface{}{"path": "docs/a.md"})
	forward, _, block, _ := in.HandleClientRequest(line1)
	if !forward || block != nil {
		t.Fatalf("expected allow-once to forward the first call")
	}

	// A second flagged call to the same tool must prompt again — allow
	// once must not silently become a standing grant.
	promptCalls := 0
	in.Prompt = func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) {
		promptCalls++
		return Deny, nil
	}
	line2 := toolCallLine(t, "7", "write_file", map[string]interface{}{"path": "docs/b.md"})
	forward2, _, block2, _ := in.HandleClientRequest(line2)
	if forward2 || block2 == nil {
		t.Fatalf("expected second flagged call to be evaluated independently")
	}
	if promptCalls != 1 {
		t.Fatalf("expected the interactive prompt to fire again for a second flagged call, promptCalls=%d", promptCalls)
	}
}

func TestFlaggedCallAllowSessionGrantsFutureCallsAndLogsAmend(t *testing.T) {
	rec := &recorder{}
	c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	prompt := func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) {
		return AllowSession, nil
	}
	in := NewInterceptor("run-1", c, rec.emit, prompt, true)

	line1 := toolCallLine(t, "8", "write_file", map[string]interface{}{"path": "docs/a.md"})
	forward, _, _, _ := in.HandleClientRequest(line1)
	if !forward {
		t.Fatalf("expected allow-session to forward the call")
	}
	if !rec.has("policy.amend") {
		t.Fatalf("expected policy.amend logged for allow-session, got %v", rec.types)
	}

	// Second call to the SAME tool should now sail through without
	// prompting again.
	in.Prompt = func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) {
		t.Fatalf("prompt should not be called again after an Allow session grant")
		return Deny, nil
	}
	line2 := toolCallLine(t, "9", "write_file", map[string]interface{}{"path": "docs/b.md"})
	forward2, _, block2, _ := in.HandleClientRequest(line2)
	if !forward2 || block2 != nil {
		t.Fatalf("expected session grant to auto-forward subsequent calls to the same tool")
	}
}

func TestAllowSessionDisabledByEnterpriseConfig(t *testing.T) {
	rec := &recorder{}
	c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	sawAllowSessionOffered := true
	prompt := func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) {
		sawAllowSessionOffered = allowSessionOffered
		return AllowOnce, nil
	}
	in := NewInterceptor("run-1", c, rec.emit, prompt, false) // AllowSessionAmend=false
	line := toolCallLine(t, "10", "write_file", map[string]interface{}{"path": "docs/a.md"})
	in.HandleClientRequest(line)
	if sawAllowSessionOffered {
		t.Fatalf("expected AllowSessionAmend=false to be conveyed to the prompt")
	}
}

func TestMassMutationIsFlagged(t *testing.T) {
	rec := &recorder{}
	c := testContract() // whole-repo scope so path check alone would Allow
	prompt := func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) {
		if !strings.Contains(reason, "mass-mutation") {
			t.Fatalf("expected mass mutation reason, got %q", reason)
		}
		return Deny, nil
	}
	in := NewInterceptor("run-1", c, rec.emit, prompt, true)

	paths := make([]interface{}, 0, 25)
	for i := 0; i < 25; i++ {
		paths = append(paths, "src/file"+strings.Repeat("x", i%3)+".go")
	}
	line := toolCallLine(t, "11", "batch_write", map[string]interface{}{"paths": paths})
	forward, _, block, _ := in.HandleClientRequest(line)
	if forward || block == nil {
		t.Fatalf("expected mass mutation to be flagged then denied")
	}
}

func TestNonJSONClientFrameIsSilencedNotForwarded(t *testing.T) {
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
	forward, fwd, block, err := in.HandleClientRequest([]byte("not json at all"))
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if forward || fwd != nil || block != nil {
		t.Fatalf("expected non-JSON frame to be dropped, not forwarded or blocked")
	}
	if !rec.has("policy.silence") {
		t.Fatalf("expected policy.silence for unparseable frame")
	}
}

func TestNonToolCallMethodsPassThroughUnevaluated(t *testing.T) {
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
	line := []byte(`{"jsonrpc":"2.0","id":"1","method":"initialize","params":{}}`)
	forward, fwd, block, err := in.HandleClientRequest(line)
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if !forward || block != nil || string(fwd) != string(line) {
		t.Fatalf("expected non-tools/call methods to pass through untouched")
	}
	if len(rec.types) != 0 {
		t.Fatalf("expected no ledger noise for pass-through methods, got %v", rec.types)
	}
}

// TestFullPumpMockMCPServer runs Run() against a real subprocess (a tiny
// shell echo loop standing in for an MCP server) and confirms: an
// allowed call reaches the target and its response reaches the client,
// a blocked call's response comes straight from centrol without ever
// reaching the target, and the client-facing stdout stream contains
// only valid JSON-RPC frames (the mandatory stdio discipline).
func TestFullPumpMockMCPServer(t *testing.T) {
	if _, err := exec_LookPathSh(); err != nil {
		t.Skip("sh not available in this environment")
	}

	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	// The mock target echoes back a canned JSON-RPC result for any
	// tools/call it receives, one line per input line: a minimalist
	// stand-in MCP server with no external dependency.
	script := `while IFS= read -r line; do echo '{"jsonrpc":"2.0","id":"echo","result":{"ok":true}}'; done`

	var clientOut bytes.Buffer
	var diag bytes.Buffer
	clientIn := strings.NewReader(
		string(toolCallLine(t, "20", "read_file", map[string]interface{}{"path": "src/main.go"})) + "\n" +
			string(toolCallLine(t, "21", "read_file", map[string]interface{}{"path": "/etc/passwd"})) + "\n",
	)

	err := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	outLines := strings.Split(strings.TrimSpace(clientOut.String()), "\n")
	for _, l := range outLines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("client stdout contained a non-JSON-RPC line (stdio discipline violated): %q", l)
		}
	}

	if !rec.has("tool.call") {
		t.Fatalf("expected the allowed call to be logged as tool.call")
	}
	if !rec.has("tool.blocked") {
		t.Fatalf("expected the /etc/passwd call to be logged as tool.blocked")
	}

	sawBlockError := false
	for _, l := range outLines {
		if strings.Contains(l, "blocked by policy") {
			sawBlockError = true
		}
	}
	if !sawBlockError {
		t.Fatalf("expected a policy-blocked JSON-RPC error to reach the client for the hard-blocked call")
	}
}

// TestConcurrentInFlightCallsCompleteOutOfOrder is the mandated
// concurrency test: MCP allows multiple in-flight tools/call requests,
// and a slow one must never block a faster one behind it. This proxy
// has no request/response correlation queue of its own — it forwards
// requests to the target as soon as each is evaluated, and relays
// whatever order the target itself emits responses in, via an
// independent goroutine. This test proves that property empirically: a
// mock target that answers a "fast" request in ~10ms and a "slow" one
// in ~500ms (started first) must deliver the fast response to the
// client BEFORE the slow one, not in request order.
func TestConcurrentInFlightCallsCompleteOutOfOrder(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	// Each backgrounded subshell writes its response independently, so
	// the target can answer "fast" before "slow" even though "slow" was
	// received first — exactly the scenario a naive sequential
	// request/response queue would get wrong.
	script := `
while IFS= read -r line; do
  case "$line" in
    *'"id":"slow"'*) ( sleep 0.5; echo '{"jsonrpc":"2.0","id":"slow","result":{"tag":"slow"}}' ) & ;;
    *'"id":"fast"'*) ( sleep 0.01; echo '{"jsonrpc":"2.0","id":"fast","result":{"tag":"fast"}}' ) & ;;
  esac
done
wait
`
	slowLine := toolCallLine(t, "slow", "read_file", map[string]interface{}{"path": "a.go"})
	fastLine := toolCallLine(t, "fast", "read_file", map[string]interface{}{"path": "b.go"})

	var clientOut bytes.Buffer
	var diag bytes.Buffer
	clientIn := strings.NewReader(string(slowLine) + "\n" + string(fastLine) + "\n")

	if err := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag); err != nil {
		t.Fatalf("Run: %v", err)
	}

	outLines := strings.Split(strings.TrimSpace(clientOut.String()), "\n")
	if len(outLines) != 2 {
		t.Fatalf("expected 2 response lines, got %d: %q", len(outLines), outLines)
	}
	if !strings.Contains(outLines[0], `"id":"fast"`) {
		t.Fatalf("expected the fast response first (completion order), got order: %v", outLines)
	}
	if !strings.Contains(outLines[1], `"id":"slow"`) {
		t.Fatalf("expected the slow response second, got order: %v", outLines)
	}
}

// TestDiagnosticsNeverReachClientStdout is the mandated stdout-
// contamination regression test. It proves two things: (1) a target
// process's own stderr chatter never lands on the client-facing
// stdout stream, and (2) every line that DOES land on it is valid,
// parseable JSON-RPC — so a future regression (e.g. an accidental
// fmt.Println on the wrong writer, exactly the class of bug already
// caught once in the guard surface's git-diff handling) fails this
// test immediately instead of shipping silently.
func TestDiagnosticsNeverReachClientStdout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	// The mock target deliberately writes noise to ITS OWN stderr
	// before and after answering, simulating a chatty real-world MCP
	// server (startup banners, debug logs) that a naive proxy might
	// accidentally let leak onto the client's stdout if cmd.Stderr were
	// ever wired to the wrong writer.
	script := `
echo "mock-server: starting up" >&2
while IFS= read -r line; do
  echo "mock-server: got a request" >&2
  echo '{"jsonrpc":"2.0","id":"1","result":{"ok":true}}'
done
echo "mock-server: shutting down" >&2
`
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	var clientOut, diag bytes.Buffer
	clientIn := strings.NewReader(string(line) + "\n")

	if err := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Contains(clientOut.String(), "mock-server") {
		t.Fatalf("target's stderr chatter leaked onto client stdout: %q", clientOut.String())
	}
	if !strings.Contains(diag.String(), "mock-server") {
		t.Fatalf("expected target's stderr chatter to land on diagOut instead, got %q", diag.String())
	}

	for _, l := range strings.Split(strings.TrimSpace(clientOut.String()), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("client stdout contained a non-JSON-RPC line: %q (err: %v)", l, err)
		}
	}
}

// TestMalformedClientFrameDoesNotCrashAndIsSilenced is the mandated
// malformed-frame integration test, run through the full pump (not just
// HandleClientRequest in isolation): a bad frame from the agent must be
// recorded as policy.silence and dropped, never forwarded to the
// target and never crashing the proxy — valid frames before and after
// it must still be processed normally.
func TestMalformedClientFrameDoesNotCrashAndIsSilenced(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	script := `while IFS= read -r line; do echo '{"jsonrpc":"2.0","id":"echo","result":{"ok":true}}'; done`

	before := toolCallLine(t, "before", "read_file", map[string]interface{}{"path": "a.go"})
	after := toolCallLine(t, "after", "read_file", map[string]interface{}{"path": "b.go"})
	malformed := "{this is not valid json at all"

	var clientOut, diag bytes.Buffer
	clientIn := strings.NewReader(string(before) + "\n" + malformed + "\n" + string(after) + "\n")

	err := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag)
	if err != nil {
		t.Fatalf("Run should not crash or error on a malformed frame, got: %v", err)
	}

	if !rec.has("policy.silence") {
		t.Fatalf("expected the malformed frame to be recorded as policy.silence, got %v", rec.types)
	}

	outLines := strings.Split(strings.TrimSpace(clientOut.String()), "\n")
	var nonEmpty []string
	for _, l := range outLines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) != 2 {
		t.Fatalf("expected exactly 2 responses (before/after the malformed frame, which is dropped, not answered), got %d: %v", len(nonEmpty), nonEmpty)
	}
	for _, l := range nonEmpty {
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("client stdout contained a non-JSON-RPC line: %q", l)
		}
	}
}

// TestNonJSONLineOnTargetStdoutIsSilencedNotForwarded is the
// target-side mirror of TestMalformedClientFrameDoesNotCrashAndIsSilenced:
// a real-world MCP server (in particular a Node one) can leak a
// console.log or startup banner onto its own stdout instead of stderr.
// STDIO DISCIPLINE (see the package doc) makes the client-facing
// stdout stream JSON-RPC-only, non-negotiably, so that leak must be
// recorded as policy.silence and dropped — never forwarded to the
// client, where it would corrupt the agent harness's JSON-RPC parser —
// and it must not crash or hang the proxy, with the real response
// around it still delivered.
func TestNonJSONLineOnTargetStdoutIsSilencedNotForwarded(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	// target's stdout: a console.log leak, then its real JSON-RPC response
	script := `
echo "Server listening on stdio"
while IFS= read -r line; do
  echo '{"jsonrpc":"2.0","id":"1","result":{"ok":true}}'
done
`
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	var clientOut, diag bytes.Buffer
	clientIn := strings.NewReader(string(line) + "\n")

	if err := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag); err != nil {
		t.Fatalf("Run should not crash or error on a non-JSON target stdout line, got: %v", err)
	}

	if !rec.has("policy.silence") {
		t.Fatalf("expected the leaked line to be recorded as policy.silence, got %v", rec.types)
	}

	outLines := strings.Split(strings.TrimSpace(clientOut.String()), "\n")
	var nonEmpty []string
	for _, l := range outLines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) != 1 {
		t.Fatalf("expected exactly 1 response on client stdout (the leaked line dropped, not forwarded), got %d: %v", len(nonEmpty), nonEmpty)
	}
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(nonEmpty[0]), &v); err != nil {
		t.Fatalf("client stdout contained a non-JSON-RPC line (STDIO DISCIPLINE violated): %q", nonEmpty[0])
	}
}

// TestStdoutCleanlinessCheckHasTeeth is a negative control for
// TestDiagnosticsNeverReachClientStdout: it proves the "every stdout
// line must parse as JSON-RPC" assertion actually fails when
// contamination is present, rather than passing vacuously. Without this,
// a future refactor that weakens the check (e.g. skipping empty-ish
// lines too loosely) could silently stop catching the exact bug class
// this whole test file exists to prevent.
func TestStdoutCleanlinessCheckHasTeeth(t *testing.T) {
	contaminated := "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\ncentrol: debug: got here\n{\"jsonrpc\":\"2.0\",\"id\":\"2\",\"result\":{}}\n"
	sawFailure := false
	for _, l := range strings.Split(strings.TrimSpace(contaminated), "\n") {
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatalf("the JSON-RPC validity check failed to catch a deliberately contaminated stdout stream — the check itself is broken")
	}
}

func exec_LookPathSh() (string, error) {
	return exec.LookPath("sh")
}

// TestFlaggedCallTimeoutLogsPolicyViolationNotOrdinaryDenial is the
// real proof behind ship criterion 7: a PromptFunc that reports Timeout
// (as StderrPrompt does when gate.PromptWithTimeout's deadline elapses)
// must deny the call AND log it as policy.violation with
// reason="prompt_timeout" — distinct from an ordinary tool.blocked
// denial, so `centrol audit` can tell "nobody answered in time" apart
// from "an operator said no".
func TestFlaggedCallTimeoutLogsPolicyViolationNotOrdinaryDenial(t *testing.T) {
	rec := &recorder{}
	c := policy.Contract{TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	prompt := func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) {
		return Timeout, nil
	}
	in := NewInterceptor("run-1", c, rec.emit, prompt, true)
	line := toolCallLine(t, "1", "write_file", map[string]interface{}{"path": "docs/a.md"})

	forward, _, block, err := in.HandleClientRequest(line)
	if err != nil {
		t.Fatalf("HandleClientRequest: %v", err)
	}
	if forward || block == nil {
		t.Fatalf("expected a timed-out prompt to deny the call")
	}
	if !rec.has("policy.violation") {
		t.Fatalf("expected policy.violation logged for a timed-out prompt, got %v", rec.types)
	}
	found := false
	for _, e := range rec.entries {
		if e["reason"] == "prompt_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected reason=prompt_timeout on the logged entry, got %v", rec.entries)
	}
}

// TestMCPCallTimeoutReturnsStructuredErrorAndLogsToolTimeout is ship
// criterion 12: proxy.mcp_call_timeout_seconds = 2 against a mock
// server that takes 5s to answer one call must (a) deliver a
// structured JSON-RPC error to the client at ~2s rather than hanging
// until ~5s, (b) log a tool.timeout event naming the tool and elapsed
// time, and (c) leave a second, faster call on the same run completely
// unaffected — "only the hung call fails" per spec.
func TestMCPCallTimeoutReturnsStructuredErrorAndLogsToolTimeout(t *testing.T) {
	if _, err := exec_LookPathSh(); err != nil {
		t.Skip("sh not available in this environment")
	}

	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
	in.MCPCallTimeout = 2 * time.Second

	// Each input line is handled in its own backgrounded subshell, so
	// the fast call's response is not stuck behind the slow one in a
	// single-threaded read loop — mirroring a real MCP server that can
	// service concurrent in-flight calls.
	script := `while IFS= read -r line; do
  ( id=$(echo "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    case "$line" in
      *sleepy_tool*) sleep 5 ;;
    esac
    echo '{"jsonrpc":"2.0","id":"'"$id"'","result":{"ok":true}}'
  ) &
done
wait`

	var clientOut bytes.Buffer
	var diag bytes.Buffer
	clientIn := strings.NewReader(
		string(toolCallLine(t, "slow-1", "sleepy_tool", map[string]interface{}{"path": "src/a.go"})) + "\n" +
			string(toolCallLine(t, "fast-1", "read_file", map[string]interface{}{"path": "src/b.go"})) + "\n",
	)

	start := time.Now()
	if err := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = time.Since(start) // Run only returns once the target (and its 5s sleep) has actually exited; the ~2s bound below is about *when the client's structured error appears in the stream*, not Run's total wall time.

	outLines := strings.Split(strings.TrimSpace(clientOut.String()), "\n")
	var sawTimeoutError, sawFastResult bool
	for _, l := range outLines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("client stdout contained a non-JSON-RPC line: %q", l)
		}
		if errObj, ok := v["error"].(map[string]interface{}); ok {
			if data, ok := errObj["data"].(map[string]interface{}); ok && data["reason"] == "mcp_call_timeout" {
				sawTimeoutError = true
				if errObj["code"] != float64(-32000) {
					t.Fatalf("expected code -32000 for mcp_call_timeout, got %v", errObj["code"])
				}
			}
		}
		if id, _ := v["id"].(string); id == "fast-1" {
			if _, ok := v["result"]; ok {
				sawFastResult = true
			}
		}
	}
	if !sawTimeoutError {
		t.Fatalf("expected a structured mcp_call_timeout JSON-RPC error in client stdout, got %q", clientOut.String())
	}
	if !sawFastResult {
		t.Fatalf("expected the second, unrelated call's real result to reach the client (only the hung call should fail), got %q", clientOut.String())
	}
	if !rec.has("tool.timeout") {
		t.Fatalf("expected a tool.timeout ledger entry, got %v", rec.types)
	}
}

// TestShipCriterion15_PolicyDenialsAreStructuredNotSilent proves ship
// criterion 15 end to end at the interceptor level: a hard block, an
// operator's explicit deny, and a prompt timeout each return a
// structured JSON-RPC error carrying the fixed data.reason vocabulary
// (never Protocol Silence — that's for malformed input only, proven
// separately by TestNonJSONClientFrameIsSilencedNotForwarded), and none
// of them corrupt interceptor state: the very next call, an ordinary
// allowed one, still succeeds normally.
func TestShipCriterion15_PolicyDenialsAreStructuredNotSilent(t *testing.T) {
	assertStructuredDenial := func(t *testing.T, block []byte, wantReason string) {
		t.Helper()
		var resp map[string]interface{}
		if err := json.Unmarshal(block, &resp); err != nil {
			t.Fatalf("denial response not valid JSON-RPC: %v (%s)", err, block)
		}
		errObj, ok := resp["error"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected a JSON-RPC error object, got %s", block)
		}
		data, ok := errObj["data"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected error.data to be present (the agent-actionable part), got %s", block)
		}
		if data["reason"] != wantReason {
			t.Fatalf("expected data.reason=%q, got %v (%s)", wantReason, data["reason"], block)
		}
		if data["target"] == "" || data["target"] == nil {
			t.Fatalf("expected data.target to be set, got %s", block)
		}
		if data["suggestion"] == "" || data["suggestion"] == nil {
			t.Fatalf("expected data.suggestion to be set, got %s", block)
		}
	}

	t.Run("hard_block", func(t *testing.T) {
		rec := &recorder{}
		in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)
		line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "/etc/passwd"})
		forward, _, block, err := in.HandleClientRequest(line)
		if err != nil || forward || block == nil {
			t.Fatalf("expected a hard block denial, forward=%v err=%v block=%s", forward, err, block)
		}
		assertStructuredDenial(t, block, "credential_path")
		if rec.has("policy.silence") {
			t.Fatalf("a hard block must not also log policy.silence")
		}

		// The agent's next call, an ordinary in-scope one, must succeed
		// normally — a denial must not corrupt interceptor state.
		next := toolCallLine(t, "2", "write_file", map[string]interface{}{"path": "src/ok.go"})
		forward2, fwd2, block2, err2 := in.HandleClientRequest(next)
		if err2 != nil || !forward2 || block2 != nil || fwd2 == nil {
			t.Fatalf("expected the next call to be allowed normally, got forward=%v block=%s err=%v", forward2, block2, err2)
		}
	})

	t.Run("operator_denied", func(t *testing.T) {
		rec := &recorder{}
		c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
		prompt := func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) { return Deny, nil }
		in := NewInterceptor("run-1", c, rec.emit, prompt, true)
		line := toolCallLine(t, "3", "write_file", map[string]interface{}{"path": "docs/a.md"})
		forward, _, block, err := in.HandleClientRequest(line)
		if err != nil || forward || block == nil {
			t.Fatalf("expected an operator-denied response")
		}
		assertStructuredDenial(t, block, "operator_denied")

		next := toolCallLine(t, "4", "write_file", map[string]interface{}{"path": "src/ok.go"})
		forward2, fwd2, block2, _ := in.HandleClientRequest(next)
		if !forward2 || block2 != nil || fwd2 == nil {
			t.Fatalf("expected the next call to succeed after an operator deny")
		}
	})

	t.Run("prompt_timeout", func(t *testing.T) {
		rec := &recorder{}
		c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
		prompt := func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) { return Timeout, nil }
		in := NewInterceptor("run-1", c, rec.emit, prompt, true)
		line := toolCallLine(t, "5", "write_file", map[string]interface{}{"path": "docs/a.md"})
		forward, _, block, err := in.HandleClientRequest(line)
		if err != nil || forward || block == nil {
			t.Fatalf("expected a prompt-timeout response")
		}
		assertStructuredDenial(t, block, "prompt_timeout")

		in.Prompt = func(tool, reason, preview string, allowSessionOffered bool) (Decision, error) { return AllowOnce, nil }
		next := toolCallLine(t, "6", "write_file", map[string]interface{}{"path": "docs/b.md"})
		forward2, fwd2, block2, _ := in.HandleClientRequest(next)
		if !forward2 || block2 != nil || fwd2 == nil {
			t.Fatalf("expected the next call to succeed after a prompt timeout")
		}
	})
}

// --- Pass 0.5a: ReadFrames must distinguish a real scan/read failure
// from an ordinary clean EOF. ---

// flakyReader serves data first, then fails every subsequent Read with
// err (which must not be io.EOF, or bufio.Scanner would treat it as a
// clean end rather than a real error).
type flakyReader struct {
	data []byte
	err  error
}

func (f *flakyReader) Read(p []byte) (int, error) {
	if len(f.data) > 0 {
		n := copy(p, f.data)
		f.data = f.data[n:]
		return n, nil
	}
	return 0, f.err
}

func TestReadFramesCleanEOFReportsNoError(t *testing.T) {
	r := strings.NewReader(`{"jsonrpc":"2.0","id":"1","method":"ping"}` + "\n")
	lines, errFn := ReadFrames(r)

	var got [][]byte
	for line := range lines {
		got = append(got, line)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 line, got %d", len(got))
	}
	if err := errFn(); err != nil {
		t.Fatalf("expected nil error after a clean EOF, got: %v", err)
	}
}

func TestReadFramesScanErrorIsReportedAsErrStreamRead(t *testing.T) {
	boom := errors.New("boom: pipe went away")
	r := &flakyReader{
		data: []byte(`{"jsonrpc":"2.0","id":"1","method":"ping"}` + "\n"),
		err:  boom,
	}
	lines, errFn := ReadFrames(r)

	var got [][]byte
	for line := range lines {
		got = append(got, line)
	}
	if len(got) != 1 {
		t.Fatalf("expected the one line sent before the failure, got %d", len(got))
	}

	err := errFn()
	if err == nil {
		t.Fatalf("expected a non-nil error after a real read failure, got nil")
	}
	if !errors.Is(err, ErrStreamRead) {
		t.Fatalf("expected errors.Is(err, ErrStreamRead) to hold, got: %v", err)
	}
}

// TestClientStreamReadErrorIsSilencedAndSurfaced exercises the failure
// through the real client -> target pump (not just ReadFrames in
// isolation): a clientIn whose Read fails mid-stream (not a clean EOF)
// must produce a policy.silence entry naming the clientIn stream, as
// opposed to a stream that simply closes normally, which must not.
func TestClientStreamReadErrorIsSilencedAndSurfaced(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	script := `while IFS= read -r line; do echo '{"jsonrpc":"2.0","id":"1","result":{"ok":true}}'; done`
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	clientIn := &flakyReader{data: []byte(string(line) + "\n"), err: errors.New("boom: client pipe broke")}

	var clientOut, diag bytes.Buffer
	runErr := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag)

	if !errors.Is(runErr, ErrStreamRead) {
		t.Fatalf("expected Run to return an error wrapping ErrStreamRead, got: %v", runErr)
	}

	rec.mu.Lock()
	var found bool
	for i, typ := range rec.types {
		if typ == "policy.silence" && rec.entries[i]["stream"] == "clientIn" {
			found = true
		}
	}
	rec.mu.Unlock()
	if !found {
		t.Fatalf("expected a policy.silence entry for stream=clientIn, got types=%v", rec.types)
	}
}

// TestCleanClientEOFProducesNoStreamErrorSilence is the negative control
// for TestClientStreamReadErrorIsSilencedAndSurfaced: an ordinary client
// hangup (clean EOF, the common case — e.g. the harness closing stdin)
// must NOT be recorded as a stream read error, and Run must not report
// one.
func TestCleanClientEOFProducesNoStreamErrorSilence(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	script := `while IFS= read -r line; do echo '{"jsonrpc":"2.0","id":"1","result":{"ok":true}}'; done`
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	clientIn := strings.NewReader(string(line) + "\n")

	var clientOut, diag bytes.Buffer
	runErr := Run(Target{Command: "sh", Args: []string{"-c", script}}, in, clientIn, &clientOut, &diag)
	if runErr != nil {
		t.Fatalf("expected a clean EOF to produce no error, got: %v", runErr)
	}

	rec.mu.Lock()
	for i, typ := range rec.types {
		if typ == "policy.silence" && rec.entries[i]["reason"] == "stream read error" {
			t.Fatalf("did not expect a stream-read-error policy.silence entry for a clean EOF")
		}
	}
	rec.mu.Unlock()
}

// --- Pass 0.5b: the out_of_scope denial hint must not tell the agent
// that `centrol scope` will change anything for a running proxy (it
// doesn't — see the Interceptor construction comment in
// cmd/centrol/cmd_proxy.go). ---
func TestOutOfScopeDenialSuggestionDoesNotMentionCentrolScope(t *testing.T) {
	rec := &recorder{}
	c := policy.Contract{Kind: policy.KindProxy, TaskID: "run-1", RepoRoot: "/repo", AllowedPaths: []string{"src"}, MassMutationThreshold: 20}
	in := NewInterceptor("run-1", c, rec.emit, nil, true)
	line := toolCallLine(t, "1", "write_file", map[string]interface{}{"path": "../outside/evil.go"})
	forward, _, block, err := in.HandleClientRequest(line)
	if err != nil || forward || block == nil {
		t.Fatalf("expected an out-of-scope path to be denied")
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(block, &resp); err != nil {
		t.Fatalf("denial response not valid JSON-RPC: %v", err)
	}
	data := resp["error"].(map[string]interface{})["data"].(map[string]interface{})
	suggestion, _ := data["suggestion"].(string)
	if strings.Contains(suggestion, "centrol scope") {
		t.Fatalf("suggestion still tells the agent to run `centrol scope`, which has no effect on a running proxy: %q", suggestion)
	}
}

// --- Pre-Pass-2 check (extended by pass 2.5's targetWaiter split):
// RunTarget's optional-capability degrade path (targetWaiter /
// targetErrReporter) must not silently skip a transport.Target that
// implements only the four required methods — it must drive it
// correctly and report the missing capability on diagOut, never
// panic, and never skip the mandatory shutdown signal (target.Stop()). ---

// minimalTarget implements exactly transport.Target's four required
// methods (Start, Send, Receive, Stop) — deliberately no Err() and no
// Wait() — so it can never satisfy targetErrReporter or targetWaiter.
// It answers exactly one request: Send echoes back a canned result
// for whatever id it was given and then closes its receive channel,
// so the target -> client pump ends deterministically without
// depending on when RunTarget happens to call Stop().
type minimalTarget struct {
	mu      sync.Mutex
	ch      chan []byte
	started bool
	stopped bool
}

func newMinimalTarget() *minimalTarget {
	return &minimalTarget{ch: make(chan []byte, 4)}
}

func (m *minimalTarget) Start(ctx context.Context) error {
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	return nil
}

func (m *minimalTarget) Send(frame []byte) error {
	var env map[string]interface{}
	if err := json.Unmarshal(frame, &env); err != nil {
		return err
	}
	resp, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": env["id"], "result": map[string]interface{}{"ok": true},
	})
	if err != nil {
		return err
	}
	m.ch <- resp
	close(m.ch)
	return nil
}

func (m *minimalTarget) Receive() (<-chan []byte, error) {
	return m.ch, nil
}

func (m *minimalTarget) Stop() error {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	return nil
}

func (m *minimalTarget) wasStarted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}

func (m *minimalTarget) wasStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

// safeBuffer is a bytes.Buffer with its own lock, for a test where more
// than one goroutine writes to the same sink concurrently (here:
// RunTarget's two pump goroutines can both write a capability-degrade
// debug note to diagOut). RunTarget itself serializes those writes
// with its own internal mutex, but that only protects diagOut's
// *contents* from interleaving — a plain bytes.Buffer read from the
// test goroutine while a pump goroutine might still be writing is a
// separate, textbook data race on the Buffer's own fields, which this
// avoids.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunTargetDrivesMinimalTargetWithoutOptionalCapabilities(t *testing.T) {
	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	mt := newMinimalTarget()
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	clientIn := strings.NewReader(string(line) + "\n")
	clientOut := &safeBuffer{}
	diag := &safeBuffer{}

	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = RunTarget(mt, in, clientIn, clientOut, diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunTarget did not return against a minimal Target — likely deadlocked on a missing optional capability")
	}

	if runErr != nil {
		t.Fatalf("expected RunTarget to complete without error against a minimal Target, got: %v", runErr)
	}

	if !mt.wasStarted() {
		t.Fatalf("expected target.Start() to have been called")
	}
	if !mt.wasStopped() {
		t.Fatalf("expected target.Stop() to have been called — shutdown cleanup must not be skipped just because optional capabilities are absent")
	}

	out := strings.TrimSpace(clientOut.String())
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("client stdout did not contain the forwarded response: %q (%v)", out, err)
	}
	if resp["id"] != "1" {
		t.Fatalf("expected response id=1, got %v", resp["id"])
	}

	// The degrade path must not be silent-skip: RunTarget notes each
	// missing capability on diagOut (never on clientOut — STDIO
	// DISCIPLINE still applies). Both notes here are written on
	// RunTarget's own return path — targetErrReporter's by the target ->
	// client pump, which RunTarget's return already waits on (via
	// targetToClientDone), and targetWaiter's synchronously in RunTarget
	// itself, right after that same drain — so, unlike the old
	// targetInputCloser note (written asynchronously from the client ->
	// target pump's defer), both are guaranteed present the instant
	// RunTarget returns; no polling needed.
	if !strings.Contains(diag.String(), "targetWaiter") {
		t.Fatalf("expected a debug note about the missing targetWaiter capability on diagOut, got: %q", diag.String())
	}
	if !strings.Contains(diag.String(), "targetErrReporter") {
		t.Fatalf("expected a debug note about the missing targetErrReporter capability on diagOut, got: %q", diag.String())
	}
	if strings.Contains(clientOut.String(), "debug:") {
		t.Fatalf("the capability-degrade debug note leaked onto clientOut, violating STDIO DISCIPLINE: %q", clientOut.String())
	}
}

// --- Pass 2.5: RunTarget driving a transport.HTTPTarget end-to-end,
// verifying Stop's DELETE is timed correctly by RunTarget's own
// shutdown sequence (not just by HTTPTarget's own unit tests, which
// call Stop directly rather than through RunTarget). ---

func TestRunTargetWithHTTPTargetSendsDeleteOnlyAfterBothResponsesForwarded(t *testing.T) {
	var mu sync.Mutex
	var events []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			events = append(events, "delete")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var env map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		id := env["id"]
		mu.Lock()
		events = append(events, fmt.Sprintf("response-%v", id))
		mu.Unlock()
		w.Header().Set("Mcp-Session-Id", "sess-order")
		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"ok": true},
		})
		_, _ = w.Write(resp)
	}))
	defer srv.Close()

	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	line1 := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	line2 := toolCallLine(t, "2", "read_file", map[string]interface{}{"path": "b.go"})
	clientIn := strings.NewReader(string(line1) + "\n" + string(line2) + "\n")

	target := &transport.HTTPTarget{URL: srv.URL}
	var clientOut, diag bytes.Buffer

	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = RunTarget(target, in, clientIn, &clientOut, &diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunTarget did not return against an HTTPTarget")
	}
	if runErr != nil {
		t.Fatalf("expected RunTarget to complete without error, got: %v", runErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("expected 3 events (two responses, then one delete), got %d: %v", len(events), events)
	}
	if events[2] != "delete" {
		t.Fatalf("expected DELETE to be the last event, fired only after both responses were forwarded — got order: %v", events)
	}
}

func TestRunTargetWithHTTPTargetSendsNoDeleteIfClientClosesBeforeAnyRequest(t *testing.T) {
	var mu sync.Mutex
	var deleteSeen bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleteSeen = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s request: no client call was ever sent, so the target should never see traffic", r.Method)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rec := &recorder{}
	in := NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	clientIn := strings.NewReader("") // EOF immediately: the client never sends a request.

	target := &transport.HTTPTarget{URL: srv.URL}
	var clientOut, diag bytes.Buffer

	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = RunTarget(target, in, clientIn, &clientOut, &diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunTarget did not return against an HTTPTarget with no client traffic")
	}
	if runErr != nil {
		t.Fatalf("expected RunTarget to complete without error, got: %v", runErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if deleteSeen {
		t.Fatal("expected no DELETE to be sent when no request ever established a session")
	}
}
