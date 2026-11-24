package client

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": {"0"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestRetrySafety(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		body         bool
		replayable   bool
		optIn        bool
		want         int
	}{
		{"get", "GET", false, false, false, 2},
		{"post", "POST", true, true, false, 1},
		{"put", "PUT", true, true, false, 1},
		{"delete", "DELETE", false, false, false, 1},
		{"explicit post", "POST", true, true, true, 2},
		{"stream cannot replay", "POST", true, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			lim := &fakeLimiter{}
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				hits++
				if r.Body != nil {
					b, _ := io.ReadAll(r.Body)
					r.Body.Close()
					if string(b) != "payload" {
						t.Errorf("body = %q", b)
					}
				}
				if hits == 1 {
					return response(503, "busy"), nil
				}
				return response(200, "ok"), nil
			})}
			opts := []Option{WithHTTPClient(hc), WithRateLimiter(lim)}
			if tc.optIn {
				opts = append(opts, WithRetryMethods(tc.method))
			}
			c := newClient(t, opts...)
			var body io.Reader
			if tc.body {
				body = strings.NewReader("payload")
			}
			req, _ := http.NewRequest(tc.method, "https://example.com", body)
			if !tc.replayable {
				req.GetBody = nil
			}
			_, _, err := c.DoRequest(req)
			if hits != tc.want || int(lim.calls) != tc.want {
				t.Fatalf("hits=%d limiter=%d want=%d", hits, lim.calls, tc.want)
			}
			if (err == nil) != (tc.want == 2) {
				t.Fatalf("unexpected error %v", err)
			}
		})
	}
}

func TestCancelDuringRetryAfter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hits := 0
	c := newClient(t, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		resp := response(429, "")
		resp.Header.Set("Retry-After", "3600")
		time.AfterFunc(20*time.Millisecond, cancel)
		return resp, nil
	})}))
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com", nil)
	done := make(chan error, 1)
	go func() { _, _, err := c.DoRequest(req); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || hits != 1 {
			t.Fatalf("err=%v hits=%d", err, hits)
		}
	case <-time.After(time.Second):
		t.Fatal("retry ignored cancellation")
	}
}

func TestRetryTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{{"transient", io.ErrUnexpectedEOF, 2}, {"permanent", errors.New("bad certificate"), 1}} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			c := newClient(t, WithRetry(1), WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				hits++
				if hits == 1 {
					return nil, tc.err
				}
				return response(200, "ok"), nil
			})}))
			req, _ := http.NewRequest("GET", "https://example.com", nil)
			_, _, err := c.DoRequest(req)
			if hits != tc.want || (err == nil) != (tc.want == 2) {
				t.Fatalf("hits=%d err=%v", hits, err)
			}
		})
	}
}

func TestBodyLimitBoundary(t *testing.T) {
	for _, n := range []int{4, 5, 6} {
		c := newClient(t, WithMaxBodyBytes(5), WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, strings.Repeat("a", n)), nil })}))
		req, _ := http.NewRequest("GET", "https://example.com", nil)
		body, _, err := c.DoRequest(req)
		if n <= 5 {
			if err != nil || len(body) != n {
				t.Fatalf("n=%d body=%q err=%v", n, body, err)
			}
		} else {
			var sizeErr *BodyTooLargeError
			if !errors.As(err, &sizeErr) || body != "" {
				t.Fatalf("body=%q err=%v", body, err)
			}
		}
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, opt := range []Option{WithBaseURL("%"), WithBaseURL("example.com"), WithBaseURL("ftp://example.com"), WithProxy("wat://example.com"), WithProxy("http://"), WithRetry(-1), WithMaxBodyBytes(0), WithHTTPClient(nil)} {
		if c, err := New(opt); err == nil || c != nil {
			t.Fatalf("invalid option accepted: %v", c)
		}
	}
	if _, err := NewFromConfig(Config{BaseURL: "%"}); err == nil {
		t.Fatal("invalid config accepted")
	}
	if _, err := NewFromConfig(Config{Timeout: -1}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	c, err := NewFromConfig(Config{MaxRetries: 0})
	if err != nil || c.maxRetries != 0 {
		t.Fatalf("zero retries ignored: %v", err)
	}
}

func TestTransportOwnership(t *testing.T) {
	original := http.DefaultTransport.(*http.Transport).Clone()
	original.TLSClientConfig = &tls.Config{ServerName: "original"}
	hc := &http.Client{Transport: original}
	cfg := &tls.Config{ServerName: "device"}
	c := newClient(t, WithHTTPClient(hc), WithProxy("http://proxy.example:8080"), WithTLSConfig(cfg))
	tr := c.http.Transport.(*http.Transport)
	if hc.Transport != original || tr == original || original.TLSClientConfig.ServerName != "original" {
		t.Fatal("caller transport mutated")
	}
	cfg.ServerName = "changed"
	if tr.TLSClientConfig.ServerName != "device" {
		t.Fatal("TLS config shared")
	}
	custom := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, ""), nil })}
	if _, err := New(WithHTTPClient(custom), WithInsecureTLS()); err == nil {
		t.Fatal("custom transport silently replaced")
	}
}

func TestEscapedPaths(t *testing.T) {
	c := newClient(t, WithBaseURL("https://example.com/api%2Froot"))
	req, err := c.NewRequest(context.Background(), "GET", "/objects/a%2Fb%25c", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.URL.EscapedPath(); got != "/api%2Froot/objects/a%2Fb%25c" {
		t.Fatalf("escaped path=%s", got)
	}
	if _, err := c.NewRequest(context.Background(), "GET", "//other.example/path", nil, nil); err == nil {
		t.Fatal("accepted host in relative path")
	}
}

func TestLoggingAndRequestOwnership(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	c := newClient(t, WithLogger(zap.New(core).Sugar()), WithAuthenticator(BearerToken("secret")), WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		return response(400, "secret response"), nil
	})}))
	req, _ := http.NewRequest("GET", "https://example.com/secret-path?token=secret", nil)
	c.DoRequest(req)
	if req.Header.Get("Authorization") != "" {
		t.Fatal("caller headers mutated")
	}
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, "secret") {
			t.Fatalf("sensitive log: %s", entry.Message)
		}
	}
	quiet := newClient(t)
	if quiet.logger != nil {
		t.Fatal("default logger enabled")
	}
}

func TestRetryAfter(t *testing.T) {
	r := response(429, "")
	defer r.Body.Close()
	for _, value := range []string{"-1", "invalid"} {
		r.Header.Set("Retry-After", value)
		if retryAfterOr(time.Second, r) != time.Second {
			t.Fatal("invalid retry delay accepted")
		}
	}
	r.Header.Set("Retry-After", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	delay := retryAfterOr(0, r)
	if delay < 59*time.Minute || delay > time.Hour {
		t.Fatalf("HTTP date delay=%s", delay)
	}
}
