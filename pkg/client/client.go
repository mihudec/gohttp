package client

import (
	"crypto/tls"
    "net"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/proxy"

	"github.com/mihudec/httpclient/internal/logging"
)

var Logger = logging.CreateLogger(logging.ParseLogLevel("debug"), "httpclient.log")
var Suggar = Logger.Sugar()

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

func WithLogger(logger *zap.SugaredLogger) Option {
	return func(c *Client) { c.logger = logger }
}

// Cap the max bytes read from a successful response body.
func WithMaxBodyBytes(n int64) Option {
	return func(c *Client) { c.maxBody = n }
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
    return func(c *Client) { c.paginator = p }
}


// Use a specific proxy URL. Supports http/https/socks5/socks5h.
// Examples:
//  "http://proxy.local:3128"
//  "http://user:pass@proxy.local:3128"
//  "socks5://127.0.0.1:1080"
//  "socks5h://user:pass@127.0.0.1:1080"
func WithProxy(raw string) Option {
    return func(c *Client) {
        if c.http == nil {
            c.http = &http.Client{Timeout: 30 * time.Second}
        }
        tr := ensureTransport(c.http.Transport)
        if err := applyProxy(tr, raw, c.logger); err != nil && c.logger != nil {
            c.logger.Warnf("proxy setup failed: %v", err)
        }
        c.http.Transport = tr
    }
}

// Respect HTTP(S) proxy env vars (HTTP_PROXY/HTTPS_PROXY/NO_PROXY, etc.)
func WithProxyFromEnv() Option {
    return func(c *Client) {
        if c.http == nil {
            c.http = &http.Client{Timeout: 30 * time.Second}
        }
        tr := ensureTransport(c.http.Transport)
        tr.Proxy = http.ProxyFromEnvironment
        c.http.Transport = tr
    }
}

// Optional: allow customizing TLS (e.g., custom roots)
func WithTLSConfig(tlsCfg *tls.Config) Option {
    return func(c *Client) {
        if c.http == nil {
            c.http = &http.Client{Timeout: 30 * time.Second}
        }
        tr := ensureTransport(c.http.Transport)
        tr.TLSClientConfig = tlsCfg
        c.http.Transport = tr
    }
}

func WithInsecureTLS() Option {
    return WithTLSConfig(&tls.Config{
        InsecureSkipVerify: true, // skip server cert verification
    })
}


type Client struct {
	http       *http.Client
	baseURL    *url.URL
	auth       Authenticator
	paginator  Paginator
	userAgent  string
	maxRetries int
	rateLimit  RateLimiter
	logger     *zap.SugaredLogger
	maxBody    int64
}

func New(opts ...Option) *Client {
	c := &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
		maxRetries: 2,
		logger:     Suggar,
		maxBody:    8 << 20, // 8 MiB default cap on successful reads
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

// DoRequest handles rate limit, auth, retries, and returns the response body as string.
// Also returns *http.Response for callers that want headers/status inspection.
// For non-2xx it returns an *HTTPError containing status code and a trimmed body snippet.
func (c *Client) DoRequest(req *http.Request) (string, *http.Response, error) {
	start := time.Now()

	// rate limit
	if c.rateLimit != nil {
		if err := c.rateLimit.Wait(req.Context()); err != nil {
			if c.logger != nil {
				c.logger.Errorf("rate limit error: %v", err)
			}
			return "", nil, err
		}
	}

	// auth
	if c.auth != nil {
		if err := c.auth.Authorize(req); err != nil {
			if c.logger != nil {
				c.logger.Errorf("auth error: %v", err)
			}
			return "", nil, err
		}
	}

	var resp *http.Response
	var err error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if c.logger != nil {
			c.logger.Debugf("HTTP %s %s (attempt %d)", req.Method, req.URL.String(), attempt+1)
		}

		resp, err = c.http.Do(req)
		if err != nil {
			if c.logger != nil {
				c.logger.Errorf("request error (attempt %d): %v", attempt+1, err)
			}
			// context canceled/deadline?
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return "", nil, ctxErr
			}
			// transient network error – backoff and retry
			if attempt < c.maxRetries {
				time.Sleep(backoff(attempt, 250*time.Millisecond))
				continue
			}
			return "", nil, err
		}

		// Retry on 429/5xx
		if shouldRetry(resp.StatusCode) && attempt < c.maxRetries {
			if c.logger != nil {
				c.logger.Warnf("server busy/error: status=%d (attempt %d)", resp.StatusCode, attempt+1)
			}
			_ = drain(resp.Body)
			time.Sleep(retryAfterOr(backoff(attempt, 400*time.Millisecond), resp))
			continue
		}
		break
	}

	lat := time.Since(start)

	// Non-2xx: read small body for diagnostics and return HTTPError
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if c.logger != nil {
			c.logger.Errorf("HTTP error: status=%d dur=%s body=%s", resp.StatusCode, lat, string(b))
		}
		return "", resp, &HTTPError{Code: resp.StatusCode, Body: string(b)}
	}

	// 2xx: read with cap
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody))
	if err != nil {
		if c.logger != nil {
			c.logger.Errorf("read body error: %v", err)
		}
		return "", resp, err
	}

	if c.logger != nil {
		c.logger.Debugf("HTTP %s %s -> %d in %s, %d bytes",
			req.Method, req.URL.String(), resp.StatusCode, lat, len(b))
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
		if c.logger != nil {
			c.logger.Errorf("decode error: %v", err)
		}
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

func shouldRetry(code int) bool { return code == 429 || code >= 500 }

func retryAfterOr(def time.Duration, resp *http.Response) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		// seconds or HTTP-date; handle seconds
		if secs, err := strconv.Atoi(ra); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return def
}

func backoff(attempt int, base time.Duration) time.Duration {
	// exponential backoff with full jitter
	exp := time.Duration(math.Pow(2, float64(attempt))) * base
	j := time.Duration(rand.Int63n(int64(base)))
	return exp + j
}

// ---------- transport helpers ----------

// ensureTransport returns a *http.Transport, cloning defaults if nil or if the current
// transport isn't an *http.Transport.
func ensureTransport(t http.RoundTripper) *http.Transport {
    if tr, ok := t.(*http.Transport); ok && tr != nil {
        return tr
    }
    // Clone http.DefaultTransport to keep sane defaults & pooling.
    dt := http.DefaultTransport.(*http.Transport)
    clone := &http.Transport{
        Proxy:                 dt.Proxy,
        DialContext:           dt.DialContext,
        ForceAttemptHTTP2:     dt.ForceAttemptHTTP2,
        MaxIdleConns:          dt.MaxIdleConns,
        IdleConnTimeout:       dt.IdleConnTimeout,
        TLSHandshakeTimeout:   dt.TLSHandshakeTimeout,
        ExpectContinueTimeout: dt.ExpectContinueTimeout,
        TLSClientConfig:       dt.TLSClientConfig,
        DisableKeepAlives:     dt.DisableKeepAlives,
        MaxConnsPerHost:       dt.MaxConnsPerHost,
        MaxIdleConnsPerHost:   dt.MaxIdleConnsPerHost,
        ResponseHeaderTimeout: dt.ResponseHeaderTimeout,
        DisableCompression:    dt.DisableCompression,
        ProxyConnectHeader:    dt.ProxyConnectHeader,
        DialTLSContext:        dt.DialTLSContext,
        // Other fields inherit zero-values sensibly.
    }
    return clone
}

// applyProxy configures HTTP/HTTPS or SOCKS proxies on the transport.
func applyProxy(tr *http.Transport, raw string, log *zap.SugaredLogger) error {
    if raw == "" {
        // Empty → no proxy (caller could use WithProxyFromEnv instead)
        tr.Proxy = nil
        return nil
    }
    u, err := url.Parse(raw)
    if err != nil {
        return err
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

        // Prefer ContextDialer if available.
        if cd, ok := d.(proxy.ContextDialer); ok {
            tr.DialContext = cd.DialContext
        } else {
            // Wrap non-context dialer to respect ctx cancellation/deadline.
            tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
                type res struct {
                    c net.Conn
                    e error
                }
                ch := make(chan res, 1)
                go func() {
                    c, e := d.Dial(network, address)
                    ch <- res{c, e}
                }()
                select {
                case <-ctx.Done():
                    return nil, ctx.Err()
                case r := <-ch:
                    return r.c, r.e
                }
            }
        }

        // Make sure HTTP Proxy is disabled when using SOCKS.
        tr.Proxy = nil
        return nil

    default:
        return fmt.Errorf("unsupported proxy scheme: %q", u.Scheme)
    }
}