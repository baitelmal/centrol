// Package proxy implements the MCP JSON-RPC gateway (centrol proxy): it sits
// between an MCP client (the agent's harness) and a real MCP server,
// intercepting tools/call requests for policy evaluation before they
// reach the target, and routing every response back through the
// Governor before it reaches the client.
//
// STDIO DISCIPLINE (mandatory): the client-facing stdout stream carries
// nothing but JSON-RPC frames. Prompts, errors, and debug output all go
// to stderr or the controlling TTY. Any other byte on that stdout would
// corrupt the client's parser and break the session.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/scirem/centrol/internal/policy"
)

// EmitFunc forwards one governed signal to the Governor.
type EmitFunc func(run, src, typ string, payload map[string]interface{}) error

// Decision is the outcome of a FLAG-tier interactive prompt.
type Decision int

const (
	AllowOnce Decision = iota
	AllowSession
	Deny
	Timeout // the human did not respond within prompt_timeout_seconds; distinct from an explicit Deny so it logs as policy.violation with reason="prompt_timeout" rather than an ordinary denial
)

// PromptFunc asks the human (via stderr/tty, never stdout) to resolve a
// FLAG-tier action. allowSessionOffered is false when the operator has
// disabled "Allow session" (the enterprise-configurable restriction).
type PromptFunc func(toolName, reason, argsPreview string, allowSessionOffered bool) (Decision, error)

// TimeoutFunc is invoked when a forwarded tools/call does not receive a
// response within Interceptor.MCPCallTimeout. It must deliver the
// structured timeout error directly to the client — Interceptor itself
// never touches the client-facing stdout stream, so the caller (proxy.Run)
// supplies this bound to its own synchronized writer.
type TimeoutFunc func(id json.RawMessage, tool string, elapsed time.Duration)

// rpcEnvelope is the minimal JSON-RPC 2.0 shape this package inspects.
// Fields it doesn't recognize are preserved via json.RawMessage so
// forwarding never drops or reorders anything the target/client sent.
type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ReadFrames reads newline-delimited JSON-RPC messages from r and sends
// each raw line (without its trailing newline) on the returned channel,
// which is closed when r is exhausted or errors.
func ReadFrames(r io.Reader) <-chan []byte {
	out := make(chan []byte)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			cp := make([]byte, len(line))
			copy(cp, line)
			out <- cp
		}
	}()
	return out
}

// WriteFrame writes exactly one JSON-RPC frame plus a trailing newline.
// This is the only function in the proxy package that is allowed to
// write to the client-facing stdout stream.
func WriteFrame(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n"))
	return err
}

// Interceptor holds the state needed to evaluate and log tools/call
// traffic for one run: the session contract, the Governor emit hook,
// the interactive prompt hook, and per-tool "allow session" grants.
type Interceptor struct {
	RunID             string
	Contract          policy.Contract
	Emit              EmitFunc
	Prompt            PromptFunc
	AllowSessionAmend bool // enterprise-configurable; false disables "Allow session"
	Observe           bool // observe-only mode: evaluate policy, log the decision, never prompt, never block

	// MCPCallTimeout bounds how long a forwarded tools/call may take to
	// receive a response before the client gets a structured timeout
	// error instead. Zero disables the timeout (no arming, no timers) —
	// callers should resolve proxy.mcp_call_timeout_seconds and set this
	// to a positive duration in normal operation; this field exists so
	// tests can exercise the interceptor without it.
	MCPCallTimeout time.Duration
	// OnTimeout is called exactly once per timed-out call, on its own
	// goroutine. proxy.Run sets this to a closure that writes the
	// timeout error to the client under its own stdout mutex.
	OnTimeout TimeoutFunc

	sessionGrants map[string]bool

	pendingMu    sync.Mutex
	pendingCalls map[string]*pendingCall // armed, not yet resolved either way — keyed by the JSON-RPC request id's raw string form
	timedOutIDs  map[string]bool         // ids whose timer already fired; kept for the run's lifetime so a late real response is still recognized and dropped
	pendingWG    sync.WaitGroup          // one Add per armed timer; Done exactly once per timer, whichever side (Stop() or the fired callback) accounts for it — see WaitPendingTimeouts
}

// pendingCall tracks one forwarded tools/call awaiting a response, for
// mcp_call_timeout_seconds correlation. timer is kept so a real
// response arriving first (resolvePending) can Stop() it — cancelling a
// timer that hasn't fired yet, rather than letting every armed call add
// its full timeout duration to how long Run() takes to return, which
// would turn an ordinary successful run into one that always waits out
// mcp_call_timeout_seconds after the target exits.
type pendingCall struct {
	tool  string
	timer *time.Timer
}

func NewInterceptor(runID string, contract policy.Contract, emit EmitFunc, prompt PromptFunc, allowSessionAmend bool) *Interceptor {
	return &Interceptor{
		RunID: runID, Contract: contract, Emit: emit, Prompt: prompt,
		AllowSessionAmend: allowSessionAmend, sessionGrants: map[string]bool{},
		pendingCalls: map[string]*pendingCall{}, timedOutIDs: map[string]bool{},
	}
}

// armTimeout registers a forwarded tools/call for timeout tracking and
// starts its timer. Calls with no JSON-RPC id (notifications) can't be
// correlated to a later response and are never armed — there would be
// nothing to reply to anyway. Safe to call with MCPCallTimeout <= 0 or
// OnTimeout == nil: it becomes a no-op, so a caller that never resolved
// proxy.mcp_call_timeout_seconds (e.g. an older test) is unaffected.
func (in *Interceptor) armTimeout(id json.RawMessage, tool string) {
	if in.MCPCallTimeout <= 0 || in.OnTimeout == nil {
		return
	}
	key := rawIDString(id)
	if key == "" {
		return
	}
	started := time.Now()
	pc := &pendingCall{tool: tool}

	in.pendingMu.Lock()
	if in.pendingCalls == nil {
		in.pendingCalls = map[string]*pendingCall{}
	}
	in.pendingCalls[key] = pc
	in.pendingMu.Unlock()

	// Tracked so Run can block until every armed timer has been
	// accounted for — fired, or cleanly canceled by a real response —
	// before it returns. See WaitPendingTimeouts for why this matters:
	// without it, a slow-firing timer could still write to clientOut
	// after the caller already treated a read of clientOut as final.
	in.pendingWG.Add(1)
	pc.timer = time.AfterFunc(in.MCPCallTimeout, func() {
		// Once this closure is actually running, resolvePending's
		// Stop() call (if any) has already lost that race — Stop()
		// returns false for a timer that has already fired or started
		// firing, and it is exactly that return value which tells
		// resolvePending NOT to call pendingWG.Done() itself. So this
		// closure, whenever it runs to this point, unconditionally owns
		// and must discharge the Done() that armTimeout's Add(1)
		// promised, regardless of which branch below it takes.
		defer in.pendingWG.Done()

		in.pendingMu.Lock()
		cur, ok := in.pendingCalls[key]
		if !ok || cur != pc {
			// Already resolved by a real response before this closure
			// got the lock (Stop() raced and lost, but the response
			// still won on the map): nothing left to time out.
			in.pendingMu.Unlock()
			return
		}
		delete(in.pendingCalls, key)
		in.timedOutIDs[key] = true
		in.pendingMu.Unlock()

		elapsed := time.Since(started)
		_ = in.Emit(in.RunID, "proxy", "tool.timeout", map[string]interface{}{
			"tool": tool, "elapsed_ms": elapsed.Milliseconds(), "id": key,
		})
		in.OnTimeout(id, tool, elapsed)
	})
}

// WaitPendingTimeouts blocks until every timer armed by armTimeout has
// been accounted for — either fired (and, if so, finished calling
// OnTimeout) or cleanly canceled because a real response arrived first.
// proxy.Run calls this after the target process has exited and both
// pump goroutines have drained, so it never returns while a timeout
// callback could still be about to write to clientOut. Unlike a naive
// "wait for every timer's full duration," this returns immediately once
// every call has actually been resolved one way or the other — an
// ordinary successful run with no timeouts returns as fast as the
// target itself exits, not delayed by mcp_call_timeout_seconds.
func (in *Interceptor) WaitPendingTimeouts() {
	in.pendingWG.Wait()
}

// resolvePending is called by HandleTargetResponse for every response
// carrying an id. It reports whether this id already timed out (in
// which case the caller must drop the response rather than forward it —
// the client already received a structured timeout error for that id,
// and a second response for the same id would confuse, not help, the
// agent). A response for an id that was never armed (Block/Flag denials,
// or MCPCallTimeout disabled) is not dropped. A response that arrives
// before its timer fires cancels that timer via Stop() — if that
// succeeds, this call (not the timer, which will now never fire)
// accounts for the pendingWG slot armTimeout added.
func (in *Interceptor) resolvePending(id json.RawMessage) (dropLateResponse bool) {
	key := rawIDString(id)
	if key == "" {
		return false
	}
	in.pendingMu.Lock()
	if in.timedOutIDs[key] {
		in.pendingMu.Unlock()
		return true
	}
	pc, ok := in.pendingCalls[key]
	if !ok {
		in.pendingMu.Unlock()
		return false
	}
	delete(in.pendingCalls, key)
	in.pendingMu.Unlock()

	if pc.timer.Stop() {
		// Successfully canceled before it fired: its AfterFunc body
		// will never run, so it will never call pendingWG.Done() —
		// this call must, on its behalf.
		in.pendingWG.Done()
	}
	// If Stop() returned false, the timer already fired (or is firing
	// concurrently right now); its own AfterFunc body already found (or
	// will find) the entry gone from pendingCalls and returns without
	// re-marking timedOutIDs or calling OnTimeout twice — see the
	// `cur != pc` / `!ok` guard there. That firing goroutine is the one
	// that calls pendingWG.Done() in that case, not this one.
	return false
}

// HandleClientRequest processes one raw JSON-RPC line arriving from the
// client (destined for the target MCP server). It returns forward=true
// with the (possibly unmodified) line to send to the target, or
// forward=false with a blockResponse line the caller should write
// directly back to the client's stdout instead — the target never sees
// a blocked call.
func (in *Interceptor) HandleClientRequest(line []byte) (forward bool, forwardLine []byte, blockResponse []byte, err error) {
	var env rpcEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		// Non-JSON-RPC noise from the client is exactly what Protocol
		// Silence exists for: record it fully inward, forward nothing,
		// say nothing outward.
		_ = in.Emit(in.RunID, "proxy", "policy.silence", map[string]interface{}{
			"reason": "unparseable client frame",
			"raw":    string(line),
		})
		return false, nil, nil, nil
	}

	if env.Method != "tools/call" {
		return true, line, nil, nil
	}

	var params toolCallParams
	if len(env.Params) > 0 {
		_ = json.Unmarshal(env.Params, &params)
	}

	args := extractToolCallArgs(params, in.Contract)
	tier, reason := policy.EvaluateToolCall(args, in.Contract)

	// Mass-mutation check layers on top of the per-path tier: a call can
	// individually address in-scope files yet still touch more of them
	// than the contract's threshold.
	if tier == policy.Allow {
		if mTier, mReason := policy.EvaluateMassMutation(len(args.PathArgs), in.Contract); mTier != policy.Allow {
			tier, reason = mTier, mReason
		}
	}

	switch tier {
	case policy.Allow:
		_ = in.Emit(in.RunID, "proxy", "tool.call", map[string]interface{}{
			"tool": params.Name, "decision": "allow", "id": rawIDString(env.ID),
		})
		in.armTimeout(env.ID, params.Name)
		return true, line, nil, nil

	case policy.Block:
		if in.Observe {
			// Observe mode: evaluated exactly as normal, but never
			// blocks — the action proceeds, and the ledger records what
			// WOULD have happened via payload.decision, distinguishable
			// from a real block (`centrol audit` marks these with ◇).
			_ = in.Emit(in.RunID, "proxy", "tool.blocked", map[string]interface{}{
				"tool": params.Name, "reason": reason, "id": rawIDString(env.ID), "decision": "would_block",
			})
			in.armTimeout(env.ID, params.Name)
			return true, line, nil, nil
		}
		_ = in.Emit(in.RunID, "proxy", "tool.blocked", map[string]interface{}{
			"tool": params.Name, "reason": reason, "id": rawIDString(env.ID),
		})
		// This is a legitimate policy denial, not malformed input — it
		// gets a structured JSON-RPC error the agent can act on, not
		// Protocol Silence. (Protocol Silence is reserved for the
		// unparseable-frame case above, which is unchanged.)
		resp, merr := policyDenialResponse(env.ID, classifyBlockReason(reason), params.Name, denialSuggestion(classifyBlockReason(reason)))
		if merr != nil {
			return false, nil, nil, merr
		}
		return false, nil, resp, nil

	case policy.Flag:
		if in.Observe {
			// Observe mode: no prompt, no gate, always allowed —
			// logged as would_flag so a dry run surfaces exactly what
			// enforcement would have interrupted, without interrupting
			// anything.
			_ = in.Emit(in.RunID, "proxy", "tool.call", map[string]interface{}{
				"tool": params.Name, "reason": reason, "id": rawIDString(env.ID), "decision": "would_flag",
			})
			in.armTimeout(env.ID, params.Name)
			return true, line, nil, nil
		}
		if in.sessionGrants[params.Name] {
			_ = in.Emit(in.RunID, "proxy", "tool.call", map[string]interface{}{
				"tool": params.Name, "decision": "allow (session grant)", "id": rawIDString(env.ID),
			})
			in.armTimeout(env.ID, params.Name)
			return true, line, nil, nil
		}
		if in.Prompt == nil {
			// No interactive surface available (non-TTY context): the
			// safe default for a FLAG-tier action with nobody to ask is
			// to deny it, not silently allow it.
			_ = in.Emit(in.RunID, "proxy", "tool.blocked", map[string]interface{}{
				"tool": params.Name, "reason": reason + " (flagged, no interactive prompt available, auto-denied)", "id": rawIDString(env.ID),
			})
			resp, merr := policyDenialResponse(env.ID, "out_of_scope", params.Name, "no interactive session was available to confirm this flagged action; run centrol proxy from a session with a controlling terminal, or widen the contract scope")
			return false, nil, resp, merr
		}
		decision, perr := in.Prompt(params.Name, reason, argsPreview(params.Arguments), in.AllowSessionAmend)
		if perr != nil {
			return false, nil, nil, perr
		}
		switch decision {
		case AllowOnce:
			_ = in.Emit(in.RunID, "proxy", "tool.call", map[string]interface{}{
				"tool": params.Name, "decision": "allow once", "reason": reason, "id": rawIDString(env.ID),
			})
			in.armTimeout(env.ID, params.Name)
			return true, line, nil, nil
		case AllowSession:
			if in.AllowSessionAmend {
				in.sessionGrants[params.Name] = true
			}
			_ = in.Emit(in.RunID, "proxy", "policy.amend", map[string]interface{}{
				"tool": params.Name, "reason": reason, "scope": "session",
			})
			in.armTimeout(env.ID, params.Name)
			return true, line, nil, nil
		case Timeout:
			_ = in.Emit(in.RunID, "proxy", "policy.violation", map[string]interface{}{
				"tool": params.Name, "reason": "prompt_timeout", "id": rawIDString(env.ID),
			})
			resp, merr := policyDenialResponse(env.ID, "prompt_timeout", params.Name, "the flag prompt was not answered in time and the call was denied; retry the same action to get a fresh prompt")
			return false, nil, resp, merr
		default: // Deny
			_ = in.Emit(in.RunID, "proxy", "tool.blocked", map[string]interface{}{
				"tool": params.Name, "reason": reason + " (denied by operator)", "id": rawIDString(env.ID),
			})
			resp, merr := policyDenialResponse(env.ID, "operator_denied", params.Name, "the operator denied this action; the run can continue with a different approach")
			return false, nil, resp, merr
		}
	}
	return true, line, nil, nil
}

// HandleTargetResponse routes a response from the target back toward
// the client, logging it as tool.result first. The Governor sees every
// response before the client does, per spec; here that means "log then
// forward" rather than transform, since centrol is an accountability layer,
// not a content filter for tool results. A nil forwardLine (with a nil
// error) means: drop this frame, forward nothing — used only when the
// call it responds to already timed out and the client already
// received a structured timeout error for that id.
func (in *Interceptor) HandleTargetResponse(line []byte) (forwardLine []byte, err error) {
	var env rpcEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		_ = in.Emit(in.RunID, "proxy", "policy.silence", map[string]interface{}{
			"reason": "unparseable target frame", "raw": string(line),
		})
		return line, nil // still forward: a malformed frame from the *target* is not the agent's fault to lose
	}
	if env.Result != nil || env.Error != nil {
		if in.resolvePending(env.ID) {
			// This id already timed out; the client has a structured
			// mcp_call_timeout error for it already. A second response
			// for the same id would be a protocol-level surprise, not a
			// courtesy — drop it rather than forward it.
			return nil, nil
		}
		payload := map[string]interface{}{"id": rawIDString(env.ID)}
		if env.Error != nil {
			payload["error"] = true
		}
		_ = in.Emit(in.RunID, "proxy", "tool.result", payload)
	}
	return line, nil
}

func rawIDString(id json.RawMessage) string {
	if len(id) == 0 {
		return ""
	}
	return string(id)
}

// policyErrorData is the structured "data" object every policy-denial
// error carries, per the 9e039fd4 spec: the agent gets a machine-
// checkable reason code, the path or tool name that triggered it, and
// a human-readable hint, so it can adapt (retry differently, skip the
// step, ask for clarification) instead of hanging.
type policyErrorData struct {
	Reason     string `json:"reason"`
	Target     string `json:"target"`
	Suggestion string `json:"suggestion"`
}

type structuredRPCError struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    policyErrorData `json:"data"`
	} `json:"error"`
}

// policyDenialResponse builds the structured JSON-RPC error for a
// legitimate policy denial: hard block, operator deny, or prompt
// timeout. code -32001, message "blocked by policy" — reason must be
// one of the fixed vocabulary the spec defines (path_traversal,
// credential_path, operator_denied, prompt_timeout, mcp_call_timeout,
// out_of_scope) so an agent can branch on it without string-matching
// free text.
func policyDenialResponse(id json.RawMessage, reason, target, suggestion string) ([]byte, error) {
	resp := structuredRPCError{JSONRPC: "2.0", ID: id}
	resp.Error.Code = -32001
	resp.Error.Message = "blocked by policy"
	resp.Error.Data = policyErrorData{Reason: reason, Target: target, Suggestion: suggestion}
	return json.Marshal(resp)
}

// timeoutErrorResponse builds the structured JSON-RPC error for
// proxy.mcp_call_timeout_seconds: a distinct code (-32000) and message
// from an ordinary policy denial, since a timeout is an operational
// failure, not a decision anyone made about the call's legitimacy — but
// the same structured data.reason="mcp_call_timeout" shape, so agent
// code that already branches on data.reason handles both uniformly.
func timeoutErrorResponse(id json.RawMessage, tool string) ([]byte, error) {
	resp := structuredRPCError{JSONRPC: "2.0", ID: id}
	resp.Error.Code = -32000
	resp.Error.Message = "mcp call timeout"
	resp.Error.Data = policyErrorData{
		Reason:     "mcp_call_timeout",
		Target:     tool,
		Suggestion: "the call did not complete in time; retry, use a different tool, or raise proxy.mcp_call_timeout_seconds",
	}
	return json.Marshal(resp)
}

// classifyBlockReason maps the free-text reason strings
// policy.EvaluateToolCall returns for its Block tier onto the fixed
// data.reason vocabulary the agent-facing error contract defines. Any
// block reason that isn't specifically a path-traversal or credential/
// system-path hit (e.g. a shell-metacharacter chain in tool args) falls
// back to "out_of_scope" — still a precise, actionable code, just not
// one of the two most specific ones.
func classifyBlockReason(reason string) string {
	switch {
	case strings.Contains(reason, "path traversal"):
		return "path_traversal"
	case strings.Contains(reason, "credential or system path"):
		return "credential_path"
	default:
		return "out_of_scope"
	}
}

// denialSuggestion gives a short, human-readable hint for each fixed
// reason code — what the agent (or the person reading the audit log)
// can actually do next.
func denialSuggestion(reason string) string {
	switch reason {
	case "path_traversal":
		return "the path resolves outside the repository root; use a path inside the repo"
	case "credential_path":
		return "this path holds credentials or system files and is never reachable; do not retry it"
	default:
		return "this action is outside the current contract; use `centrol scope +<path>` to widen it, or ask the operator"
	}
}

// looksLikePath is a heuristic: MCP tool argument schemas vary per
// server, so there is no single canonical way to know which argument
// values are filesystem paths. This intentionally errs toward treating
// more strings as paths (over-flagging) rather than fewer (under-
// blocking) — consistent with centrol being an accountability layer that
// would rather flag a false positive than miss a real path.
func looksLikePath(s string) bool {
	if s == "" {
		return false
	}
	return strings.ContainsAny(s, "/\\") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".")
}

var shellChainChars = []string{"&&", "||", ";", "`", "$(", "|"}

func looksLikeShellChain(s string) bool {
	for _, c := range shellChainChars {
		if strings.Contains(s, c) {
			return true
		}
	}
	return false
}

var newServerToolNames = []string{"add_server", "register_server", "install_server", "server/add", "server/register"}

func extractToolCallArgs(params toolCallParams, contract policy.Contract) policy.ToolCallArgs {
	args := policy.ToolCallArgs{ToolName: params.Name}
	for _, n := range newServerToolNames {
		if strings.EqualFold(params.Name, n) {
			args.NewMCPServer = true
		}
	}
	walkArgValues(params.Arguments, &args)
	return args
}

func walkArgValues(v interface{}, out *policy.ToolCallArgs) {
	switch val := v.(type) {
	case string:
		if looksLikeShellChain(val) {
			out.ShellChain = true
		}
		if looksLikePath(val) {
			out.PathArgs = append(out.PathArgs, val)
		}
	case map[string]interface{}:
		for _, vv := range val {
			walkArgValues(vv, out)
		}
	case []interface{}:
		for _, vv := range val {
			walkArgValues(vv, out)
		}
	}
}

func argsPreview(args map[string]interface{}) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
