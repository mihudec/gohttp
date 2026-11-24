package client

import (
	// "fmt"
	"net/http"
	"net/url"
	"testing"
)


func TestNewRequest_BaseUrlWoQ_1(t *testing.T) {
	c := newClient(t, WithBaseURL("http://example.com"))
	req, err := c.NewRequest(ctx(t), http.MethodGet, "/example", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if req.URL.Path != "/example" {
		t.Fatal("Wrong path, expected '/example'")
	}
	
}
func TestNewRequest_BaseUrlWoQ_2(t *testing.T) {
	c := newClient(t, WithBaseURL("http://example.com"))
	req, err := c.NewRequest(ctx(t), http.MethodGet, "example", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if req.URL.Path != "/example" {
		t.Fatal("Wrong path, expected '/example'")
	}
	
}

func TestNewRequest_BaseUrlWQ_1(t *testing.T) {
	c := newClient(t, WithBaseURL("http://example.com"))
	req, err := c.NewRequest(ctx(t), http.MethodGet, "/example", url.Values{"foo": {"bar"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/example" {
		t.Fatal("Wrong path, expected '/example'")
	}
	if got := req.URL.Query().Get("foo"); got != "bar" {
		t.Fatalf("Wrong query, expected 'bar', got '%s'", got)
	}
	if got := req.URL.String(); got != "http://example.com/example?foo=bar" {
		t.Fatalf("Wrong URL, got '%s'", got)
	}
	
}

func TestNewRequest_BaseUrlWQ_2(t *testing.T) {
	c := newClient(t, WithBaseURL("http://example.com"))
	req, err := c.NewRequest(ctx(t), http.MethodGet, "/example/1?a=b", url.Values{"foo": {"bar"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/example/1" {
		t.Fatal("Wrong path, expected '/example/1'")
	}
	if got := req.URL.Query().Get("foo"); got != "bar" {
		t.Fatalf("Wrong query, expected 'bar', got '%s'", got)
	}
	if got := req.URL.Query().Get("a"); got != "b" {
		t.Fatalf("Wrong query, expected 'b', got '%s'", got)
	}
	if got := req.URL.String(); got != "http://example.com/example/1?a=b&foo=bar" {
		t.Fatalf("Wrong URL, got '%s'", got)
	}
	
}
func TestNewRequest_NoBaseUrl_1(t *testing.T) {
	c := newClient(t)
	req, err := c.NewRequest(ctx(t), http.MethodGet, "http://example.com/example", url.Values{"foo": {"bar"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "example.com" {
		t.Fatal("Wrong host, expected 'example.com'")
	}
	if req.URL.Path != "/example" {
		t.Fatal("Wrong path, expected '/example'")
	}
	if got := req.URL.Query().Get("foo"); got != "bar" {
		t.Fatalf("Wrong query, expected 'bar', got '%s'", got)
	}
	if got := req.URL.String(); got != "http://example.com/example?foo=bar" {
		t.Fatalf("Wrong URL, got '%s'", got)
	}
	
}

func TestNewRequest_NoBaseUrl_2(t *testing.T) {
	c := newClient(t)
	req, err := c.NewRequest(ctx(t), http.MethodGet, "http://example.com/example?a=b", url.Values{"foo": {"bar"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "example.com" {
		t.Fatal("Wrong host, expected 'example.com'")
	}
	if req.URL.Path != "/example" {
		t.Fatal("Wrong path, expected '/example'")
	}
	if got := req.URL.Query().Get("foo"); got != "bar" {
		t.Fatalf("Wrong query, expected 'bar', got '%s'", got)
	}
	if got := req.URL.Query().Get("a"); got != "b" {
		t.Fatalf("Wrong query, expected 'b', got '%s'", got)
	}
	if got := req.URL.String(); got != "http://example.com/example?a=b&foo=bar" {
		t.Fatalf("Wrong URL, got '%s'", got)
	}
	
}