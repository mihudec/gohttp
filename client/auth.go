package client

import (
	"net/http"
)

// Token in Authorization header (FortiGate works well with this)
type BearerToken string
func (t BearerToken) Authorize(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+string(t))
	return nil
}

// Token via custom header (some Fortinet endpoints prefer/require API key headers)
type APIKeyHeader struct {
	Header string // e.g. "X-API-Key"
	Value    string
}
func (a APIKeyHeader) Authorize(req *http.Request) error {
	req.Header.Set(a.Header, a.Value)
	return nil
}

type APIKeyHeaders struct {
	Headers []APIKeyHeader
}

func (a APIKeyHeaders) Authorize(req *http.Request) error {
	for _, header := range(a.Headers) {
		req.Header.Set(header.Header, header.Value)
	}
	return nil
}

// Session + CSRF (often used by GUI-style APIs)
type CookieCSRF struct {
	CookieName string
	CookieVal  string
	HeaderName string // e.g. "X-CSRFTOKEN"
	Token      string
}
func (c CookieCSRF) Authorize(req *http.Request) error {
	req.AddCookie(&http.Cookie{Name: c.CookieName, Value: c.CookieVal})
	if c.HeaderName != "" && c.Token != "" {
		req.Header.Set(c.HeaderName, c.Token)
	}
	return nil
}
