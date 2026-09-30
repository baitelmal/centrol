package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

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
	// plain &http.Client{} on first use if nil. Exposed so tests (and,
	// later, Pass 3's CLI wiring) can inject one.
	Client *http.Client

	mu      sync.Mutex
	ctx     context.Context
	recvCh  chan []byte
	stopped bool
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
	if t.recvCh == nil {
		t.recvCh = make(chan []byte)
	}
	return nil
}

// Receive returns the channel Send delivers response frames onto.
// Safe to call more than once; the channel is created once (in Start,
// or lazily here if Receive is somehow called first) and reused.
func (t *HTTPTarget) Receive() (<-chan []byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.recvCh == nil {
		t.recvCh = make(chan []byte)
	}
	return t.recvCh, nil
}

// Send POSTs frame to URL and forwards whatever response frame(s) it
// produces onto the channel Receive returns, per the Streamable HTTP
// contract: a single application/json body becomes one frame, a
// text/event-stream's "data:" lines each become one frame in order, a
// 202 Accepted means "no response to forward right now," and a
// non-2xx/non-202 status or a request-level failure (network error,
// timeout) is returned as an error rather than silently dropped.
func (t *HTTPTarget) Send(frame []byte) error {
	ctx, cancel := t.requestContext()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("proxy: building HTTP request to target: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid := t.getSessionID(); sid != "" {
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

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.setSessionID(sid)
	}

	if resp.StatusCode == http.StatusAccepted {
		// No body: the response will arrive via a separately-held
		// stream, if the server keeps one open. Nothing to forward now.
		_, _ = io.Copy(io.Discard, resp.Body)
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
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("proxy: reading target's response: %w", err)
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
func (t *HTTPTarget) deliver(frame []byte) {
	cp := make([]byte, len(frame))
	copy(cp, frame)
	t.mu.Lock()
	ch := t.recvCh
	t.mu.Unlock()
	if ch != nil {
		ch <- cp
	}
}

// Stop sends a best-effort DELETE to terminate the session, if one
// was issued, and closes the receive channel. DELETE errors are
// intentionally ignored (cleanup, not a correctness requirement) per
// the pass 2 spec. Idempotent: proxy.RunTarget calls Stop from more
// than one place as a shutdown-signal fallback, and a second call
// here is a no-op rather than a duplicate DELETE or a double-close of
// the receive channel.
func (t *HTTPTarget) Stop() error {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return nil
	}
	t.stopped = true
	sid := t.SessionID
	ch := t.recvCh
	t.recvCh = nil
	t.mu.Unlock()

	if sid != "" {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, t.URL, nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", sid)
			if resp, derr := t.httpClient().Do(req); derr == nil {
				_ = resp.Body.Close()
			}
		}
	}

	if ch != nil {
		close(ch)
	}
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
		t.Client = &http.Client{}
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
