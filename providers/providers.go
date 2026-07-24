// Package providers defines types shared across all Understudy providers.
package providers

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Handler serves proxied requests for one provider type. Chat proxies a
// chat-completions request body to the upstream identified by cfg and returns
// its response. Models lists the models the upstream identified by cfg
// advertises.
type Handler interface {
	Chat(ctx context.Context, cfg Config, body io.Reader) (*http.Response, error)
	Models(ctx context.Context, cfg Config) ([]Model, error)
}

// Config carries the per-call configuration for an LLM provider call.
// BaseURL must be a valid parsed URL — callers own validation.
type Config struct {
	BaseURL    *url.URL
	APIKey     string
	HTTPClient *http.Client // optional override; nil falls back to the package-shared default
}

// Client returns cfg.HTTPClient if non-nil, otherwise the package-shared default.
func (cfg Config) Client() *http.Client {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	return defaultClient
}

// defaultClient is shared across all providers' calls that don't supply their
// own. It is tuned for LLM API roundtrips: short connect/TLS/TTFB timeouts to
// fail fast on misbehaving upstreams, but no overall Client.Timeout so
// streaming response bodies aren't cut off.
var defaultClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

// Model represents an LLM model exposed by an upstream provider.
// Fields are the union of those surfaced by any supported provider; a zero
// value means the source provider didn't supply that field.
type Model struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}
