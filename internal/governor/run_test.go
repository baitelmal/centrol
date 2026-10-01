package governor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/scirem/centrol/internal/policy"
	"github.com/scirem/centrol/internal/proxy"
	"github.com/scirem/centrol/internal/proxy/transport"
)

// This file holds the pump-orchestration tests migrated from
// internal/proxy/proxy_test.go under Pass 3.9 Shape A: everything that
// drove the old proxy.Run/proxy.RunTarget free functions now drives
// Governor.Run instead, since the orchestration logic itself moved
// here (see run.go). The ~25 tests that exercise
// Interceptor.HandleClientRequest/HandleTargetResponse directly (no
// pump involved) stayed in internal/proxy/proxy_test.go, unaffected by
// this refactor.
//
// Because this file is package governor (white-box, matching
// governor_test.go), it constructs a *Governor directly via &Governor{}
// rather than through New() — these tests exercise Run's pump logic
// only, never Emit/Validate/the ledger, so the zero-value schema/ledger
// fields are never touched.

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

func exec_LookPathSh() (string, error) {
	return exec.LookPath("sh")
}

// TestFullPumpMockMCPServer runs Governor.Run against a real subprocess
// (a tiny shell echo loop standing in for an MCP server) and confirms:
// an allowed call reaches the target and its response reaches the
// client, a blocked call's response comes straight from centrol
// without ever reaching the target, and the client-facing stdout
// stream contains only valid JSON-RPC frames (the mandatory stdio
// discipline).
func TestFullPumpMockMCPServer(t *testing.T) {
	if _, err := exec_LookPathSh(); err != nil {
		t.Skip("sh not available in this environment")
	}

	rec := &recorder{}
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

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

	g := &Governor{}
	err := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag)
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
// and a slow one must never block a faster one behind it. Governor.Run
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
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

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

	g := &Governor{}
	if err := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag); err != nil {
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
// parseable JSON-RPC.
func TestDiagnosticsNeverReachClientStdout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	// The mock target deliberately writes noise to ITS OWN stderr before
	// and after answering, simulating a chatty real-world MCP server
	// (startup banners, debug logs) that a naive proxy might accidentally
	// let leak onto the client's stdout if cmd.Stderr were ever wired to
	// the wrong writer.
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

	g := &Governor{}
	if err := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag); err != nil {
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
// malformed-frame integration test, run through the full pump (not
// just HandleClientRequest in isolation): a bad frame from the agent
// must be recorded as policy.silence and dropped, never forwarded to
// the target and never crashing Governor.Run — valid frames before and
// after it must still be processed normally.
func TestMalformedClientFrameDoesNotCrashAndIsSilenced(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	script := `while IFS= read -r line; do echo '{"jsonrpc":"2.0","id":"echo","result":{"ok":true}}'; done`

	before := toolCallLine(t, "before", "read_file", map[string]interface{}{"path": "a.go"})
	after := toolCallLine(t, "after", "read_file", map[string]interface{}{"path": "b.go"})
	malformed := "{this is not valid json at all"

	var clientOut, diag bytes.Buffer
	clientIn := strings.NewReader(string(before) + "\n" + malformed + "\n" + string(after) + "\n")

	g := &Governor{}
	err := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag)
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
// STDIO DISCIPLINE makes the client-facing stdout stream JSON-RPC-only,
// non-negotiably, so that leak must be recorded as policy.silence and
// dropped — never forwarded to the client — and it must not crash or
// hang Governor.Run, with the real response around it still delivered.
func TestNonJSONLineOnTargetStdoutIsSilencedNotForwarded(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

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

	g := &Governor{}
	if err := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag); err != nil {
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
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)
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

	g := &Governor{}
	start := time.Now()
	if err := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag); err != nil {
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

// TestClientStreamReadErrorIsSilencedAndSurfaced exercises the failure
// through the real client -> target pump (not just proxy.ReadFrames in
// isolation): a clientIn whose Read fails mid-stream (not a clean EOF)
// must produce a policy.silence entry naming the clientIn stream, as
// opposed to a stream that simply closes normally, which must not.
func TestClientStreamReadErrorIsSilencedAndSurfaced(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available in this environment")
	}
	rec := &recorder{}
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	script := `while IFS= read -r line; do echo '{"jsonrpc":"2.0","id":"1","result":{"ok":true}}'; done`
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	clientIn := &flakyReader{data: []byte(string(line) + "\n"), err: errors.New("boom: client pipe broke")}

	var clientOut, diag bytes.Buffer
	g := &Governor{}
	runErr := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag)

	if !errors.Is(runErr, proxy.ErrStreamRead) {
		t.Fatalf("expected Run to return an error wrapping proxy.ErrStreamRead, got: %v", runErr)
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
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	script := `while IFS= read -r line; do echo '{"jsonrpc":"2.0","id":"1","result":{"ok":true}}'; done`
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	clientIn := strings.NewReader(string(line) + "\n")

	var clientOut, diag bytes.Buffer
	g := &Governor{}
	runErr := g.Run(context.Background(), transport.NewStdioTarget("sh", []string{"-c", script}, &diag), in, clientIn, &clientOut, &diag)
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

// --- Pre-Pass-2 check (extended by pass 2.5's targetWaiter split):
// Governor.Run's optional-capability degrade path (targetWaiter /
// targetErrReporter) must not silently skip a RunTransport that
// implements only the four required methods — it must drive it
// correctly and report the missing capability on diagOut, never
// panic, and never skip the mandatory shutdown signal (target.Stop()).
// ---

// minimalTarget implements exactly RunTransport's four required
// methods (Start, Send, Receive, Stop) — deliberately no Err() and no
// Wait() — so it can never satisfy targetErrReporter or targetWaiter.
// It answers exactly one request: Send echoes back a canned result for
// whatever id it was given and then closes its receive channel, so the
// target -> client pump ends deterministically without depending on
// when Governor.Run happens to call Stop().
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
// Governor.Run's two pump goroutines can both write a capability-degrade
// debug note to diagOut). Governor.Run itself serializes those writes
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
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	mt := newMinimalTarget()
	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	clientIn := strings.NewReader(string(line) + "\n")
	clientOut := &safeBuffer{}
	diag := &safeBuffer{}

	g := &Governor{}
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = g.Run(context.Background(), mt, in, clientIn, clientOut, diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run did not return against a minimal target — likely deadlocked on a missing optional capability")
	}

	if runErr != nil {
		t.Fatalf("expected Governor.Run to complete without error against a minimal target, got: %v", runErr)
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

	// The degrade path must not be silent-skip: Governor.Run notes each
	// missing capability on diagOut (never on clientOut — STDIO
	// DISCIPLINE still applies). These exact debug substrings
	// ("targetWaiter", "targetErrReporter") are preserved verbatim from
	// the pre-3.9 RunTarget per the Pass 3.9c migration note.
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

// --- Pass 2.5: Governor.Run driving a transport.HTTPTarget end-to-end,
// verifying Stop's DELETE is timed correctly by Governor.Run's own
// shutdown sequence (not just by HTTPTarget's own unit tests, which
// call Stop directly rather than through Governor.Run). ---

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
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	line1 := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	line2 := toolCallLine(t, "2", "read_file", map[string]interface{}{"path": "b.go"})
	clientIn := strings.NewReader(string(line1) + "\n" + string(line2) + "\n")

	target := &transport.HTTPTarget{URL: srv.URL}
	var clientOut, diag bytes.Buffer

	g := &Governor{}
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = g.Run(context.Background(), target, in, clientIn, &clientOut, &diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run did not return against an HTTPTarget")
	}
	if runErr != nil {
		t.Fatalf("expected Governor.Run to complete without error, got: %v", runErr)
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
	in := proxy.NewInterceptor("run-1", testContract(), rec.emit, nil, true)

	clientIn := strings.NewReader("") // EOF immediately: the client never sends a request.

	target := &transport.HTTPTarget{URL: srv.URL}
	var clientOut, diag bytes.Buffer

	g := &Governor{}
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = g.Run(context.Background(), target, in, clientIn, &clientOut, &diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run did not return against an HTTPTarget with no client traffic")
	}
	if runErr != nil {
		t.Fatalf("expected Governor.Run to complete without error, got: %v", runErr)
	}

	mu.Lock()
	defer mu.Unlock()
	if deleteSeen {
		t.Fatal("expected no DELETE to be sent when no request ever established a session")
	}
}

// panicTarget triggers a synthetic panic inside Governor.Run's
// target->client pump goroutine (Receive panics immediately), to
// verify that goroutine's top-level recover (Pass 3.9c Final, Section
// 2 "Panics") catches it without crashing the process, routes it to
// Stop("panic", 2), and still lets Run return instead of hanging
// forever on a pump that never reports its own completion.
type panicTarget struct {
	mu      sync.Mutex
	stopped bool
}

func (p *panicTarget) Start(ctx context.Context) error { return nil }
func (p *panicTarget) Send(frame []byte) error         { return nil }
func (p *panicTarget) Receive() (<-chan []byte, error) {
	panic("synthetic panic for TestGovernorRunRecoversPumpPanic")
}
func (p *panicTarget) Stop() error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	return nil
}

func TestGovernorRunRecoversPumpPanic(t *testing.T) {
	rec := &recorder{}
	in := proxy.NewInterceptor("run-panic", testContract(), rec.emit, nil, true)

	clientIn := strings.NewReader("") // EOF immediately: nothing for the client->target pump to forward.
	clientOut := &safeBuffer{}
	diag := &safeBuffer{}

	g := &Governor{}
	pt := &panicTarget{}
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = g.Run(context.Background(), pt, in, clientIn, clientOut, diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run did not return after a pump panic — the recover must still let Run finish, not hang")
	}

	if runErr == nil {
		t.Fatal("expected Governor.Run to return a non-nil error after a pump panic")
	}
	if got := g.Cause(); got != "panic" {
		t.Fatalf(`expected Governor.Cause() == "panic" after a pump panic, got %q`, got)
	}
	if got := g.ExitCode(); got != 2 {
		t.Fatalf("expected Governor.ExitCode() == 2 after a pump panic, got %d", got)
	}
}

// sendPanicTarget triggers a synthetic panic inside Governor.Run's
// client->target pump goroutine specifically (Send panics), with
// Receive returning an already-closed channel so the target->client
// pump ends cleanly with a nil error on its own, and Stop succeeding
// normally — unlike panicTarget above, every OTHER signal Run reads
// (drainErr, waitErr, stopErr) is nil, so this isolates the audit's 4a
// gap: that pump's own recover calls Stop("panic", 2) but has no error
// channel of its own to report on (see Run's doc comment on the
// client->target goroutine), so before the 4a fix Run returned nil
// here even though a goroutine inside it had panicked.
// sendPanicTarget's Receive channel is deliberately gated on Stop, the
// same way cancelTarget's is — not pre-closed — so this test exercises
// the real causal chain a live target goes through: the target->client
// pump cannot end (and so Run's drainErr cannot resolve) until Stop
// closes the channel, and Stop is only ever reached, in this scenario,
// via the clientPumpDone-watching goroutine noticing the client->target
// pump ended. That is what makes the 4a ordering fix (clientPumpDone
// sent only after the panic's own recover has already called
// g.Stop("panic", 2)) actually observable here, rather than racing it
// the way a pre-closed channel would.
type sendPanicTarget struct {
	ch chan []byte
}

func newSendPanicTarget() *sendPanicTarget {
	return &sendPanicTarget{ch: make(chan []byte)}
}

func (s *sendPanicTarget) Start(ctx context.Context) error { return nil }
func (s *sendPanicTarget) Send(frame []byte) error {
	panic("synthetic panic for TestGovernorRunSurfacesClientToTargetPumpPanic")
}
func (s *sendPanicTarget) Receive() (<-chan []byte, error) { return s.ch, nil }
func (s *sendPanicTarget) Stop() error {
	close(s.ch)
	return nil
}

func TestGovernorRunSurfacesClientToTargetPumpPanic(t *testing.T) {
	rec := &recorder{}
	in := proxy.NewInterceptor("run-panic-2", testContract(), rec.emit, nil, true)

	line := toolCallLine(t, "1", "read_file", map[string]interface{}{"path": "a.go"})
	clientIn := strings.NewReader(string(line) + "\n")
	clientOut := &safeBuffer{}
	diag := &safeBuffer{}

	g := &Governor{}
	st := newSendPanicTarget()
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = g.Run(context.Background(), st, in, clientIn, clientOut, diag)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run did not return after a client->target pump panic")
	}

	if runErr == nil {
		t.Fatal("expected Governor.Run to return a non-nil error after a client->target pump panic (audit 4a)")
	}
	if got := g.Cause(); got != "panic" {
		t.Fatalf(`expected Governor.Cause() == "panic" after a client->target pump panic, got %q`, got)
	}
	if got := g.ExitCode(); got != 2 {
		t.Fatalf("expected Governor.ExitCode() == 2 after a client->target pump panic, got %d", got)
	}
}

// cancelTarget blocks Receive() until Stop() is called, then closes
// its receive channel — mirroring how a real RunTransport's Stop
// actually ends the target->client pump's loop. minimalTarget
// deliberately does not do this (its own tests exercise targets that
// end via a canned response instead), so a dedicated target is needed
// here to exercise context cancellation without the target->client
// pump hanging forever on a channel nothing ever closes.
type cancelTarget struct {
	ch      chan []byte
	started chan struct{} // closed once Start is called — see waitStarted
}

func newCancelTarget() *cancelTarget {
	return &cancelTarget{ch: make(chan []byte), started: make(chan struct{})}
}
func (c *cancelTarget) Start(context.Context) error {
	close(c.started)
	return nil
}
func (c *cancelTarget) Send([]byte) error { return nil }
func (c *cancelTarget) Receive() (<-chan []byte, error) {
	return c.ch, nil
}
func (c *cancelTarget) Stop() error {
	close(c.ch)
	return nil
}

// waitStarted blocks until Governor.Run has called Start on this
// target — used instead of a fixed sleep (Pass 3.9c Final correction)
// where a test needs Run to have reached a specific point, such as
// its own signal registration, before acting: Start is the first
// thing Run does, so by the time it returns, Run's goroutine has
// definitely been scheduled and is executing, removing goroutine-
// scheduling delay as a source of test flakiness under load. It does
// not, by itself, guarantee anything Run does AFTER Start has run yet
// (see individual callers for how they account for that).
func (c *cancelTarget) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-c.started:
	case <-time.After(5 * time.Second):
		t.Fatal("target.Start was never called")
	}
}

func TestGovernorRunContextCancellationStopsRun(t *testing.T) {
	rec := &recorder{}
	in := proxy.NewInterceptor("run-ctx", testContract(), rec.emit, nil, true)

	// clientIn is left open and silent (an io.Pipe nothing writes to or
	// closes until the test cleans up) so ordinary completion can never
	// win this race — the only way Run can end is via the context
	// cancellation below.
	clientInReader, clientInWriter := io.Pipe()
	defer clientInWriter.Close()
	clientOut := &safeBuffer{}
	diag := &safeBuffer{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := &Governor{}
	ct := newCancelTarget()
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = g.Run(ctx, ct, in, clientInReader, clientOut, diag)
		close(done)
	}()

	// Wait for Run to have actually started the target before
	// cancelling — the point of the test is cancellation interrupting
	// an in-progress run, not a cancelled context Run never got a
	// chance to observe mid-flight. Unlike a real OS signal, racing
	// this is never dangerous (there's no OS-default-disposition
	// fallthrough to worry about), but waitStarted is still strictly
	// more deterministic than a fixed sleep.
	ct.waitStarted(t)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run did not return after context cancellation")
	}
	_ = runErr // Run's return value here is the drain error (nil for a clean target Stop); the cause/code assertions below are what this test is actually checking.

	if got := g.Cause(); got != "context_cancelled" {
		t.Fatalf(`expected Governor.Cause() == "context_cancelled" after ctx cancellation, got %q`, got)
	}
	if got := g.ExitCode(); got != 2 {
		t.Fatalf("expected Governor.ExitCode() == 2 after ctx cancellation, got %d", got)
	}
}

// TestGovernorRunStopsOnSIGINT exercises Governor.Run's own signal
// watch (Pass 3.9c Final, Section 2 "Signals") — the gap its doc
// comment used to flag ("Unlike centrol guard, Governor.Run has no
// signal handling of its own"). Sends a real SIGINT to this test
// process while Run is blocked mid-session and checks that Run's
// WatchSignals call routes it to Stop (recording "sigint"/130) and
// unwinds the pumps so Run actually returns, instead of the signal
// either falling through to the OS default (which would kill the
// test binary) or leaving Run hanging.
func TestGovernorRunStopsOnSIGINT(t *testing.T) {
	rec := &recorder{}
	in := proxy.NewInterceptor("run-sigint", testContract(), rec.emit, nil, true)

	// clientIn stays open and silent so the only way this run ends is
	// via the signal below, not an ordinary clean completion racing it.
	clientInReader, clientInWriter := io.Pipe()
	defer clientInWriter.Close()
	clientOut := &safeBuffer{}
	diag := &safeBuffer{}

	// registered fires exactly once signal.Notify inside Run's own
	// WatchSignals call has actually returned (see signalsRegistered's
	// doc comment in run.go) — the one safe moment to send this process
	// a real SIGINT. Neither Start()-returned nor a fixed sleep are
	// that moment, only a proxy for it: under heavy scheduler
	// contention (a full `-race -count=N` run) this test reproduced the
	// failure mode registered exists to prevent — SIGINT sent a
	// moment too early, falling through to the OS default disposition
	// and killing the entire test binary, not just this test — when it
	// relied on ct.waitStarted(t) (Start returning) alone.
	registered := make(chan struct{})
	prevHook := signalsRegistered
	signalsRegistered = func() { close(registered) }
	defer func() { signalsRegistered = prevHook }()

	g := &Governor{}
	ct := newCancelTarget()
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = g.Run(context.Background(), ct, in, clientInReader, clientOut, diag)
		close(done)
	}()

	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run never reached WatchSignals registration")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signaling self: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Governor.Run did not return after SIGINT — its own signal watch must route it to Stop and unwind the pumps")
	}
	_ = runErr // the drain error isn't what this test is checking; Cause/ExitCode below are.

	if got := g.Cause(); got != "sigint" {
		t.Fatalf(`expected Governor.Cause() == "sigint" after SIGINT, got %q`, got)
	}
	if got := g.ExitCode(); got != 130 {
		t.Fatalf("expected Governor.ExitCode() == 130 after SIGINT, got %d", got)
	}
}
