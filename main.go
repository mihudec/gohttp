package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/mihudec/httpclient/pkg/client"
)



func main() {

	auth := client.APIKeyHeaders{
        Headers: []client.APIKeyHeader{
            {Header: "CF-Access-Client-ID", Value: "3c8288e3f68ed692cd16d48a1a20381f.access"},
            {Header: "CF-Access-Client-Secret", Value: "d4414db328fc3ef1c13470dd15e24e5b2f871f132122b49f33db85a805d9fd79"},
        },
    }
    // baseURL := "https://jsonplaceholder.typicode.com"
    baseURL := "https://feeds.erucom.io"
    // baseURL := "https://ifconfig.me"
    c := client.New(
		client.WithAuthenticator(auth),
        client.WithBaseURL(baseURL),
        client.WithUserAgent("httpclient-example/1.0"),
        client.WithProxy("socks5h://localhost:1080"),

    )

    // ctx := context.Background()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
    query := url.Values{}
	query.Add("category", "ioc")
	query.Add("type", "hash")
	query.Add("format", "plain")
    var out string

    // Example: GET /todos/1
    req, err := c.NewRequest(ctx, "GET", "/r/fetch", query, nil)
    // req, err := c.NewRequest(ctx, "GET", "/", query, nil)
    if err != nil {
        fmt.Fprintf(os.Stderr, "request build error: %v\n", err)
        os.Exit(1)
    }
    resp, err := c.DoPlainText(req, &out)
	for k,v := range(resp.Header) {
		fmt.Printf("Key: %s Value: %s\n", k, v)
	}
    if err != nil {
        fmt.Fprintf(os.Stderr, "response status: %d\n", resp.StatusCode)
        fmt.Fprintf(os.Stderr, "request error: %v\n", err)
        os.Exit(1)
    }
    fmt.Printf("Response: %+v\n", out)
}