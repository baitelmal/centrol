package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxJSONResponseBytes bounds how much of a single application/json
// response body sendOnce will read into memory. Audit fix: this path
// previously used a bare io.ReadAll(resp.Body) with no limit at all,
// unlike the two error-status branches a few lines above it (each
// already wrapped in io.LimitReader) and deliverSSE's own per-line
// buffer cap just below — a malicious or compromised target returning
// an arbitrarily large 2xx body could OOM the proxy process. Sized the
// same as deliverSSE's per-event buffer cap for consistency; there is
// no spec reason a legitimate single JSON-RPC frame needs to approach
// this size.
const maxJSONResponseBytes = 16 * 1024 * 1024

// deleteTimeout bounds the best-effort session-teardown DELETE Stop
// sends — see Stop's doc comment for why this is a fixed bound
// independent of the operator-configured Timeout field.
const deleteTimeout = 5 * time.Second

// maxRedirects bounds how many redirects checkRedirectPolicy will
// follow before refusing outright, regardless of host.
const maxRedirects = 3

// checkRedirectPolicy is installed as http.Client.CheckRedirect on the
// default client httpClient() constructs. Audit fix (5a): without this,
// Go's default redirect handling follows a target's redirect to any
// host and forwards every request header except a fixed, hardcoded
// "sensitive" set (Authorization, Cookie, ...) — Mcp-Session-Id is a
// custom header carrying this target's own session state and is not in
// that set, so an unconstrained redirect could replay it verbatim to an
// entirely different, attacker-controlled host. Returning a non-nil
// error here stops Client.Do from ever sending the redirected request
// at all, so a refused cross-host redirect means the header is never
// transmitted there. Same-host redirects (same scheme, host, and port)
// are allowed, up to maxRedirects.
func checkRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("proxy: target issued more than %d redirects", maxRedirects)
	}
	orig := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, orig.Scheme) || !strings.EqualFold(req.URL.Host, orig.Host) {
		return fmt.Errorf("proxy: refusing cross-host redirect from %s to %s", orig.Host, req.URL.Host)
	}
	return nil
}

// ErrHTTPTimeout marks an HTTPTarget.Send failure as the configured
// per-request timeout expiring (Timeout, or the context passed to
// Start running out), distinct from other transport errors, so a
// later pass can surface it as its own structured JSON-RPC error
// (mcp_call_timeout-shaped) rather than a generic failure.
var ErrHTTPTimeout = errors.New("proxy/transport: HTTP request to target timed out")

// HTTPStatusError reports a non-2xx, non-202 HTTP response from the
// target, so callers can distinguish "the target rejected this call"
// (4xx/5xx) from a transport-level failure (a network or timeout
// error) and, in a later pass, turn it into the same kind of
// structured JSON-RPC error a hard policy block already returns —
// never silence.
type HTTPStatusError struct {
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("proxy: target returned HTTP %d: %s", e.StatusCode, e.Body)
}

// HTTPTarget fronts an MCP server reached over HTTP using the MCP
// Streamable HTTP transport (2025-03-26 spec revision): every frame
// is its own POST to URL, answered either as a single
// application/json body or as a text/event-stream whose "data:"
// lines each carry one frame. The older SSE-only transport
// (2024-11-05) is out of scope.
//
// Unlike StdioTarget, there is no persistent connection and no
// subprocess to wait for — Stop's job here is session cleanup (the
// best-effort DELETE) and closing the receive channel, and there is
// nothing left to wait for once that's done, so HTTPTarget does not
// implement targetWaiter. proxy.RunTarget calls Stop once the client
// has stopped sending — the same point analogous to StdioTarget's
// stdin-close — not at startup, so the DELETE reflects whatever
// traffic the run actually sent rather than ending the session before
// any of it goes out.
type HTTPTarget struct {
	URL string

	// SessionID is the Mcp-Session-Id the target issued on its first
	// response, if any, and is then sent on every subsequent request.
	// Exported per the transport's own wire semantics (a caller may
	// reasonably want to observe or seed it); Send and Stop access it
	// through the struct's mutex, not this field directly, so it is
	// safe to read only when no Send/Stop call can be concurrently in
	// flight (e.g. after a Send has returned).
	SessionID string

	// Timeout bounds each individual POST. Zero means no additional
	// deadline beyond the context passed to Start. (Pass 3 wires this
	// from proxy.http_timeout_seconds; Pass 2 only needs the
	// mechanism.)
	Timeout time.Duration

	// Client is the http.Client used for every request; defaults to a
	// plain &http.Client{} on first use if nil. Exposed so tests (and
	// Pass 3's CLI wiring) can inject one.
	Client *http.Client

	// MaxRetries bounds how many additional attempts Send makes beyond
	// the first when a request fails. Zero (the zero value) means no
	// retries. Pass 3 wires this from proxy.http_max_retries.
	//
	// A retry only ever happens for a dial failure — an error where the
	// request was never written to the wire, so the server could not
	// have begun processing it (connection refused, DNS failure, no
	// route to host). That is what "idempotent only" means here: Send
	// has no way to know whether the JSON-RPC method it's forwarding is
	// actually idempotent, so a retry is permitted only when nothing
	// could possibly have reached the server on the failed attempt —
	// never for a timeout, a mid-request write failure, or any error
	// once bytes may have left the wire, where the server may already
	// have processed the request.
	MaxRetries int

	mu       sync.Mutex
	ctx      context.Context
	recvOnce sync.Once
	recvCh   chan []byte
	stopped  bool

	// sendWG tracks deliver calls that have passed the stopped check and
	// are about to (or are currently) send on recvCh, so Stop can wait
	// for them to finish before it closes the channel — see deliver's
	// and Stop's doc comments for why this replaced holding t.mu across
	// the send itself.
	sendWG sync.WaitGroup
}

// Start records ctx for subsequent requests. Streamable HTTP has no
// persistent connection to open, so there is nothing else to do —
// per the pass 2 spec, "Start is a no-op except for context setup."
func (t *HTTPTarget) Start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	t.ctx = ctx
	t.ensureRecvCh()
	return nil
}

// Receive returns the channel Send delivers response frames onto.
// Safe to call more than once, including after Stop: recvCh is
// allocated exactly once (via recvOnce, in Start, or lazily here if
// Receive is somehow called first) and is never replaced or nilled
// out, so a Receive call that races with or follows Stop still gets
// the same channel Stop closed — a `for range` over it exits
// immediately instead of blocking on a fresh, orphaned channel
// nothing will ever close.
//
// Deliberately takes no lock: ensureRecvCh's own sync.Once makes the
// allocation itself safe to race, and Receive must never be blocked
// behind t.mu — see deliver's doc comment for the deadlock that
// caused when Receive needed the same mutex deliver held across its
// (blocking) channel send: the target->client pump calls Receive to
// get the channel it is about to range over, so if that call is
// blocked waiting for a lock deliver won't release until something
// reads from the very channel Receive hasn't returned yet, neither
// side can ever proceed. Reproduced directly under heavy scheduler
// contention once Governor.Run started registering its signal watch
// (and so spawning an extra goroutine) before target.Start, which
// shifted the two pump goroutines' relative scheduling enough to hit
// it reliably.
func (t *HTTPTarget) Receive() (<-chan []byte, error) {
	t.ensureRecvCh()
	return t.recvCh, nil
}

// ensureRecvCh lazily allocates recvCh exactly once, via sync.Once —
// safe to call without holding t.mu, and from any number of
// goroutines concurrently. recvCh, once allocated, is never set back
// to nil — see Stop and deliver for why that invariant is what makes
// this type's shutdown safe.
func (t *HTTPTarget) ensureRecvCh() {
	t.recvOnce.Do(func() {
		t.recvCh = make(chan []byte)
	})
}

// Send POSTs frame to URL and forwards whatever response frame(s) it
// produces onto the channel Receive returns, per the Streamable HTTP
// contract: a single application/json body becomes one frame, a
// text/event-stream's "data:" lines each become one frame in order, a
// 202 Accepted means "no response to forward right now," and a
// non-2xx/non-202 status or a request-level failure (network error,
// timeout) is returned as an error rather than silently dropped.
//
// Send retries sendOnce up to MaxRetries times, but only when the
// failure was a dial failure — see MaxRetries's doc comment for why
// that's the only case a retry is safe here.
//
// Send retries only on dial failures. It does not retry on 5xx
// responses or mid-request timeouts, even though the prompt
// originally allowed 5xx retry: at this layer, Send has no
// visibility into whether the JSON-RPC method is idempotent,
// so a 5xx that may have already processed the request is not
// safe to retry.
func (t *HTTPTarget) Send(frame []byte) error {
	for attempt := 0; ; attempt++ {
		err := t.sendOnce(frame)
		if err == nil {
			return nil
		}
		if attempt >= t.MaxRetries || !isDialFailure(err) {
			return err
		}
	}
}

// isDialFailure reports whether err reflects the request never having
// reached the target at all — a failure to even establish the
// connection — as opposed to a failure after the request was in
// flight. See HTTPTarget.MaxRetries for why this is the only case
// Send treats as safe to retry.
func isDialFailure(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// requestID extracts the "id" field from an outgoing JSON-RPC request
// frame, for correlating a synthetic response (sessionRevokedErrorFrame)
// back to the call that triggered it. ok is false for a notification
// (no id — nothing to correlate, and nothing a client would read a
// response for anyway) or a frame that doesn't even parse, which
// should not happen here since HandleClientRequest has already
// validated anything Interceptor forwards.
func requestID(frame []byte) (id json.RawMessage, ok bool) {
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(frame, &env); err != nil || len(env.ID) == 0 {
		return nil, false
	}
	return env.ID, true
}

// sessionRevokedErrorFrame builds a JSON-RPC error response, addressed
// to id, reporting that the target rejected this call's session.
// Deliberately a plain local struct rather than importing
// internal/proxy's structured-error vocabulary (path_traversal,
// operator_denied, and so on): those are policy decisions the
// Interceptor makes about a call it evaluated; this is a transport-
// level fact (the target doesn't recognize this session anymore) that
// HTTPTarget itself is in the best — and, given Governor.Run's pump
// architecture, the only practical — position to report, with no
// policy judgment involved and no need to pull transport into
// proxy's policy package to say so.
func sessionRevokedErrorFrame(id json.RawMessage, statusCode int, body string) []byte {
	type errorData struct {
		Reason     string `json:"reason"`
		StatusCode int    `json:"status_code"`
		Detail     string `json:"detail,omitempty"`
	}
	type rpcError struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int       `json:"code"`
			Message string    `json:"message"`
			Data    errorData `json:"data"`
		} `json:"error"`
	}
	resp := rpcError{JSONRPC: "2.0", ID: id}
	resp.Error.Code = -32002
	resp.Error.Message = "target session revoked"
	resp.Error.Data = errorData{Reason: "session_revoked", StatusCode: statusCode, Detail: body}
	// Marshal failure is unreachable here — every field is a concrete,
	// already-valid value (id came from json.Unmarshal of a real
	// request, the rest are plain strings/ints) — so falling back to a
	// minimal hand-built frame rather than propagating an error keeps
	// deliver's signature (frame []byte, no error) simple.
	b, err := json.Marshal(resp)
	if err != nil {
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"target session revoked"}}`, string(id)))
	}
	return b
}

// sendOnce is Send's single-attempt body.
func (t *HTTPTarget) sendOnce(frame []byte) error {
	ctx, cancel := t.requestContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("proxy: building HTTP request to target: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	sid := t.getSessionID()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := t.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %v", ErrHTTPTimeout, err)
		}
		return fmt.Errorf("proxy: HTTP request to target failed: %w", err)
	}
	defer resp.Body.Close()

	if newSID := resp.Header.Get("Mcp-Session-Id"); newSID != "" {
		t.setSessionID(newSID)
	}

	if resp.StatusCode == http.StatusAccepted {
		// No body: the response will arrive via a separately-held
		// stream, if the server keeps one open. Nothing to forward now.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	// A 401/403 on a request that carried a session id is the
	// Streamable HTTP transport's way of saying that session is no
	// longer valid — expired, or revoked server-side — not an ordinary
	// auth failure the operator needs to fix (that case has sid == "",
	// since there was never a session to revoke in the first place, and
	// falls through to the generic HTTPStatusError below unchanged).
	//
	// Pass 4, item 2: this must be "logged as a structured error,
	// surfaced to the client, not silence." Silence (proxy.policy.silence)
	// is reserved for bytes centrol can't make sense of; this is the
	// opposite — a clearly-understood, actionable failure for the one
	// call that hit it. The three resolved tiers below are, in order of
	// precedence: hard block, surfaced denial, actionable failure.
	//
	// Governor.Run's client->target pump (internal/governor, out of
	// scope for this pass) treats any non-nil error from Send as fatal
	// to the whole run — exactly right for a transport-level failure,
	// wrong for "this one call's session needs reinitializing." So
	// rather than return an error here (which would end the run), this
	// clears the now-dead session id (so the next call goes out fresh,
	// consistent with "server never issues a session id" being a
	// no-assumption case) and delivers a synthetic JSON-RPC error frame
	// for THIS call's id onto the same receive channel a real response
	// would use — Governor.Run's target->client pump forwards it to the
	// client exactly like any other response, and
	// Interceptor.HandleTargetResponse logs it as tool.result with
	// error=true (a structured error, not policy.silence) along the
	// way. sendOnce then returns nil: as far as Send/the pump are
	// concerned, this attempt succeeded — the client, not the proxy,
	// decides whether to reinitialize and retry.
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && sid != "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.setSessionID("")
		if id, ok := requestID(frame); ok {
			t.deliver(sessionRevokedErrorFrame(id, resp.StatusCode, strings.TrimSpace(string(body))))
		}
		// A notification (no id) has no response to deliver regardless
		// of outcome, per JSON-RPC — the session is still cleared above
		// so the next real call starts fresh.
		return nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPStatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	ct := resp.Header.Get("Content-Type")
	mediaType := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
	switch mediaType {
	case "text/event-stream":
		return t.deliverSSE(resp.Body)
	default:
		// "application/json" per spec, and the fallback for any other
		// (or missing) Content-Type: forward the raw body as a single
		// frame. If it isn't valid JSON-RPC, HandleTargetResponse
		// already flags that as policy.silence — the same as any other
		// malformed frame, stdio included.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseBytes+1))
		if err != nil {
			return fmt.Errorf("proxy: reading target's response: %w", err)
		}
		if len(body) > maxJSONResponseBytes {
			return fmt.Errorf("proxy: target's response body exceeded %d bytes", maxJSONResponseBytes)
		}
		t.deliver(body)
		return nil
	}
}

// deliverSSE reads a text/event-stream body and forwards each "data:"
// line as its own frame, in order, until the stream ends. Lines
// starting with ":" are comments and are ignored; blank lines
// separate events and carry nothing on their own; any other SSE field
// (event:, id:, retry:) is not a payload and is ignored — only
// "data:" lines are forwarded, per the pass 2 spec.
func (t *HTTPTarget) deliverSSE(body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			t.deliver([]byte(payload))
		default:
			continue
		}
	}
	return scanner.Err()
}

// deliver copies frame and sends it on the receive channel. Copying
// matches StdioTarget/proxy.ReadFrames's own convention of never
// handing out a slice a caller might still be reusing.
//
// An earlier version of this method held t.mu across the send itself
// (not just the "is t.stopped" check), reasoning that nothing on the
// receiving end ever needed t.mu, so holding it across a blocking
// send couldn't deadlock. That reasoning was wrong: the receiving end
// is the target->client pump's call to Receive, and an earlier
// version of Receive DID need t.mu to hand back the very channel this
// send blocks on — so if deliver's send ran first and grabbed the
// lock, Receive could never get it, nothing could ever read the
// channel, and deliver's send (holding the lock) blocked forever.
// Reproduced directly (TestRunTargetWithHTTPTargetSendsDeleteOnlyAfter-
// BothResponsesForwarded, under heavy scheduler contention).
//
// The fix: sendWG, not t.mu, now guards against racing Stop's close.
// deliver checks "is t.stopped" and, if not, registers its intent to
// send (sendWG.Add(1)) in the same critical section — so either this
// runs before Stop's own "set stopped, then wait, then close" critical
// section (and Stop's Wait blocks until this send finishes, then
// closes safely after), or Stop's stopped=true is already visible
// here (and this skips the frame instead of sending on, or panicking
// on, a closed channel). Either way the actual send happens with t.mu
// released, so it can never block anything that needs t.mu to make
// progress — including Receive.
func (t *HTTPTarget) deliver(frame []byte) {
	cp := make([]byte, len(frame))
	copy(cp, frame)

	t.mu.Lock()
	if t.stopped {
		// Stop has already closed recvCh (or is about to, under this
		// same lock); nothing is listening anymore, and sending here
		// would panic on a closed channel. Drop the frame.
		t.mu.Unlock()
		return
	}
	t.sendWG.Add(1)
	t.mu.Unlock()
	defer t.sendWG.Done()

	t.ensureRecvCh()
	t.recvCh <- cp
}

// Stop sends a best-effort DELETE to terminate the session, if one
// was issued, and closes the receive channel. DELETE errors are
// intentionally ignored (cleanup, not a correctness requirement) per
// the pass 2 spec. Idempotent: proxy.RunTarget calls Stop from more
// than one place as a shutdown-signal fallback, and a second call
// here is a no-op rather than a duplicate DELETE or a double-close of
// the receive channel.
//
// Stop never sets recvCh back to nil. The old behavior did, so that
// a Receive call racing with (or arriving after) Stop would silently
// allocate a fresh channel nothing would ever write to or close,
// hanging any `for range` over it forever. recvCh is allocated once
// and kept for the HTTPTarget's lifetime; stopped (checked and set
// under t.mu, same as deliver's own check) is the single source of
// truth for whether the channel has been closed.
//
// Stop waits on sendWG — populated by deliver, under the same
// stopped-check critical section — before closing the channel: any
// deliver call that observed stopped == false before this ran has
// already registered its intent to send, and this blocks until each
// of those sends has actually completed, so the close below can never
// race a send that was already committed to happen. See deliver's
// doc comment for why t.mu itself no longer guards the send.
func (t *HTTPTarget) Stop() error {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return nil
	}
	t.stopped = true
	sid := t.SessionID
	t.mu.Unlock()

	if sid != "" {
		// Audit fix: this request used to carry context.Background() —
		// no deadline at all, unlike every other request sendOnce makes
		// (requestContext applies t.Timeout). A target that accepts the
		// connection but never responds to the DELETE could hang this
		// call, and therefore Stop, and therefore the whole proxy
		// process's shutdown path, indefinitely. deleteTimeout is a
		// fixed bound independent of t.Timeout (which may be 0 — no
		// limit — by the operator's own choice for normal traffic) since
		// this is best-effort cleanup, not a call whose latency the
		// operator has any reason to tune.
		ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.URL, nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", sid)
			if resp, derr := t.httpClient().Do(req); derr == nil {
				_ = resp.Body.Close()
			}
		}
	}

	t.sendWG.Wait()
	t.ensureRecvCh()
	close(t.recvCh)
	return nil
}

func (t *HTTPTarget) requestContext() (context.Context, context.CancelFunc) {
	t.mu.Lock()
	parent := t.ctx
	timeout := t.Timeout
	t.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func (t *HTTPTarget) httpClient() *http.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Client == nil {
		t.Client = &http.Client{CheckRedirect: checkRedirectPolicy}
	}
	return t.Client
}

func (t *HTTPTarget) getSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.SessionID
}

func (t *HTTPTarget) setSessionID(sid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.SessionID = sid
}
