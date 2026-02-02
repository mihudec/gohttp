package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ----- fakes -----

type fakeAuth struct {
	header string
	value  string
	calls  int32
	err    error
}

func (a *fakeAuth) Authorize(req *http.Request) error {
	atomic.AddInt32(&a.calls, 1)
	if a.err != nil {
		return a.err
	}
	if a.header != "" {
		req.Header.Set(a.header, a.value)
	}
	return nil
}
type fakeLimiter struct {
	calls  int32
	waitFn func(ctx context.Context) error
}

func (f *fakeLimiter) Wait(ctx context.Context) error {
	atomic.AddInt32(&f.calls, 1)
	if f.waitFn != nil {
		return f.waitFn(ctx)
	}
	return nil
}

// ----- helpers -----

func newClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	return New(opts...)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return c
}

// ----- tests -----

func TestDoJSON_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/monitor/system/status" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "7.2.6"})
	}))
	t.Cleanup(srv.Close)

	auth := &fakeAuth{header: "X-API-Key", value: "secret"}
	c := newClient(t,
		WithBaseURL(srv.URL),
		WithAuthenticator(auth),
		WithUserAgent("ut/1"),
	)

	req, err := c.NewRequest(ctx(t), http.MethodGet, "/api/v2/monitor/system/status", url.Values{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Version string `json:"version"` }
	resp, err := c.DoJSON(req, &out)
	if err != nil {
		t.Fatalf("DoJSON error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if out.Version != "7.2.6" {
		t.Fatalf("version = %q", out.Version)
	}
	if atomic.LoadInt32(&auth.calls) != 1 {
		t.Fatalf("auth called %d times", auth.calls)
	}
}

func TestDoJSON_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, WithBaseURL(srv.URL))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/x", nil, nil)

	_, err := c.DoJSON(req, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	he, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("want HTTPError, got %T", err)
	}
	if he.Code != 400 {
		t.Fatalf("code=%d", he.Code)
	}
	if !strings.Contains(he.Body, "nope") {
		t.Fatalf("body=%q", he.Body)
	}
}

func TestRetryOn500ThenSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			http.Error(w, "boom", 500)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, WithBaseURL(srv.URL), WithRetry(2))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/retry", nil, nil)

	var out map[string]any
	_, err := c.DoJSON(req, &out)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestRetryOn429WithRetryAfterZeroThenSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate", 429)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, WithBaseURL(srv.URL), WithRetry(2))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/rate", nil, nil)

	var out map[string]any
	_, err := c.DoJSON(req, &out)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestAuthenticatorAddsHeader(t *testing.T) {
	const hdr = "X-Auth"
	const val = "token123"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(hdr); got != val {
			t.Fatalf("missing auth header: got=%q", got)
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	auth := &fakeAuth{header: hdr, value: val}
	c := newClient(t, WithBaseURL(srv.URL), WithAuthenticator(auth))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/h", nil, nil)

	_, err := c.DoJSON(req, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if atomic.LoadInt32(&auth.calls) != 1 {
		t.Fatalf("auth calls=%d", auth.calls)
	}
}

func TestRateLimiterInvoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	lim := &fakeLimiter{
		waitFn: func(ctx context.Context) error { return nil },
	}

	c := newClient(t, WithBaseURL(srv.URL), WithRateLimiter(lim))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/x", nil, nil)

	_, err := c.DoJSON(req, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := atomic.LoadInt32(&lim.calls); got != 1 {
		t.Fatalf("limiter calls=%d, want 1", got)
	}
}


func TestRateLimiterErrorBubblesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit when limiter errors")
	}))
	t.Cleanup(srv.Close)

	lim := &fakeLimiter{
		waitFn: func(ctx context.Context) error { return errors.New("limited") },
	}

	c := newClient(t, WithBaseURL(srv.URL), WithRateLimiter(lim))

	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/", nil, nil)

	_, _, err := c.DoRequest(req)
	if err == nil {
		t.Fatal("expected limiter error, got nil")
	}
	if !strings.Contains(err.Error(), "limited") {
		t.Fatalf("expected 'limited' error, got %v", err)
	}
	if got := atomic.LoadInt32(&lim.calls); got != 1 {
		t.Fatalf("limiter calls=%d, want 1", got)
	}
}


func TestDoPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, WithBaseURL(srv.URL))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/plain", nil, nil)

	var s string
	_, err := c.DoPlainText(req, &s)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if s != "hello" {
		t.Fatalf("got %q", s)
	}
}

func TestMaxBodyCap(t *testing.T) {
	// return long body (100 bytes)
	body := strings.Repeat("A", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, WithBaseURL(srv.URL), WithMaxBodyBytes(5))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/cap", nil, nil)

	got, _, err := c.DoRequest(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 bytes, got %d", len(got))
	}
}

func TestNewRequest_JoinPathAndUA(t *testing.T) {
	c := newClient(t, WithBaseURL("https://example.com/api"), WithUserAgent("ua/1"))
	r, err := c.NewRequest(ctx(t), http.MethodGet, "/v2/status", url.Values{"q": {"x"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.URL.String(); got != "https://example.com/api/v2/status?q=x" {
		t.Fatalf("url=%s", got)
	}
	if got := r.Header.Get("User-Agent"); got != "ua/1" {
		t.Fatalf("ua=%s", got)
	}
}

func TestNon2xxReturnsHTTPErrorWithSnippet(t *testing.T) {
	snippet := strings.Repeat("Z", 1024) // well under 64KiB limit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, snippet, 502)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, WithBaseURL(srv.URL))
	req, _ := c.NewRequest(ctx(t), http.MethodGet, "/", nil, nil)

	_, _, err := c.DoRequest(req)
	if err == nil {
		t.Fatal("expected error")
	}
	he, ok := err.(*HTTPError)
	if !ok || he.Code != 502 {
		t.Fatalf("want HTTPError 502, got %T %+v", err, err)
	}
	if !strings.Contains(he.Body, "Z") {
		t.Fatalf("missing body snippet")
	}
}

// This is a *sanity check* for SOCKS option wiring — we don't need a real proxy.
// We assert that DialContext is set and Proxy is nil after WithProxy("socks5://...").
func TestWithProxy_SOCKS5_WiresDialContext(t *testing.T) {
	c := newClient(t, WithProxy("socks5://127.0.0.1:1080"))
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", c.http.Transport)
	}
	if tr.DialContext == nil {
		t.Fatalf("DialContext not set for SOCKS5")
	}
	if tr.Proxy != nil {
		t.Fatalf("Proxy should be nil for SOCKS5")
	}
}


func TestDoRequest_ContextDeadlineExceededBeforeLimiter(t *testing.T) {
    var hitServer atomic.Bool

    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        hitServer.Store(true)
    }))
    t.Cleanup(srv.Close)

    // Limiter that sleeps longer than our ctx deadline
    lim := &fakeLimiter{
        waitFn: func(ctx context.Context) error {
            time.Sleep(5 * time.Millisecond)
            return nil
        },
    }

    c := newClient(t,
        WithBaseURL(srv.URL),
        WithRateLimiter(lim),
    )

    ctx, cancel := context.WithTimeout(context.Background(), 500*time.Microsecond)
    defer cancel()

    req, err := c.NewRequest(ctx, http.MethodGet, "/", nil, nil)
    if err != nil {
        t.Fatal(err)
    }

    _, _, err = c.DoRequest(req)
    if err == nil {
        t.Fatal("expected error, got nil")
    }
    if !errors.Is(err, context.DeadlineExceeded) {
        t.Fatalf("expected context.DeadlineExceeded, got %v", err)
    }
    if hitServer.Load() {
        t.Fatal("server was hit but should not have been")
    }
}
