package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/scirem/centrol/internal/policy"
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
