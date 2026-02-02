package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Authenticator interface {
	// Mutate the request to add headers/cookies/etc.
	Authorize(req *http.Request) error
}

// Option pattern for extensibility.
type Option func(*Client)

func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = mustParse(u) }
}

func WithAuthenticator(a Authenticator) Option {
	return func(c *Client) { c.auth = a }
}

func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

func WithRetry(max int) Option {
	return func(c *Client) { c.maxRetries = max }
}

func WithRateLimiter(rl RateLimiter) Option {
	return func(c *Client) { c.rateLimit = rl }
}

// Minimal rate limiter contract so you can plug in x/time/rate or your own.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

type Client struct {
	http       *http.Client
	baseURL    *url.URL
	auth       Authenticator
	userAgent  string
	maxRetries int
	rateLimit  RateLimiter
}

func New(opts ...Option) *Client {
	c := &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
		maxRetries: 2,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// NewRequest builds a request relative to baseURL and JSON-encodes body if non-nil.
func (c *Client) NewRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	u := *c.baseURL
	u.Path = joinPath(u.Path, path)
	u.RawQuery = query.Encode()

	var r io.Reader
	if body != nil {
		pr, pw := io.Pipe()
		go func() {
			enc := json.NewEncoder(pw)
			pw.CloseWithError(enc.Encode(body))
		}()
		r = pr
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	return req, nil
}

// DoJSON performs the request with auth, retry, rate-limit and decodes JSON into out.
func (c *Client) DoJSON(req *http.Request, out any) (*http.Response, error) {
	// rate limit
	if c.rateLimit != nil {
		if err := c.rateLimit.Wait(req.Context()); err != nil {
			return nil, err
		}
	}
	// auth
	if c.auth != nil {
		if err := c.auth.Authorize(req); err != nil {
			return nil, err
		}
	}

	var resp *http.Response
	var err error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		resp, err = c.http.Do(req)
		if err != nil {
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return nil, ctxErr
			}
			// transient network error – backoff and retry
			if attempt < c.maxRetries {
				time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
				continue
			}
			return nil, err
		}

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			// best-effort retry on server busy
			_ = drain(resp.Body)
			if attempt < c.maxRetries {
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
				continue
			}
		}
		break
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp, &HTTPError{Code: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		return resp, nil
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return resp, err
	}
	return resp, nil
}

func drain(rc io.ReadCloser) error {
	defer rc.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 32<<10))
	return nil
}

type HTTPError struct {
	Code int
	Body string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Code, e.Body)
}

func joinPath(a, b string) string {
	switch {
	case a == "" || a == "/":
		if b == "" {
			return "/"
		}
		if b[0] != '/' {
			return "/" + b
		}
		return b
	default:
		if a[len(a)-1] == '/' {
			if b == "" {
				return a
			}
			if b[0] == '/' {
				return a + b[1:]
			}
			return a + b
		}
		if b == "" {
			return a
		}
		if b[0] == '/' {
			return a + b
		}
		return a + "/" + b
	}
}