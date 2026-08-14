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
// offers.
//
// An error a Handler returns carries the status the upstream itself returned,
// via yerrors.HTTPStatus, and may carry further upstream detail through the
// optional interfaces below — an error implementing none still renders, so a
// provider whose transport is not HTTP implements only what it can supply. A
// provider reports what the upstream said; what the client is shown is derived
// at the handler boundary, never set by a provider.
type Handler interface {
	Chat(ctx context.Context, cfg Config, body io.Reader) (*http.Response, error)
	Models(ctx context.Context, cfg Config) ([]Model, error)
}

// RetryAfterError carries the moment the upstream gave as its retry
// boundary, however the provider recovered it — a Retry-After header, a
// structured body field, or a quota's reset time.
type RetryAfterError interface {
	error
	RetryAfter() time.Time
}

// ErrorTyper carries the error envelope's type string for the failure.
type ErrorTyper interface {
	error
	ErrorType() string
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

// ErrServerBusy reports that the upstream declined the request because it is
// momentarily at capacity, not because it failed: the same request stands a good
// chance of succeeding shortly, on the same target. A provider raises it
// alongside — never instead of — the status the upstream sent.
const ErrServerBusy sentinelErr = "upstream server busy"

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
