package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// drainOne reads exactly one frame from ch, failing the test if none
// arrives within the timeout.
func drainOne(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case frame, ok := <-ch:
		if !ok {
			t.Fatal("receive channel closed before a frame arrived")
		}
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a frame on the receive channel")
	}
	return nil
}

func TestHTTPTargetForwardsJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected Content-Type: application/json, got %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("expected Accept header per spec, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"tools":[]}}`))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	if err := target.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ch, err := target.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"tools/list"}`))
	}()

	frame := drainOne(t, ch)

	if err := <-sendErr; err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}

	var env map[string]interface{}
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("forwarded frame is not valid JSON: %v (%s)", err, frame)
	}
	if env["id"] != "1" {
		t.Fatalf("expected id=1, got %v", env["id"])
	}
}

func TestHTTPTargetForwardsSSEEventsInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("httptest ResponseWriter does not support flushing")
		}
		body := "" +
			": this is a comment, ignore it\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{\"step\":1}}\n" +
			"\n" +
			"event: message\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":\"2\",\"result\":{\"step\":2}}\n" +
			"\n"
		_, _ = io.WriteString(w, body)
		flusher.Flush()
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	if err := target.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ch, err := target.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"tools/call"}`))
	}()

	first := drainOne(t, ch)
	second := drainOne(t, ch)

	if err := <-sendErr; err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}

	var firstEnv, secondEnv map[string]interface{}
	if err := json.Unmarshal(first, &firstEnv); err != nil {
		t.Fatalf("first frame is not valid JSON: %v (%s)", err, first)
	}
	if err := json.Unmarshal(second, &secondEnv); err != nil {
		t.Fatalf("second frame is not valid JSON: %v (%s)", err, second)
	}
	if firstEnv["id"] != "1" {
		t.Fatalf("expected the first forwarded frame to have id=1 (event order preserved), got %v", firstEnv["id"])
	}
	if secondEnv["id"] != "2" {
		t.Fatalf("expected the second forwarded frame to have id=2 (event order preserved), got %v", secondEnv["id"])
	}
}

func TestHTTPTargetSurfacesStructuredErrorOn500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	if err := target.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := target.Receive(); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"tools/list"}`))
	if err == nil {
		t.Fatal("expected Send to return an error for a 500 response, got nil")
	}

	var statusErr *HTTPStatusError
	if !asHTTPStatusError(err, &statusErr) {
		t.Fatalf("expected a structured *HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected StatusCode=500, got %d", statusErr.StatusCode)
	}
	if !strings.Contains(statusErr.Body, "internal error") {
		t.Fatalf("expected the error to carry the response body, got: %q", statusErr.Body)
	}
}

// asHTTPStatusError is errors.As, spelled out locally so this test
// file doesn't need to import "errors" just for one call.
func asHTTPStatusError(err error, target **HTTPStatusError) bool {
	for err != nil {
		if se, ok := err.(*HTTPStatusError); ok {
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestHTTPTargetSessionIDStoredAndSentOnSubsequentRequests(t *testing.T) {
	var gotSessionIDOnSecondRequest string
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Mcp-Session-Id", "sess-abc123")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
			return
		}
		gotSessionIDOnSecondRequest = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"2","result":{}}`))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()
	drainOne(t, ch)

	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"2","method":"b"}`)) }()
	drainOne(t, ch)

	if gotSessionIDOnSecondRequest != "sess-abc123" {
		t.Fatalf("expected the second request to carry Mcp-Session-Id: sess-abc123, got %q", gotSessionIDOnSecondRequest)
	}
	if target.SessionID != "sess-abc123" {
		t.Fatalf("expected HTTPTarget.SessionID to be stored, got %q", target.SessionID)
	}
}

func TestHTTPTargetStopSendsDeleteWithSessionID(t *testing.T) {
	var deleteSeen bool
	var deleteSessionID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteSeen = true
			deleteSessionID = r.Header.Get("Mcp-Session-Id")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-xyz")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()
	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()
	drainOne(t, ch)

	if err := target.Stop(); err != nil {
		t.Fatalf("Stop returned an error (DELETE failures must be best-effort): %v", err)
	}
	if !deleteSeen {
		t.Fatal("expected Stop to send a DELETE request")
	}
	if deleteSessionID != "sess-xyz" {
		t.Fatalf("expected the DELETE to carry Mcp-Session-Id: sess-xyz, got %q", deleteSessionID)
	}

	// Receive's channel must be closed once Stop completes.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the receive channel to be closed after Stop, got an unexpected frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receive channel was not closed after Stop")
	}
}

func TestHTTPTargetStopIgnoresDeleteErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()
	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()
	drainOne(t, ch)

	if err := target.Stop(); err != nil {
		t.Fatalf("expected Stop to ignore a DELETE failure (best-effort cleanup), got: %v", err)
	}
}

func TestHTTPTargetStopIsIdempotent(t *testing.T) {
	var deleteCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-idem")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()
	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()
	drainOne(t, ch)

	if err := target.Stop(); err != nil {
		t.Fatalf("first Stop call: %v", err)
	}
	if err := target.Stop(); err != nil {
		t.Fatalf("second Stop call: %v", err)
	}
	if err := target.Stop(); err != nil {
		t.Fatalf("third Stop call: %v", err)
	}

	if deleteCount != 1 {
		t.Fatalf("expected exactly one DELETE across three Stop calls, got %d", deleteCount)
	}
}

func TestHTTPTargetAcceptedResponseHasNoFrameToForward(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	if err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)); err != nil {
		t.Fatalf("expected a 202 Accepted to be handled without error, got: %v", err)
	}

	select {
	case frame := <-ch:
		t.Fatalf("expected no frame to be forwarded for a 202 Accepted response, got: %s", frame)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHTTPTargetTimeoutIsReportedAsErrHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL, Timeout: 20 * time.Millisecond}
	_ = target.Start(context.Background())
	if _, err := target.Receive(); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected Send to time out and return an error")
	}
	if !isErrHTTPTimeout(err) {
		t.Fatalf("expected the error to wrap ErrHTTPTimeout, got: %v", err)
	}
}

func isErrHTTPTimeout(err error) bool {
	for err != nil {
		if err == ErrHTTPTimeout {
			return true
		}
		if strings.Contains(err.Error(), ErrHTTPTimeout.Error()) {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// flakyDialTransport is an http.RoundTripper test double that fails
// with a dial error (*net.OpError{Op: "dial"}) on its first `failures`
// calls, then delegates to respFunc — letting these tests drive
// HTTPTarget.Send's retry loop deterministically, without depending on
// real network timing to produce a genuine connection-refused error.
type flakyDialTransport struct {
	failures int
	calls    int
	respFunc func() (*http.Response, error)
}

func (f *flakyDialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return f.respFunc()
}

func jsonOKResponse(id interface{}) (*http.Response, error) {
	resp, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"ok": true},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(resp))),
	}, nil
}

func TestHTTPTargetRetriesOnDialFailureUpToMaxRetries(t *testing.T) {
	rt := &flakyDialTransport{failures: 2, respFunc: func() (*http.Response, error) { return jsonOKResponse("1") }}
	target := &HTTPTarget{URL: "http://example.invalid/mcp", MaxRetries: 2, Client: &http.Client{Transport: rt}}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	sendErr := make(chan error, 1)
	go func() { sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()

	drainOne(t, ch)
	if err := <-sendErr; err != nil {
		t.Fatalf("expected Send to succeed after retrying past 2 dial failures, got: %v", err)
	}
	if rt.calls != 3 {
		t.Fatalf("expected exactly 3 attempts (2 failures + 1 success), got %d", rt.calls)
	}
}

func TestHTTPTargetStopsRetryingOnceMaxRetriesExhausted(t *testing.T) {
	rt := &flakyDialTransport{failures: 100, respFunc: func() (*http.Response, error) { return jsonOKResponse("1") }}
	target := &HTTPTarget{URL: "http://example.invalid/mcp", MaxRetries: 1, Client: &http.Client{Transport: rt}}
	_ = target.Start(context.Background())

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected Send to fail once every dial attempt fails")
	}
	if rt.calls != 2 {
		t.Fatalf("expected exactly 2 attempts (1 initial + 1 retry), got %d", rt.calls)
	}
}

func TestHTTPTargetDoesNotRetryNonDialErrors(t *testing.T) {
	rt := &flakyDialTransport{
		failures: 0,
		respFunc: func() (*http.Response, error) { return nil, errors.New("some non-dial transport failure") },
	}
	target := &HTTPTarget{URL: "http://example.invalid/mcp", MaxRetries: 5, Client: &http.Client{Transport: rt}}
	_ = target.Start(context.Background())

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected Send to fail on a non-dial error")
	}
	if rt.calls != 1 {
		t.Fatalf("expected exactly 1 attempt — a non-dial error must never be retried, regardless of MaxRetries — got %d", rt.calls)
	}
}

// TestHTTPTargetReceiveAfterStopDoesNotHang guards against a
// regression of a race where Stop nilled recvCh after closing it: a
// Receive call arriving concurrently with, or after, Stop could then
// lazily allocate a brand-new channel nothing would ever write to or
// close, hanging any `for range` over it forever (the proxy's target
// pump does exactly that). recvCh must now be allocated once and
// never nilled, so Receive after Stop always returns the same,
// already-closed channel and a range over it exits immediately.
func TestHTTPTargetReceiveAfterStopDoesNotHang(t *testing.T) {
	target := &HTTPTarget{URL: "http://example.invalid/mcp"}
	if err := target.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := target.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	ch, err := target.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected a closed channel (no frame), got an open one with a value")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Receive after Stop hung — race not fixed")
	}
}

// TestHTTPTargetForwardsNonJSONBodyUnvalidated proves HTTPTarget itself
// does no JSON validation of a response body before delivering it —
// Pass 4 item 1 ("do not introduce a second policy for HTTP"). The
// body here is garbage, Content-Type says application/json anyway (a
// misbehaving target is exactly the realistic case), and deliver still
// forwards it byte-for-byte: recognizing and dropping it is
// Interceptor.HandleTargetResponse's job (proxy.policy.silence), the
// same single code path a malformed stdio line goes through — see
// TestMalformedHTTPAndStdioResponsesTakeTheSameSilencePath in
// internal/proxy/proxy_test.go for that shared-path proof.
func TestHTTPTargetForwardsNonJSONBodyUnvalidated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "this is not json at all")
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	sendErr := make(chan error, 1)
	go func() { sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()

	frame := drainOne(t, ch)
	if err := <-sendErr; err != nil {
		t.Fatalf("expected Send to succeed (delivery, not validation, is HTTPTarget's job), got: %v", err)
	}
	if string(frame) != "this is not json at all" {
		t.Fatalf("expected the malformed body forwarded verbatim, got: %q", frame)
	}
}

// TestHTTPTargetForwardsMalformedSSEDataLineUnvalidated is the SSE
// counterpart to TestHTTPTargetForwardsNonJSONBodyUnvalidated: a
// "data:" line that isn't valid JSON is still forwarded as its own
// frame, unvalidated — same reasoning, same downstream policy.silence
// handling.
func TestHTTPTargetForwardsMalformedSSEDataLineUnvalidated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("httptest ResponseWriter does not support flushing")
		}
		_, _ = io.WriteString(w, "data: {not valid json\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	sendErr := make(chan error, 1)
	go func() { sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()

	frame := drainOne(t, ch)
	if err := <-sendErr; err != nil {
		t.Fatalf("expected Send to succeed, got: %v", err)
	}
	if string(frame) != "{not valid json" {
		t.Fatalf("expected the malformed data: line forwarded verbatim, got: %q", frame)
	}
}

// TestHTTPTargetSessionRevocationOn401SurfacesStructuredErrorAndClearsSession
// is Pass 4 item 2's middle case: a 401 on a request that carried a
// session id means the target revoked that session, not an ordinary
// auth failure (see sendOnce's doc comment). Send must return nil (the
// whole run must not die over one call's session), a structured
// JSON-RPC error addressed to that call's own id must appear on the
// receive channel instead, and the dead session id must be cleared so
// the next request goes out fresh.
func TestHTTPTargetSessionRevocationOn401SurfacesStructuredErrorAndClearsSession(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Mcp-Session-Id", "sess-doomed")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
			return
		}
		if got := r.Header.Get("Mcp-Session-Id"); got != "sess-doomed" {
			t.Errorf("expected the revoked call to still carry the session id it was sent with, got %q", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("session expired"))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()
	drainOne(t, ch) // first call's real response; establishes the session

	sendErr := make(chan error, 1)
	go func() { sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"2","method":"b"}`)) }()

	frame := drainOne(t, ch)
	if err := <-sendErr; err != nil {
		t.Fatalf("expected a session revocation to be absorbed, not returned as an error (it would end the whole run), got: %v", err)
	}

	var env map[string]interface{}
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("expected a well-formed JSON-RPC error frame, got %q: %v", frame, err)
	}
	if env["id"] != "2" {
		t.Fatalf("expected the synthetic error addressed to id=2 (the call that hit it), got %v", env["id"])
	}
	errObj, ok := env["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected an \"error\" object (a structured error, not silence), got: %s", frame)
	}
	data, ok := errObj["data"].(map[string]interface{})
	if !ok || data["reason"] != "session_revoked" {
		t.Fatalf(`expected error.data.reason == "session_revoked", got: %s`, frame)
	}

	if target.SessionID != "" {
		t.Fatalf("expected the revoked session id to be cleared, got %q", target.SessionID)
	}
}

// TestHTTPTargetSessionRevocationOn403 confirms the same handling
// applies to 403, not just 401 — the Streamable HTTP transport doesn't
// pin session revocation to one specific status code.
func TestHTTPTargetSessionRevocationOn403(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Mcp-Session-Id", "sess-doomed-403")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()
	drainOne(t, ch)

	sendErr := make(chan error, 1)
	go func() { sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"2","method":"b"}`)) }()

	frame := drainOne(t, ch)
	if err := <-sendErr; err != nil {
		t.Fatalf("expected a 403 session revocation to be absorbed, got: %v", err)
	}
	var env map[string]interface{}
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("expected a well-formed JSON-RPC error frame, got %q: %v", frame, err)
	}
	if env["id"] != "2" {
		t.Fatalf("expected the synthetic error addressed to id=2, got %v", env["id"])
	}
	if target.SessionID != "" {
		t.Fatalf("expected the revoked session id to be cleared, got %q", target.SessionID)
	}
}

// TestHTTPTarget401WithoutASessionIsAnOrdinaryError is the negative
// control: a 401 that never had a session id attached is an ordinary
// auth failure the operator needs to fix, not a revocation to recover
// from — it must surface as the usual HTTPStatusError (ending the run,
// same as any other unrecoverable transport failure), not be absorbed
// as a per-call recoverable error.
func TestHTTPTarget401WithoutASessionIsAnOrdinaryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("who are you"))
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected a 401 with no prior session to surface as an ordinary error")
	}
	var statusErr *HTTPStatusError
	if !asHTTPStatusError(err, &statusErr) {
		t.Fatalf("expected a plain *HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected StatusCode=401, got %d", statusErr.StatusCode)
	}

	select {
	case frame := <-ch:
		t.Fatalf("expected no synthetic frame delivered for an ordinary auth failure, got: %s", frame)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHTTPTargetSessionRevocationOnNotificationClearsSessionButDeliversNoFrame
// covers the JSON-RPC edge case: a notification (no id) has nothing a
// client could correlate a response to, so even though its session was
// just revoked, nothing is delivered on the receive channel — but the
// session is still cleared so the next real call starts fresh.
func TestHTTPTargetSessionRevocationOnNotificationClearsSessionButDeliversNoFrame(t *testing.T) {
	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Mcp-Session-Id", "sess-doomed-notif")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	go func() { _ = target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`)) }()
	drainOne(t, ch)

	// A notification: no "id" field at all.
	if err := target.Send([]byte(`{"jsonrpc":"2.0","method":"notifications/progress"}`)); err != nil {
		t.Fatalf("expected the revocation to be absorbed even for a notification, got: %v", err)
	}

	select {
	case frame := <-ch:
		t.Fatalf("expected no frame delivered for a notification's revoked session, got: %s", frame)
	case <-time.After(100 * time.Millisecond):
	}
	if target.SessionID != "" {
		t.Fatalf("expected the session id to be cleared even though nothing was delivered, got %q", target.SessionID)
	}
}

// TestHTTPTargetConcurrentSendsCorrelateResponsesByID is Pass 4 item 3:
// HTTPTarget.Send must be safe to call from multiple goroutines at
// once, and each response must be delivered correlated to its own
// call's id — never mixed up, never dropped — regardless of which
// call's HTTP round trip happens to finish first. This is a safety net
// for future pipelining work (the client->target pump is currently
// serial by construction — see internal/governor/run.go — so no real
// run exercises this today), not a test of current end-to-end
// behavior.
//
// The server deliberately answers the first (slow) call's request
// only after the second (fast) call has already been sent and
// answered, so a correlation bug (e.g. a shared buffer reused across
// concurrent sendOnce calls) would show up as the wrong body paired
// with the wrong id.
func TestHTTPTargetConcurrentSendsCorrelateResponsesByID(t *testing.T) {
	const slowID, fastID = "slow-1", "fast-2"
	fastArrived := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&env)
		id, _ := env["id"].(string)

		if id == slowID {
			// Hold the slow call's response until the fast call has
			// been sent and answered, so their completions interleave
			// in the order the test is named for.
			select {
			case <-fastArrived:
			case <-time.After(2 * time.Second):
			}
		} else {
			defer close(fastArrived)
		}

		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"echo": id},
		})
		_, _ = w.Write(resp)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := target.Send([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"%s","method":"slow"}`, slowID))); err != nil {
			t.Errorf("slow Send: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := target.Send([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":"%s","method":"fast"}`, fastID))); err != nil {
			t.Errorf("fast Send: %v", err)
		}
	}()

	seen := map[string]string{} // id -> echoed id, from each frame's own result.echo
	for i := 0; i < 2; i++ {
		frame := drainOne(t, ch)
		var env map[string]interface{}
		if err := json.Unmarshal(frame, &env); err != nil {
			t.Fatalf("frame %d is not valid JSON: %v (%s)", i, err, frame)
		}
		id, _ := env["id"].(string)
		result, _ := env["result"].(map[string]interface{})
		echo, _ := result["echo"].(string)
		if id != echo {
			t.Fatalf("frame correlation broke: id=%q but result.echo=%q — a response was delivered under the wrong id", id, echo)
		}
		seen[id] = echo
	}
	wg.Wait()

	if len(seen) != 2 || seen[slowID] != slowID || seen[fastID] != fastID {
		t.Fatalf("expected both %q and %q delivered once each, correctly correlated, got: %v", slowID, fastID, seen)
	}
}

func TestHTTPTargetDoesNotRetryAfterAResponseWasReceived(t *testing.T) {
	// A 500 means the request definitely reached the server and was
	// processed (however badly) — retrying could double-fire whatever
	// the request asked the server to do. MaxRetries must not apply
	// here even though it's nonzero.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL, MaxRetries: 3}
	_ = target.Start(context.Background())

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected Send to surface the 500 as an error")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 request — a received response must never be retried — got %d", calls)
	}
}

// TestHTTPTargetRejectsOversizedJSONResponseBody is a regression test
// for a v0.2.0 audit finding: sendOnce's application/json branch used
// a bare io.ReadAll(resp.Body) with no size bound at all, unlike its
// sibling error-status branches (each already wrapped in
// io.LimitReader) and deliverSSE's own per-line cap — a malicious or
// compromised target returning an arbitrarily large 2xx body could OOM
// the process. This serves a body one byte over maxJSONResponseBytes
// and confirms Send returns an error (never forwarding any of it)
// rather than reading the whole thing into memory.
func TestHTTPTargetRejectsOversizedJSONResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		written := int64(0)
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for written <= maxJSONResponseBytes {
			n, err := w.Write(chunk)
			if err != nil {
				return
			}
			written += int64(n)
		}
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	ch, _ := target.Receive()

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected Send to reject a response body larger than maxJSONResponseBytes")
	}

	select {
	case frame := <-ch:
		t.Fatalf("expected no frame delivered for an oversized body, got %d bytes", len(frame))
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHTTPTargetRefusesCrossHostRedirect is the audit's 5a fix:
// Mcp-Session-Id is a custom header that Go's default redirect handling
// would otherwise forward to any host the target redirects to (only a
// fixed set of "sensitive" headers like Authorization/Cookie are
// stripped cross-host, and Mcp-Session-Id isn't one of them). This
// confirms a cross-host redirect is refused outright — Send returns an
// error and the redirect target never receives the request at all, so
// the session header is never transmitted there.
func TestHTTPTargetRefusesCrossHostRedirect(t *testing.T) {
	var otherSrvHit bool
	var mu sync.Mutex
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		otherSrvHit = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}))
	defer otherSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherSrv.URL+"/", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL, SessionID: "secret-session-id"}
	if err := target.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := target.Receive(); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected Send to fail when the target issues a cross-host redirect")
	}
	if !strings.Contains(err.Error(), "cross-host") {
		t.Fatalf("expected a cross-host redirect error, got: %v", err)
	}

	mu.Lock()
	hit := otherSrvHit
	mu.Unlock()
	if hit {
		t.Fatal("expected the cross-host redirect target to never receive a request — the session header would have been replayed to it")
	}
}

// TestHTTPTargetFollowsSameHostRedirect confirms the fix doesn't break
// the ordinary case: a redirect to the same scheme+host+port is
// followed and the response is forwarded normally.
func TestHTTPTargetFollowsSameHostRedirect(t *testing.T) {
	var sessionHeaderOnSecondRequest string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/after-redirect" {
			sessionHeaderOnSecondRequest = r.Header.Get("Mcp-Session-Id")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"ok":true}}`))
			return
		}
		http.Redirect(w, r, "/after-redirect", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL, SessionID: "same-host-session-id"}
	if err := target.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ch, err := target.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	}()

	frame := drainOne(t, ch)
	if err := <-sendErr; err != nil {
		t.Fatalf("Send returned an error for a same-host redirect: %v", err)
	}
	var env map[string]interface{}
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("forwarded frame is not valid JSON: %v (%s)", err, frame)
	}
	if sessionHeaderOnSecondRequest != "same-host-session-id" {
		t.Fatalf("expected the session header to still be sent on a same-host redirect, got %q", sessionHeaderOnSecondRequest)
	}
}

// TestHTTPTargetRefusesMoreThanMaxRedirects confirms the redirect cap
// itself, independent of host: a target that keeps redirecting
// same-host forever must eventually be refused rather than followed
// indefinitely.
func TestHTTPTargetRefusesMoreThanMaxRedirects(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		http.Redirect(w, r, fmt.Sprintf("/hop-%d", n), http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	target := &HTTPTarget{URL: srv.URL}
	_ = target.Start(context.Background())
	_, _ = target.Receive()

	err := target.Send([]byte(`{"jsonrpc":"2.0","id":"1","method":"a"}`))
	if err == nil {
		t.Fatal("expected Send to fail once the redirect count exceeds the cap")
	}
}
