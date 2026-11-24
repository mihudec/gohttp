package client

import (
	"fmt"
	"net/http"
	"time"
)

// Config is a cobra/viper-friendly specification for constructing a Client.
// Tags are provided for mapstructure/json/yaml decoding.
type Config struct {
	BaseURL      string        `mapstructure:"base_url" json:"base_url" yaml:"base_url"`
	UserAgent    string        `mapstructure:"user_agent" json:"user_agent" yaml:"user_agent"`
	Timeout      time.Duration `mapstructure:"timeout" json:"timeout" yaml:"timeout"`
	MaxRetries   int           `mapstructure:"max_retries" json:"max_retries" yaml:"max_retries"`
	MaxBodyBytes int64         `mapstructure:"max_body_bytes" json:"max_body_bytes" yaml:"max_body_bytes"`
	ProxyURL     string        `mapstructure:"proxy_url" json:"proxy_url" yaml:"proxy_url"`
	ProxyFromEnv bool          `mapstructure:"proxy_from_env" json:"proxy_from_env" yaml:"proxy_from_env"`
	TLS          TLSConfig     `mapstructure:"tls" json:"tls" yaml:"tls"`
	Auth         AuthConfig    `mapstructure:"auth" json:"auth" yaml:"auth"`
}

type TLSConfig struct {
	Insecure bool `mapstructure:"insecure" json:"insecure" yaml:"insecure"`
}

type AuthConfig struct {
	Type          string               `mapstructure:"type" json:"type" yaml:"type"`
	BearerToken   string               `mapstructure:"bearer_token" json:"bearer_token" yaml:"bearer_token"`
	APIKeyHeader  string               `mapstructure:"api_key_header" json:"api_key_header" yaml:"api_key_header"`
	APIKeyValue   string               `mapstructure:"api_key_value" json:"api_key_value" yaml:"api_key_value"`
	APIKeyHeaders []APIKeyHeaderConfig `mapstructure:"api_key_headers" json:"api_key_headers" yaml:"api_key_headers"`
	CookieCSRF    CookieCSRFConfig     `mapstructure:"cookie_csrf" json:"cookie_csrf" yaml:"cookie_csrf"`
}

type APIKeyHeaderConfig struct {
	Header string `mapstructure:"header" json:"header" yaml:"header"`
	Value  string `mapstructure:"value" json:"value" yaml:"value"`
}

type CookieCSRFConfig struct {
	CookieName string `mapstructure:"cookie_name" json:"cookie_name" yaml:"cookie_name"`
	CookieVal  string `mapstructure:"cookie_val" json:"cookie_val" yaml:"cookie_val"`
	HeaderName string `mapstructure:"header_name" json:"header_name" yaml:"header_name"`
	Token      string `mapstructure:"token" json:"token" yaml:"token"`
}

// NewFromConfig builds a Client from the provided config. MaxRetries is literal:
// zero disables retries. Zero Timeout and MaxBodyBytes use the defaults.
func NewFromConfig(cfg Config) (*Client, error) {
	if cfg.Timeout < 0 || cfg.MaxBodyBytes < 0 {
		return nil, fmt.Errorf("timeout and max body bytes must be nonnegative")
	}
	var opts []Option

	if cfg.Timeout > 0 {
		opts = append(opts, WithHTTPClient(&http.Client{Timeout: cfg.Timeout}))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, WithBaseURL(cfg.BaseURL))
	}
	if cfg.UserAgent != "" {
		opts = append(opts, WithUserAgent(cfg.UserAgent))
	}
	opts = append(opts, WithRetry(cfg.MaxRetries))
	if cfg.MaxBodyBytes > 0 {
		opts = append(opts, WithMaxBodyBytes(cfg.MaxBodyBytes))
	}
	if cfg.ProxyURL != "" {
		opts = append(opts, WithProxy(cfg.ProxyURL))
	} else if cfg.ProxyFromEnv {
		opts = append(opts, WithProxyFromEnv())
	}
	if cfg.TLS.Insecure {
		opts = append(opts, WithInsecureTLS())
	}

	if cfg.Auth.Type != "" {
		auth, err := authFromConfig(cfg.Auth)
		if err != nil {
			return nil, err
		}
		if auth != nil {
			opts = append(opts, WithAuthenticator(auth))
		}
	}

	return New(opts...)
}

func authFromConfig(cfg AuthConfig) (Authenticator, error) {
	switch cfg.Type {
	case "":
		return nil, nil
	case "bearer":
		if cfg.BearerToken == "" {
			return nil, fmt.Errorf("auth.bearer_token is required")
		}
		return BearerToken(cfg.BearerToken), nil
	case "api_key":
		if cfg.APIKeyHeader == "" || cfg.APIKeyValue == "" {
			return nil, fmt.Errorf("auth.api_key_header and auth.api_key_value are required")
		}
		return APIKeyHeader{Header: cfg.APIKeyHeader, Value: cfg.APIKeyValue}, nil
	case "api_keys":
		if len(cfg.APIKeyHeaders) == 0 {
			return nil, fmt.Errorf("auth.api_key_headers must not be empty")
		}
		headers := make([]APIKeyHeader, 0, len(cfg.APIKeyHeaders))
		for _, h := range cfg.APIKeyHeaders {
			if h.Header == "" {
				return nil, fmt.Errorf("auth.api_key_headers.header is required")
			}
			headers = append(headers, APIKeyHeader{Header: h.Header, Value: h.Value})
		}
		return APIKeyHeaders{Headers: headers}, nil
	case "cookie_csrf":
		if cfg.CookieCSRF.CookieName == "" || cfg.CookieCSRF.CookieVal == "" {
			return nil, fmt.Errorf("auth.cookie_csrf.cookie_name and auth.cookie_csrf.cookie_val are required")
		}
		return CookieCSRF{
			CookieName: cfg.CookieCSRF.CookieName,
			CookieVal:  cfg.CookieCSRF.CookieVal,
			HeaderName: cfg.CookieCSRF.HeaderName,
			Token:      cfg.CookieCSRF.Token,
		}, nil
	default:
		return nil, fmt.Errorf("unknown auth.type %q", cfg.Type)
	}
}
