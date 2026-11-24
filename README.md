# gohttp

A small HTTP client library with pluggable authentication, rate limiting, retries,
proxy/TLS configuration, and pagination.

```go
c, err := client.New(
    client.WithBaseURL("https://device.example"),
    client.WithAuthenticator(client.BearerToken(token)),
)
if err != nil {
    return err
}
req, err := c.NewRequest(ctx, http.MethodGet, "/api/v2/monitor/system/status", nil, nil)
if err != nil {
    return err
}
_, err = c.DoJSON(req, &result)
```

Import `github.com/mihudec/gohttp/client`. Supply credentials from your application.

## Behavior

- `New` returns `(*Client, error)` and validates configuration. Custom `Option`
  functions now return an error too.
- Defaults: 30-second timeout per HTTP attempt, two additional attempts, and an
  8 MiB successful-response limit. Use a request context deadline to bound the
  entire operation, including retries and rate-limit waits.
- Only GET, HEAD, and OPTIONS are eligible for retries by default. Requests with
  bodies require `GetBody` to replay them; `NewRequest` supplies it for JSON bodies.
  `WithRetryMethods(...)` replaces the eligible methods. Opt writes in only when
  the operation is safe to repeat. `WithRetry(0)` disables library retries.
- Retries honor cancellation, apply the rate limiter on each attempt, and support
  both seconds and HTTP dates in `Retry-After`.
- `NewFromConfig` treats `MaxRetries: 0` as no retries. Zero timeout and body limit
  select the defaults; negative values are rejected.
- Oversized successful responses return `*BodyTooLargeError`, never partial success.
  Non-2xx responses return `*HTTPError` with up to 64 KiB of diagnostic body text.
- Returned response bodies are already closed; use the response for status and
  headers. Request bodies are consumed and closed. Use a fresh request for each call.
- Logging is off by default. `WithLogger` enables status/timing logs without URLs,
  credentials, or response bodies. Returned errors can contain sensitive diagnostics;
  the application controls how it records them.
- `WithHTTPClient` copies the supplied client; apply it before TLS/proxy options.
  Those options clone standard transports and reject incompatible custom transports.
  TLS settings are cloned. Do not mutate clients or their options during use.
- Base URL paths are prefixed to request paths while preserving escaped identifiers.
  Query values from the base URL, request path, and explicit query are appended.
- `DoPaginated` collects pages in memory. Product-specific pagination and response
  interpretation belong in the consuming API client.

## Validation

```sh
go test -race ./...
go vet ./...
```
