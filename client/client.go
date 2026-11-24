package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/proxy"
)

type Authenticator interface {
	// Mutate the request to add headers/cookies/etc.
	Authorize(req *http.Request) error
}

// Option pattern for extensibility.
type Option func(*Client) error

// WithHTTPClient copies hc. Apply this before proxy or TLS options.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return fmt.Errorf("nil http client")
		}
		clone := *hc
		c.http = &clone
		return nil
	}
}

func WithBaseURL(u string) Option {
	return func(c *Client) error {
		parsed, err := url.Parse(u)
		if err != nil {
			return fmt.Errorf("invalid base URL")
		}
		if err := validateURL(parsed); err != nil {
			return err
		}
		c.baseURL = parsed
		return nil
	}
}

func WithAuthenticator(a Authenticator) Option {
	return func(c *Client) error { c.auth = a; return nil }
}

func WithUserAgent(ua string) Option {
	return func(c *Client) error { c.userAgent = ua; return nil }
}

// WithRetry sets the number of additional attempts; zero disables retries.
func WithRetry(max int) Option {
	return func(c *Client) error { c.maxRetries = max; return nil }
}

func WithRateLimiter(rl RateLimiter) Option {
	return func(c *Client) error { c.rateLimit = rl; return nil }
}

func WithLogger(logger *zap.SugaredLogger) Option {
	return func(c *Client) error { c.logger = logger; return nil }
}

// Cap the max bytes read from a successful response body.
func WithMaxBodyBytes(n int64) Option {
	return func(c *Client) error { c.maxBody = n; return nil }
}

// Minimal rate limiter contract so you can plug in x/time/rate or your own.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

// Paginator defines how to build the next request for paged APIs.
// It should inspect the last response (status/body/headers) and decide:
// - Whether there's a "next page"
// - How to modify the query params/path for the next page
type Paginator interface {
	// Next returns a modified request for the next page, or nil if done.
	Next(prevReq *http.Request, prevResp *http.Response, prevBody string) (*http.Request, error)
}

func WithPaginator(p Paginator) Option {
	return func(c *Client) error { c.paginator = p; return nil }
}

// Use a specific proxy URL. Supports http/https/socks5/socks5h.
// Examples:
//
//	"http://proxy.local:3128"
//	"http://user:pass@proxy.local:3128"
//	"socks5://127.0.0.1:1080"
//	"socks5h://user:pass@127.0.0.1:1080"
func WithProxy(raw string) Option {
	return func(c *Client) error {
		tr, err := ensureTransport(c.http.Transport)
		if err != nil {
			return err
		}
		if err := applyProxy(tr, raw); err != nil {
			return err
		}
		c.http.Transport = tr
		return nil
	}
}

// WithProxyFromEnv respects HTTP_PROXY, HTTPS_PROXY and NO_PROXY.
func WithProxyFromEnv() Option {
	return func(c *Client) error {
		tr, err := ensureTransport(c.http.Transport)
		if err != nil {
			return err
		}
		tr.Proxy = http.ProxyFromEnvironment
		c.http.Transport = tr
		return nil
	}
}

// WithTLSConfig installs a clone of tlsCfg. Nil restores default TLS settings.
func WithTLSConfig(tlsCfg *tls.Config) Option {
	return func(c *Client) error {
		tr, err := ensureTransport(c.http.Transport)
		if err != nil {
			return err
		}
		tr.TLSClientConfig = nil
		if tlsCfg != nil {
			tr.TLSClientConfig = tlsCfg.Clone()
		}
		c.http.Transport = tr
		return nil
	}
}

// WithRetryMethods replaces the methods eligible for retries. Only opt in to
// writes when repeating the operation is safe. Bodies must still be replayable.
// By default, only GET, HEAD and OPTIONS are eligible.
func WithRetryMethods(methods ...string) Option {
	return func(c *Client) error {
		c.retryMethods = make(map[string]bool, len(methods))
		for _, method := range methods {
			c.retryMethods[method] = true
		}
		return nil
	}
}

func WithInsecureTLS() Option {
	return WithTLSConfig(&tls.Config{
		InsecureSkipVerify: true, // skip server cert verification
	})
}

type Client struct {
	http         *http.Client
	baseURL      *url.URL
	auth         Authenticator
	paginator    Paginator
	userAgent    string
	maxRetries   int
	retryMethods map[string]bool
	rateLimit    RateLimiter
	logger       *zap.SugaredLogger
	maxBody      int64
}

// New validates options and returns a quiet, reusable HTTP client.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
		maxRetries:   2,
		retryMethods: map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true},
		maxBody:      8 << 20, // 8 MiB default cap on successful reads
	}
	for _, o := range opts {
		if o == nil {
			return nil, fmt.Errorf("nil option")
		}
		if err := o(c); err != nil {
			return nil, err
		}
	}
	if c.maxRetries < 0 {
		return nil, fmt.Errorf("max retries must be nonnegative")
	}
	if c.maxBody <= 0 || c.maxBody == int64(1<<63-1) {
		return nil, fmt.Errorf("max body bytes must be positive and less than MaxInt64")
	}
	if c.http.Timeout < 0 {
		return nil, fmt.Errorf("timeout must be nonnegative")
	}
	return c, nil
}

func validateURL(u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("URL must be absolute HTTP(S), without userinfo or fragment")
	}
	return nil
}

// NewRequest builds a request relative to baseURL and JSON-encodes body if non-nil.
func (c *Client) NewRequest(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	rel, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	var u url.URL
	mergedRelQuery := false

	// If c.baseURL is not set, path must be absolute
	if c.baseURL == nil {
		if err := validateURL(rel); err != nil {
			return nil, fmt.Errorf("absolute URL required when baseURL is not set: %q", path)
		}
		u = *rel
	} else {
		if rel.IsAbs() || rel.Host != "" || rel.User != nil || rel.Fragment != "" {
			return nil, fmt.Errorf("path must be relative when baseURL is set: %q", path)
		}
		u = *c.baseURL
		u.RawPath = joinPath(u.EscapedPath(), rel.EscapedPath())
		u.Path, err = url.PathUnescape(u.RawPath)
		if err != nil {
			return nil, err
		}
		mergedRelQuery = true
	}

	baseQ := u.Query()
	if mergedRelQuery && rel.RawQuery != "" {
		for k, vs := range rel.Query() {
			for _, v := range vs {
				baseQ.Add(k, v)
			}
		}
	}
	if query != nil {
		for k, vs := range query {
			for _, v := range vs {
				baseQ.Add(k, v)
			}
		}
	}
	u.RawQuery = baseQ.Encode()

	var r io.Reader
	var bodyBytes []byte
	if body != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		if err := enc.Encode(body); err != nil {
			return nil, err
		}
		bodyBytes = buf.Bytes()
		r = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(bodyBytes)), nil
		}
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	return req, nil
}

// DoRequest handles rate limit, auth, retries, and returns the response body as string.
// Also returns *http.Response for headers/status inspection; its body is closed.
// The request body is consumed and closed, including on preflight failures.
// For non-2xx it returns an *HTTPError containing status code and a trimmed body snippet.
func (c *Client) DoRequest(req *http.Request) (string, *http.Response, error) {
	if req == nil {
		return "", nil, fmt.Errorf("nil request")
	}
	if c.http == nil {
		return "", nil, fmt.Errorf("nil http client")
	}
	start := time.Now()
	sent := false
	defer func() {
		if !sent && req.Body != nil {
			_ = req.Body.Close()
		}
	}()

	// Clone headers and URL so authorization does not mutate the caller's request.
	// As with http.Client.Do, the request body is consumed and closed.
	req = req.Clone(req.Context())
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if c.auth != nil {
		if err := c.auth.Authorize(req); err != nil {
			return "", nil, err
		}
	}
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	canRetry := c.retryMethods[req.Method] && replayable
	var resp *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		if err := req.Context().Err(); err != nil {
			return "", nil, err
		}
		if c.rateLimit != nil {
			if err := c.rateLimit.Wait(req.Context()); err != nil {
				return "", nil, err
			}
		}
		if err := req.Context().Err(); err != nil {
			return "", nil, err
		}
		if attempt > 0 && req.GetBody != nil {
			req.Body, err = req.GetBody()
			if err != nil {
				return "", nil, err
			}
		}
		if c.logger != nil {
			c.logger.Debugf("HTTP %s (attempt %d)", req.Method, attempt+1)
		}
		sent = true
		resp, err = c.http.Do(req)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return "", nil, ctxErr
			}
			// Retry only transient network failures, not TLS/configuration errors.
			if !canRetry || attempt >= c.maxRetries || !transientError(err) {
				return "", resp, err
			}
			if err := waitRetry(req.Context(), backoff(attempt, 250*time.Millisecond)); err != nil {
				return "", nil, err
			}
			continue
		}
		if !canRetry || attempt >= c.maxRetries || !shouldRetry(resp.StatusCode) {
			break
		}
		_ = drain(resp.Body)
		if err := waitRetry(req.Context(), retryAfterOr(backoff(attempt, 400*time.Millisecond), resp)); err != nil {
			return "", resp, err
		}
	}

	lat := time.Since(start)

	if resp == nil {
		if err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf("no response received")
	}

	// Non-2xx: read small body for diagnostics and return HTTPError
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if c.logger != nil {
			c.logger.Errorf("HTTP error: status=%d dur=%s", resp.StatusCode, lat)
		}
		return "", resp, &HTTPError{Code: resp.StatusCode, Body: string(b)}
	}

	// 2xx: read with cap
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return "", resp, err
	}

	if int64(len(b)) > c.maxBody {
		return "", resp, &BodyTooLargeError{Limit: c.maxBody}
	}
	if c.logger != nil {
		c.logger.Debugf("HTTP %s -> %d in %s, %d bytes", req.Method, resp.StatusCode, lat, len(b))
	}

	return string(b), resp, nil
}

// DoPaginated runs the request, then uses the client's paginator to fetch
// subsequent pages until there are no more or an error occurs.
// It accumulates all page bodies into a slice.
func (c *Client) DoPaginated(req *http.Request) ([]string, []*http.Response, error) {
	if c.paginator == nil {
		return nil, nil, fmt.Errorf("no paginator configured")
	}

	var allBodies []string
	var allResps []*http.Response

	for {
		body, resp, err := c.DoRequest(req)
		if err != nil {
			return allBodies, allResps, err
		}
		allBodies = append(allBodies, body)
		allResps = append(allResps, resp)

		nextReq, err := c.paginator.Next(req, resp, body)
		if err != nil {
			return allBodies, allResps, err
		}
		if nextReq == nil {
			break // no more pages
		}
		req = nextReq
	}
	return allBodies, allResps, nil
}

// DoJSON calls DoRequest and JSON-decodes into out.
func (c *Client) DoJSON(req *http.Request, out any) (*http.Response, error) {
	body, resp, err := c.DoRequest(req)
	if err != nil {
		return resp, err
	}
	if out == nil {
		return resp, nil
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return resp, err
	}
	return resp, nil
}

// DoPlainText calls DoRequest and returns string into out.
func (c *Client) DoPlainText(req *http.Request, out *string) (*http.Response, error) {
	body, resp, err := c.DoRequest(req)
	if err != nil {
		return resp, err
	}
	if out == nil {
		return resp, nil
	}
	*out = body
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

// ---------- retry helpers ----------

func shouldRetry(code int) bool {
	return code == 408 || code == 425 || code == 429 || code >= 500
}

// BodyTooLargeError indicates that a successful response exceeded the read limit.
type BodyTooLargeError struct{ Limit int64 }

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("response body exceeds %d bytes", e.Limit)
}

func retryAfterOr(def time.Duration, resp *http.Response) time.Duration {
	ra := resp.Header.Get("Retry-After")
	if secs, err := strconv.ParseInt(ra, 10, 64); err == nil && secs >= 0 {
		if secs > int64((1<<63-1)/time.Second) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(secs) * time.Second
	}
	if date, err := http.ParseTime(ra); err == nil {
		if delay := time.Until(date); delay > 0 {
			return delay
		}
		return 0
	}
	return def
}

func backoff(attempt int, base time.Duration) time.Duration {
	// Bound exponential growth and apply full jitter.
	cap := 30 * time.Second
	for i := 0; i < attempt && base < cap; i++ {
		base *= 2
	}
	if base > cap {
		base = cap
	}
	return time.Duration(rand.Int63n(int64(base) + 1))
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func transientError(err error) bool {
	var timeout interface{ Timeout() bool }
	var temporary interface{ Temporary() bool }
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		(errors.As(err, &timeout) && timeout.Timeout()) ||
		(errors.As(err, &temporary) && temporary.Temporary())
}

// Clone transports before changing settings; never discard custom RoundTrippers.
func ensureTransport(t http.RoundTripper) (*http.Transport, error) {
	if t == nil {
		t = http.DefaultTransport
	}
	if tr, ok := t.(*http.Transport); ok && tr != nil {
		return tr.Clone(), nil
	}
	return nil, fmt.Errorf("proxy and TLS options require an *http.Transport")
}

// applyProxy configures HTTP/HTTPS or SOCKS proxies on the transport.
func applyProxy(tr *http.Transport, raw string) error {
	if raw == "" {
		// Empty → no proxy (caller could use WithProxyFromEnv instead)
		tr.Proxy = nil
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy URL")
	}
	if u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("invalid proxy URL")
	}

	switch u.Scheme {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
		// Leave DialContext as-is (direct TCP), CONNECT will be used for HTTPS.
		return nil

	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		// proxy.Direct means use direct for resolving the proxy itself.
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return err
		}

		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return fmt.Errorf("SOCKS proxy does not support context cancellation")
		}
		tr.DialContext = cd.DialContext

		// Make sure HTTP Proxy is disabled when using SOCKS.
		tr.Proxy = nil
		return nil

	default:
		return fmt.Errorf("unsupported proxy scheme: %q", u.Scheme)
	}
}
