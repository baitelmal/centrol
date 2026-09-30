package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
