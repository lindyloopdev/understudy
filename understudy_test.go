package understudy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gitlab.com/flimzy/testy/v2"
	"gitlab.com/flimzy/yerrors"

	"github.com/lindyloopdev/understudy/internal/slogdiff"
	"github.com/lindyloopdev/understudy/providers"
)

type stubValidator struct {
	ValidateFn func(context.Context, string) (*BackendConfig, error)
}

func (s *stubValidator) Validate(ctx context.Context, token string) (*BackendConfig, error) {
	return s.ValidateFn(ctx, token)
}

func openaiBackend(t *testing.T, baseURL, apiKey string, client *http.Client) *BackendConfig {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return &BackendConfig{Backends: map[string]Backend{
		"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: apiKey, HTTPClient: client}},
	}}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), nil))
}

// newTestServer builds a *server seeded with the default providers, so
// white-box tests that bypass New and mutate internal fields still route the
// built-in provider types.
func newTestServer(v TokenValidator) *server {
	s := newServer(v)
	for name, h := range defaultProviders() {
		s.providers[name] = h
	}
	return s
}

func TestChatCompletionsValidation(t *testing.T) {
	t.Parallel()

	type test struct {
		ctx        context.Context
		authHeader string
		validate   func(context.Context, string) (*BackendConfig, error)
		opts       []Option
		body       string
		wantStatus int
		wantBody   string
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should serve a request whose token the validator accepts", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		})
		return test{
			ctx:        t.Context(),
			authHeader: "Bearer user-token",
			validate: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			},
			wantStatus: http.StatusOK,
		}
	})

	tests.AddFunc("should reject an invalid token even when the request context is already cancelled", func(t *testing.T) test {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return test{
			ctx:        ctx,
			authHeader: "Bearer user-token",
			validate: func(context.Context, string) (*BackendConfig, error) {
				return nil, ErrInvalidToken
			},
			wantStatus: http.StatusUnauthorized,
			wantBody:   `{"error":{"message":"Unauthorized","type":"authentication_error"}}`,
		}
	})

	tests.AddFunc("should return StatusUnauthorized when validator returns non-context error", func(t *testing.T) test {
		return test{
			ctx:        t.Context(),
			authHeader: "Bearer user-token",
			validate: func(context.Context, string) (*BackendConfig, error) {
				return nil, ErrInvalidToken
			},
			wantStatus: http.StatusUnauthorized,
			wantBody:   `{"error":{"message":"Unauthorized","type":"authentication_error"}}`,
		}
	})

	tests.AddFunc("should pass empty token to validator when Authorization header is absent", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		})
		return test{
			ctx: t.Context(),
			validate: func(_ context.Context, token string) (*BackendConfig, error) {
				if token != "" {
					t.Errorf("expected empty token, got %q", token)
				}
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			},
			wantStatus: http.StatusOK,
		}
	})

	tests.Add("should route chat traffic to the provider registered under its provider type", test{
		authHeader: "Bearer user-token",
		validate: func(context.Context, string) (*BackendConfig, error) {
			u, err := url.Parse("http://acme.example/v1")
			if err != nil {
				return nil, err
			}
			return &BackendConfig{Backends: map[string]Backend{
				"acme": {ProviderType: "acme", Config: providers.Config{BaseURL: u, APIKey: "sk-acme"}},
			}}, nil
		},
		opts:       []Option{WithProvider("acme", fakeChatProvider{response: `{"id":"served-by-acme"}`})},
		body:       `{"model":"acme/frobnicator-1","messages":[{"role":"user","content":"hi"}]}`,
		wantStatus: http.StatusOK,
		wantBody:   `{"id":"served-by-acme"}`,
	})

	tests.AddFunc("should route chat traffic through a handler that overrides the OpenAI default", func(t *testing.T) test {
		decoy := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("no HTTP call expected: the override handler must serve, not the built-in OpenAI provider")
		})
		return test{
			authHeader: "Bearer user-token",
			validate: func(context.Context, string) (*BackendConfig, error) {
				u, err := url.Parse("http://openai.example/v1")
				if err != nil {
					t.Fatal(err)
				}
				return &BackendConfig{Backends: map[string]Backend{
					"openai": {ProviderType: ProviderOpenAI, Config: providers.Config{BaseURL: u, APIKey: "sk-x", HTTPClient: decoy}},
				}}, nil
			},
			opts:       []Option{WithProvider(ProviderOpenAI, fakeChatProvider{response: `{"id":"served-by-override"}`})},
			body:       `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"id":"served-by-override"}`,
		}
	})

	tests.AddFunc("should reserve no model name, rejecting default like any other undeclared model", func(t *testing.T) test {
		decoy := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("no upstream call expected: no name is reserved, so no catalog is consulted to resolve one")
		})
		return test{
			authHeader: "Bearer user-token",
			validate: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", decoy), nil
			},
			body:       `{"model":"default","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"unknown logical model \"default\"","type":"invalid_request_error"}}`,
		}
	})

	tests.AddFunc("should reject a slash-less model that names no configured logical model", func(t *testing.T) test {
		decoy := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("no upstream call expected: an unconfigured logical model must be rejected, not substituted")
		})
		return test{
			authHeader: "Bearer user-token",
			validate: func(context.Context, string) (*BackendConfig, error) {
				u, err := url.Parse("http://acme.example/v1")
				if err != nil {
					t.Fatal(err)
				}
				return &BackendConfig{Backends: map[string]Backend{
					"acme": {ProviderType: "acme", Config: providers.Config{BaseURL: u, APIKey: "sk-acme", HTTPClient: decoy}},
				}}, nil
			},
			opts: []Option{WithProvider("acme", fakeChatProvider{
				response: `{"id":"served-by-acme"}`,
				models:   []providers.Model{{ID: "frobnicator-1"}},
			})},
			body:       `{"model":"review","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"unknown logical model \"review\"","type":"invalid_request_error"}}`,
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		srv := New(&stubValidator{ValidateFn: tt.validate}, append([]Option{WithLogger(testLogger(t))}, tt.opts...)...)

		body := strings.NewReader(cmp.Or(tt.body, `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
		req, err := http.NewRequestWithContext(cmp.Or(tt.ctx, t.Context()), http.MethodPost, "/v1/chat/completions", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", tt.authHeader)
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if rr.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rr.Code, tt.wantStatus)
		}
		if d := testy.DiffJSON([]byte(tt.wantBody), rr.Body.Bytes()); d != nil {
			t.Errorf("unexpected body: %s", d)
		}
	})
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// zeroReader is an inexhaustible source of zero bytes (a /dev/zero equivalent),
// so an over-limit body can be streamed without allocating it.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestChatCompletionsRequestBodyErrors(t *testing.T) {
	t.Parallel()

	type test struct {
		body       io.Reader
		wantStatus int
	}

	tests := testy.NewTable[test]()

	tests.Add("should report client-closed when the body read fails", test{
		body:       failingBody{},
		wantStatus: statusClientClosedRequest,
	})
	tests.AddFunc("should reject an over-limit body as request-entity-too-large", func(*testing.T) test {
		return test{
			body:       io.LimitReader(zeroReader{}, maxRequestBodyBytes+1),
			wantStatus: http.StatusRequestEntityTooLarge,
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})
		srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}, WithLogger(testLogger(t)))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", tt.body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if rr.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rr.Code, tt.wantStatus)
		}
	})
}

func TestChatCompletionsHandlesResponse(t *testing.T) {
	t.Parallel()

	defaultServer := func(t *testing.T, backend testy.HTTPResponder, interceptor ResponseInterceptor) *server {
		client := testy.HTTPClient(backend)
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}
		return New(validator, WithLogger(testLogger(t)), WithResponseInterceptor(interceptor)).(*server)
	}

	type test struct {
		// ctx overrides the request context (defaults to t.Context()); set it to
		// exercise cancellation paths.
		ctx                 context.Context
		requestBody         string
		server              *server
		wantStatus          int
		wantBody            string
		wantResponseHeaders http.Header
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should proxy 200 response body to client", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-123","choices":[]}`)),
					Header:     http.Header{},
				}, nil
			}, nil),
			wantStatus:          http.StatusOK,
			wantBody:            `{"id":"chatcmpl-123","choices":[]}`,
			wantResponseHeaders: http.Header{},
		}
	})

	tests.AddFunc("should forward response headers from backend to client", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header: http.Header{
						"X-Custom-Header": []string{"custom-value"},
						"X-Request-Id":    []string{"123"},
					},
				}, nil
			}, nil),
			wantStatus: http.StatusOK,
			wantBody:   `{}`,
			wantResponseHeaders: http.Header{
				"X-Custom-Header": []string{"custom-value"},
				"X-Request-Id":    []string{"123"},
			},
		}
	})

	tests.AddFunc("should strip Authorization header from proxied backend response", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header: http.Header{
						"X-Custom-Header": []string{"custom-value"},
						"Authorization":   []string{"Bearer backend-secret"},
					},
				}, nil
			}, nil),
			wantStatus: http.StatusOK,
			wantBody:   `{}`,
			wantResponseHeaders: http.Header{
				"X-Custom-Header": []string{"custom-value"},
			},
		}
	})

	tests.AddFunc("should strip Set-Cookie header from proxied backend response", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header: http.Header{
						"X-Custom-Header": []string{"custom-value"},
						"Set-Cookie":      []string{"session=abc123; HttpOnly"},
					},
				}, nil
			}, nil),
			wantStatus: http.StatusOK,
			wantBody:   `{}`,
			wantResponseHeaders: http.Header{
				"X-Custom-Header": []string{"custom-value"},
			},
		}
	})

	tests.AddFunc("should map an upstream 5xx to 502 Bad Gateway", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Body:       io.NopCloser(strings.NewReader(`{"error":"server error"}`)),
					Header:     http.Header{},
				}, nil
			}, nil),
			wantStatus:          http.StatusBadGateway,
			wantBody:            `{"error":{"message":"Bad Gateway","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should surface the upstream error type for a non-2xx with an OpenAI error envelope", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusConflict,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{},
				}, nil
			}, nil),
			wantStatus:          http.StatusConflict,
			wantBody:            `{"error":{"message":"upstream returned status 409: slow down","type":"rate_limit_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should convert a long-Retry-After 429 to a 400 upstream_rate_limited envelope", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"600"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"upstream rate limited","type":"upstream_rate_limited"},"retry_after_ms":600000}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should convert a long-Retry-After 503 to a 400 upstream_unavailable envelope", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"service unavailable"}}`)),
					Header:     http.Header{"Retry-After": {"600"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"upstream unavailable","type":"upstream_unavailable"},"retry_after_ms":600000}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should pass through a long-Retry-After 404 instead of rejecting it", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     http.Header{"Retry-After": {"600"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"not found"}}`)),
				}, nil
			}, nil),
			wantStatus:          http.StatusNotFound,
			wantBody:            `{"error":{"message":"upstream returned status 404: not found","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should pass through a 429 whose Retry-After is within the threshold", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"90"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusTooManyRequests,
			wantBody:            `{"error":{"message":"upstream returned status 429: slow down","type":"rate_limit_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"90"}},
		}
	})

	// TODO(TODO.d/treat-an-elapsed-retry-after-as-none.md): the delay's sign is
	// unpinned. An upstream naming a moment already past yields a negative remaining
	// delay, which reaches the client as Retry-After: -649568026 here and as a
	// negative retry_after_ms in the reject body. A case belongs beside this one.

	tests.AddFunc("should relay a 503's advertised Retry-After on the 502 it answers with", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"back shortly"}}`)),
					Header:     http.Header{"Retry-After": {"30"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusBadGateway,
			wantBody:            `{"error":{"message":"Bad Gateway","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"30"}},
		}
	})

	tests.AddFunc("should synthesize a Retry-After for a 429 that lacks one", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{},
				}, nil
			}, nil),
			wantStatus:          http.StatusTooManyRequests,
			wantBody:            `{"error":{"message":"upstream returned status 429: slow down","type":"rate_limit_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"60"}},
		}
	})

	tests.AddFunc("should reject a reference naming no backend rather than reading it as an unknown one", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"target \"/gpt-4\" must be <backend>/<model>","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	// TODO(TODO.d/direct-target-reference-drops-query-overrides.md): a non-boolean
	// override — openai/gpt-4?thinking=banana — is rejected here too, and only the
	// config path covers that rule.
	tests.AddFunc("should tell the caller a reserved override is not supported rather than ignoring it", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "backend", Path: "/v1"}, APIKey: "sk-test"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"openai/gpt-4?thinking=true","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"model \"openai/gpt-4?thinking=true\": thinking=true is reserved: enabling thinking is not yet supported","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should reject a reference whose query it cannot read rather than forwarding it", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "backend", Path: "/v1"}, APIKey: "sk-test"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"openai/gpt-4?thinking=%zz","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"invalid URL escape \"%zz\"","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should reject a reference naming no model rather than asking the upstream for an empty one", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "backend", Path: "/v1"}, APIKey: "sk-test"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"openai/","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"target \"openai/\" must be <backend>/<model>","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should reject an empty model as a bad request", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{
				Backends: map[string]Backend{
					"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a"}},
				},
				Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "gpt-4"}}}},
			}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"model is required","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should return 400 with invalid_request_error when the request omits the model field", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
			}, nil),
			requestBody:         `{"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"model is required","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should return 400 with invalid_request_error when the request body is malformed", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
			}, nil),
			requestBody:         `{"model":"openai/gpt-4`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"malformed request body: unexpected EOF","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should return StatusBadGateway when backend connection fails", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			}, nil),
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"error":{"message":"Bad Gateway","type":"server_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	tests.AddFunc("should tell the caller a model whose every target is unusable cannot be served", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{
				Backends: map[string]Backend{
					"broken": {ProviderType: "openai", Config: providers.Config{APIKey: "sk-a"}},
				},
				Models: map[string]LogicalModel{"m": {Targets: []Target{
					{backend: "broken", model: "gpt-4"},
					{backend: "broken", model: "gpt-4o"},
				}}},
			}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusNotFound,
			wantBody:            `{"error":{"message":"model references unusable backend \"broken\": must provide base_url","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should tell the caller why a model declaring no targets cannot be served", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{
				Backends: map[string]Backend{
					"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a"}},
				},
				Models: map[string]LogicalModel{"empty": {Targets: []Target{}}},
			}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"empty","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusNotFound,
			wantBody:            `{"error":{"message":"logical model \"empty\" has no targets","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should render 499 when the backend call is aborted by the client", func(t *testing.T) test {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return test{
			ctx: ctx,
			server: defaultServer(t, func(r *http.Request) (*http.Response, error) {
				return nil, context.Cause(r.Context())
			}, nil),
			wantStatus: statusClientClosedRequest,
			wantBody:   `{"error":{"message":"Post \"http://backend/v1/chat/completions\": context canceled: upstream no_conn","type":"server_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	// understudy doesn't know why a request was cancelled; it surfaces the
	// caller's cause. A bare cause (no status) renders the generic 500.
	tests.AddFunc("should surface a bare cancellation cause as 500", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errors.New("lindyd: shutting down"))
		return test{
			ctx: ctx,
			server: defaultServer(t, func(r *http.Request) (*http.Response, error) {
				return nil, context.Cause(r.Context())
			}, nil),
			wantStatus:          http.StatusInternalServerError,
			wantBody:            `{"error":{"message":"Internal Server Error","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	// When the caller's cause carries its own HTTP status, understudy surfaces
	// that — this is how a consumer renders e.g. 503 on shutdown, with no
	// shutdown concept inside understudy.
	tests.AddFunc("should surface a cancellation cause's own HTTP status", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(yerrors.WithHTTPStatus(http.StatusServiceUnavailable, errors.New("lindyd: shutting down")))
		return test{
			ctx: ctx,
			server: defaultServer(t, func(r *http.Request) (*http.Response, error) {
				return nil, context.Cause(r.Context())
			}, nil),
			wantStatus:          http.StatusServiceUnavailable,
			wantBody:            `{"error":{"message":"Service Unavailable","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should render 504 when the backend call times out", func(t *testing.T) test {
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
		t.Cleanup(cancel)
		return test{
			ctx: ctx,
			server: defaultServer(t, func(r *http.Request) (*http.Response, error) {
				return nil, context.Cause(r.Context())
			}, nil),
			wantStatus: http.StatusGatewayTimeout,
			wantBody:   `{"error":{"message":"Gateway Timeout","type":"server_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	tests.AddFunc("should return 401 with JSON body when validator returns ErrInvalidToken", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return nil, ErrInvalidToken
		}}
		return test{
			server:     New(validator, WithLogger(testLogger(t))).(*server),
			wantStatus: http.StatusUnauthorized,
			wantBody:   `{"error":{"message":"Unauthorized","type":"authentication_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	tests.AddFunc("should return 500 when validator returns non-sentinel error", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return nil, errors.New("db down")
		}}
		return test{
			server:     New(validator, WithLogger(testLogger(t))).(*server),
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":{"message":"Internal Server Error","type":"server_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	tests.AddFunc("should return 503 when the validator error carries a 503 status", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return nil, yerrors.WithHTTPStatus(http.StatusServiceUnavailable, errors.New("db down"))
		}}
		return test{
			server:     New(validator, WithLogger(testLogger(t))).(*server),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"error":{"message":"Service Unavailable","type":"server_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	tests.AddFunc("should tell the caller no backend is configured to serve the model it named", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: nil}, nil
		}}
		return test{
			server:      New(validator, WithLogger(testLogger(t))).(*server),
			requestBody: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:  http.StatusNotFound,
			wantBody:    `{"error":{"message":"no backend configured to serve model \"gpt-4\"","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	tests.AddFunc("should return 404 with invalid_request_error when the model names an unconfigured backend", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
			}, nil),
			requestBody:         `{"model":"ghost/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusNotFound,
			wantBody:            `{"error":{"message":"model references unknown backend \"ghost\"","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should return 404 when the model names an absent backend and the only configured one is unusable", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"broken": {ProviderType: "openai"},
			}}, nil
		}}
		return test{
			server:     New(validator, WithLogger(testLogger(t))).(*server),
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"model references unknown backend \"openai\"","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	tests.AddFunc("should render a panicking handler as a 500 server_error envelope", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"x","choices":[]}`)), Header: http.Header{}}, nil
			}, func(context.Context, RequestMetadata, *http.Response) error {
				panic("boom: usage rewrite exploded")
			}),
			wantStatus:          http.StatusInternalServerError,
			wantBody:            `{"error":{"message":"Internal Server Error","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should name the reason a configured backend is unusable rather than call it unknown", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: nil, APIKey: "sk-test"}},
			}}, nil
		}}
		return test{
			server:     New(validator, WithLogger(testLogger(t))).(*server),
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"model references unusable backend \"openai\": must provide base_url","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{
				"Content-Type": {"application/json"},
			},
		}
	})

	// Any target order must answer from "good", since "broken" can serve nothing.
	servedByGood := func(t *testing.T, targets ...Target) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-good","choices":[]}`)), Header: http.Header{}}, nil
		})
		u, err := url.Parse("http://good/v1")
		if err != nil {
			t.Fatal(err)
		}
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{
				Backends: map[string]Backend{
					"good":   {ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: "sk-good", HTTPClient: client}},
					"broken": {ProviderType: "openai", Config: providers.Config{BaseURL: nil, APIKey: "sk-bad"}},
				},
				Models: map[string]LogicalModel{"m": {Targets: targets}},
			}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).(*server),
			requestBody:         `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusOK,
			wantBody:            `{"id":"from-good","choices":[]}`,
			wantResponseHeaders: http.Header{},
		}
	}

	tests.AddFunc("should serve from the next target when the first target's backend has no base URL", func(t *testing.T) test {
		return servedByGood(t, Target{backend: "broken", model: "gpt-4"}, Target{backend: "good", model: "gpt-4"})
	})

	tests.AddFunc("should serve from the usable target when a sibling backend has no base URL", func(t *testing.T) test {
		return servedByGood(t, Target{backend: "good", model: "gpt-4"}, Target{backend: "broken", model: "gpt-4"})
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		srv := tt.server

		ctx := tt.ctx
		if ctx == nil {
			ctx = t.Context()
		}
		reqBody := cmp.Or(tt.requestBody, `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if rr.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rr.Code, tt.wantStatus)
		}
		if d := testy.DiffJSON([]byte(tt.wantBody), rr.Body.Bytes()); d != nil {
			t.Errorf("unexpected body: %s", d)
		}
		if d := gocmp.Diff(tt.wantResponseHeaders, rr.Header()); d != "" {
			t.Errorf("unexpected response headers: %s", d)
		}
	})
}

func TestChatCompletionsStillServesRequestsAfterOnePanics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[]}`)),
				Header:     make(http.Header),
			}, nil
		})

		// panicNext makes only the first served response panic, so the follow-up
		// request exercises the capacity the panicking one was holding.
		var panicNext atomic.Bool
		panicNext.Store(true)

		srv := newServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}})
		srv.providers = defaultProviders()
		srv.logger = testLogger(t)
		srv.maxConcurrentPerUpstream = 1
		srv.interceptor = func(context.Context, RequestMetadata, *http.Response) error {
			if panicNext.Swap(false) {
				panic("boom: usage rewrite exploded")
			}
			return nil
		}

		post := func() *httptest.ResponseRecorder {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				return nil
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}

		if got := post(); got != nil && got.Code != http.StatusInternalServerError {
			t.Fatalf("panicking request: got %d, want %d", got.Code, http.StatusInternalServerError)
		}

		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- post() }()
		synctest.Wait()

		select {
		case rec := <-done:
			if rec != nil && rec.Code != http.StatusOK {
				t.Errorf("request after a panicking one: got %d, want %d", rec.Code, http.StatusOK)
			}
		default:
			t.Error("request after a panicking one is starved: it never reached the upstream")
		}
	})
}

// TODO(TODO.d/degrade-past-a-misconfigured-backend.md): "should return 500 when no
// backend configured" is the last case here that fails the listing, which reads
// against emptiness being a valid answer whatever its cause.
func TestModels(t *testing.T) {
	t.Parallel()

	// modelsClient answers any catalog fetch with a one-model list, so a backend a
	// case declares usable behaves usably without reaching the network.
	modelsClient := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"gpt-4","created":1234567890,"owned_by":"openai"}]}`)),
			Header:     http.Header{"Content-Type": {"application/json"}},
		}, nil
	})

	type test struct {
		authHeader string
		validator  TokenValidator
		opts       []Option
		wantStatus int
		wantBody   string
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should omit a backend whose provider type has no registered handler", func(t *testing.T) test {
		zeta, err := url.Parse("http://zeta.example/v1")
		if err != nil {
			t.Fatal(err)
		}
		alpha, err := url.Parse("http://alpha.example/v1")
		if err != nil {
			t.Fatal(err)
		}
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return &BackendConfig{Backends: map[string]Backend{
						"alpha": {ProviderType: "unregistered", Config: providers.Config{BaseURL: alpha, APIKey: "sk-a"}},
						"zeta":  {ProviderType: "acme", Config: providers.Config{BaseURL: zeta, APIKey: "sk-z"}},
					}}, nil
				},
			},
			opts: []Option{WithProvider("acme", fakeChatProvider{
				models: []providers.Model{{ID: "zeta-gpt", Created: 1234567890, OwnedBy: "acme"}},
			})},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"zeta/zeta-gpt","created":1234567890,"owned_by":"acme"}]}`,
		}
	})

	tests.AddFunc("should list models from the provider registered under the backend's provider type", func(t *testing.T) test {
		decoy := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"wrong-catalog"}]}`)),
				Header:     http.Header{"Content-Type": {"application/json"}},
			}, nil
		})
		u, err := url.Parse("http://acme.example/v1")
		if err != nil {
			t.Fatal(err)
		}
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return &BackendConfig{Backends: map[string]Backend{
						"acme": {ProviderType: "acme", Config: providers.Config{BaseURL: u, APIKey: "sk-acme", HTTPClient: decoy}},
					}}, nil
				},
			},
			opts: []Option{WithProvider("acme", fakeChatProvider{
				models: []providers.Model{{ID: "frobnicator-1", Created: 1234567890, OwnedBy: "acme"}},
			})},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"acme/frobnicator-1","created":1234567890,"owned_by":"acme"}]}`,
		}
	})

	tests.AddFunc("should return model list from upstream", func(t *testing.T) test {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4","created":1234567890,"owned_by":"openai"}]}`)
		}))
		t.Cleanup(server.Close)
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return openaiBackend(t, server.URL, "sk-models", nil), nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"openai/gpt-4","created":1234567890,"owned_by":"openai"}]}`,
		}
	})

	tests.Add("should return 401 when validator returns ErrInvalidToken", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return nil, ErrInvalidToken
			},
		},
		wantStatus: http.StatusUnauthorized,
		wantBody:   `{"error":{"message":"Unauthorized","type":"authentication_error"}}`,
	})

	tests.Add("should return 500 when validator returns a non-sentinel error", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return nil, errors.New("database unavailable")
			},
		},
		wantStatus: http.StatusInternalServerError,
		wantBody:   `{"error":{"message":"Internal Server Error","type":"server_error"}}`,
	})

	tests.Add("should return 500 when a configured backend has no base URL", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: map[string]Backend{
					"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: nil, APIKey: "sk-models"}},
				}}, nil
			},
		},
		wantStatus: http.StatusInternalServerError,
		wantBody:   `{"error":{"message":"Internal Server Error","type":"server_error"}}`,
	})

	tests.Add("should return 500 when no backend configured", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: nil}, nil
			},
		},
		wantStatus: http.StatusInternalServerError,
		wantBody:   `{"error":{"message":"Internal Server Error","type":"server_error"}}`,
	})

	tests.AddFunc("should list the usable backend's models when another backend has a nil config", func(t *testing.T) test {
		u, err := url.Parse("http://backend/v1")
		if err != nil {
			t.Fatal(err)
		}
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return &BackendConfig{Backends: map[string]Backend{
						"usable": {ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: "sk-ok", HTTPClient: modelsClient}},
						"broken": {ProviderType: "openai"}, // nil Config
					}}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"usable/gpt-4","created":1234567890,"owned_by":"openai"}]}`,
		}
	})

	tests.AddFunc("should list the usable backend's models when another backend has no base URL", func(t *testing.T) test {
		u, err := url.Parse("http://backend/v1")
		if err != nil {
			t.Fatal(err)
		}
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return &BackendConfig{Backends: map[string]Backend{
						"usable": {ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: "sk-ok", HTTPClient: modelsClient}},
						"broken": {ProviderType: "openai", Config: providers.Config{BaseURL: nil, APIKey: "sk-bad"}},
					}}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"usable/gpt-4","created":1234567890,"owned_by":"openai"}]}`,
		}
	})

	tests.AddFunc("should answer an empty listing when the only backend's catalog fetch fails", func(t *testing.T) test {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
		}))
		t.Cleanup(server.Close)
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return openaiBackend(t, server.URL, "sk-models", nil), nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":null}`,
		}
	})

	tests.AddFunc("should answer an empty listing when the only backend's catalog response is unparseable", func(t *testing.T) test {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return openaiBackend(t, server.URL, "sk-models", nil), nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":null}`,
		}
	})

	tests.AddFunc("should namespace each model id with its backend name", func(t *testing.T) test {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4","created":1234567890,"owned_by":"openai"}]}`)
		}))
		t.Cleanup(server.Close)
		u, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return &BackendConfig{Backends: map[string]Backend{
						"groq": {ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: "sk-models"}},
					}}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"groq/gpt-4","created":1234567890,"owned_by":"openai"}]}`,
		}
	})

	tests.AddFunc("should pass token to validator with case-insensitive bearer scheme", func(t *testing.T) test {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4","created":1234567890,"owned_by":"openai"}]}`)
		}))
		t.Cleanup(server.Close)
		return test{
			authHeader: "bearer some-token",
			validator: &stubValidator{
				ValidateFn: func(_ context.Context, token string) (*BackendConfig, error) {
					if token != "some-token" {
						t.Errorf("expected token %q, got %q", "some-token", token)
						return nil, ErrInvalidToken
					}
					return openaiBackend(t, server.URL, "sk-models", nil), nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"openai/gpt-4","created":1234567890,"owned_by":"openai"}]}`,
		}
	})

	tests.Add("should reject malformed Authorization header without calling validator", test{
		authHeader: "foo",
		validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			t.Error("validator should not be called")
			return nil, nil
		}},
		wantStatus: http.StatusUnauthorized,
		wantBody:   `{"error":{"message":"Unauthorized","type":"authentication_error"}}`,
	})

	// TODO(TODO.d/degrade-past-a-misconfigured-backend.md): the failed fetch reaches
	// the operator only through s.logger, and no case here asserts that line.
	tests.AddFunc("should list the models of the backends that answer when another backend's catalog fetch fails", func(t *testing.T) test {
		sick := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"catalog unavailable"}}`)),
				Header:     http.Header{"Content-Type": {"application/json"}},
			}, nil
		})
		good, err := url.Parse("http://good.example/v1")
		if err != nil {
			t.Fatal(err)
		}
		down, err := url.Parse("http://down.example/v1")
		if err != nil {
			t.Fatal(err)
		}
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return &BackendConfig{Backends: map[string]Backend{
						"good": {ProviderType: "openai", Config: providers.Config{BaseURL: good, APIKey: "sk-good", HTTPClient: modelsClient}},
						"down": {ProviderType: "openai", Config: providers.Config{BaseURL: down, APIKey: "sk-down", HTTPClient: sick}},
					}}, nil
				},
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"object":"list","data":[{"id":"good/gpt-4","created":1234567890,"owned_by":"openai"}]}`,
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		srv := New(tt.validator, append([]Option{WithLogger(testLogger(t))}, tt.opts...)...)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", cmp.Or(tt.authHeader, "Bearer user-token"))

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if rr.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rr.Code, tt.wantStatus)
		}
		if d := testy.DiffJSON([]byte(tt.wantBody), rr.Body.Bytes()); d != nil {
			t.Errorf("unexpected body: %s", d)
		}
	})
}

func TestChatCompletionsRoutesByBackendName(t *testing.T) {
	t.Parallel()

	type test struct {
		backendNames []string
		requestBody  string
		wantStatus   int
		wantBackend  string
	}

	tests := testy.NewTable[test]()

	tests.Add("should return 404 when the model names an unconfigured backend", test{
		backendNames: []string{"openai"},
		requestBody:  `{"model":"ghost/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		wantStatus:   http.StatusNotFound,
	})

	tests.Add("should return 400 when the request omits the model field", test{
		backendNames: []string{"openai"},
		requestBody:  `{"messages":[{"role":"user","content":"hi"}]}`,
		wantStatus:   http.StatusBadRequest,
	})

	tests.Add("should route to the named backend when several are configured", test{
		backendNames: []string{"alpha", "beta"},
		requestBody:  `{"model":"beta/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		wantStatus:   http.StatusOK,
		wantBackend:  "beta",
	})

	tests.Add("should route to a different named backend", test{
		backendNames: []string{"alpha", "beta"},
		requestBody:  `{"model":"alpha/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		wantStatus:   http.StatusOK,
		wantBackend:  "alpha",
	})

	tests.Add("should return 400 when the request body is malformed", test{
		backendNames: []string{"openai"},
		requestBody:  `{"model":"openai/gpt-4`,
		wantStatus:   http.StatusBadRequest,
		wantBackend:  "",
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		var gotBackend string
		backends := map[string]Backend{}
		for _, name := range tt.backendNames {
			client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
				gotBackend = name
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			})
			backends[name] = Backend{ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: name, Path: "/v1"}, APIKey: "sk-" + name, HTTPClient: client}}
		}
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: backends}, nil
		}}
		srv := New(validator, WithLogger(testLogger(t)))

		body := strings.NewReader(tt.requestBody)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if rr.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rr.Code, tt.wantStatus)
		}
		if gotBackend != tt.wantBackend {
			t.Errorf("routed to backend %q, want %q", gotBackend, tt.wantBackend)
		}
	})
}

func forwardedModel(t *testing.T, body []byte) string {
	t.Helper()
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("could not parse forwarded body: %v", err)
	}
	var model string
	if err := json.Unmarshal(parsed["model"], &model); err != nil {
		t.Fatalf("could not parse model field: %v", err)
	}
	return model
}

func TestChatCompletionsForwardedModel(t *testing.T) {
	t.Parallel()

	type test struct {
		requestModel string
		validator    TokenValidator
		check        func()
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should strip the backend prefix from a namespaced model", func(t *testing.T) test {
		var forwarded []byte
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			forwarded, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		})
		return test{
			requestModel: "openai/gpt-4",
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			check: func() {
				if got := forwardedModel(t, forwarded); got != "gpt-4" {
					t.Errorf("forwarded model: got %q, want %q", got, "gpt-4")
				}
			},
		}
	})

	tests.AddFunc("should serve a reference carrying an override it does not recognize", func(t *testing.T) test {
		var forwarded []byte
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			forwarded, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		})
		return test{
			requestModel: "openai/gpt-4?foo=bar",
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			check: func() {
				if got := forwardedModel(t, forwarded); got != "gpt-4" {
					t.Errorf("forwarded model: got %q, want %q", got, "gpt-4")
				}
				if bytes.Contains(forwarded, []byte(`"thinking"`)) {
					t.Errorf("forwarded body injected thinking for an unrecognized override: %s", forwarded)
				}
			},
		}
	})

	tests.AddFunc("should strip a reference's overrides from the model it forwards", func(t *testing.T) test {
		var forwarded []byte
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			forwarded, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		})
		return test{
			requestModel: "openai/gpt-4?thinking=false",
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			check: func() {
				if got := forwardedModel(t, forwarded); got != "gpt-4" {
					t.Errorf("forwarded model: got %q, want %q", got, "gpt-4")
				}
			},
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		srv := New(tt.validator, WithLogger(testLogger(t)))

		body := strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, tt.requestModel))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		srv.ServeHTTP(httptest.NewRecorder(), req)
		tt.check()
	})
}

func forwardedThinkingType(t *testing.T, body []byte) string {
	t.Helper()
	type thinkingField struct {
		Type string `json:"type"`
	}
	var parsed struct {
		Thinking thinkingField `json:"thinking"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("could not parse forwarded body: %v", err)
	}
	return parsed.Thinking.Type
}

func TestChatCompletionsThinkingInjection(t *testing.T) {
	t.Parallel()

	type test struct {
		requestBody string
		targetQuery url.Values
		wantType    string
		wantKeys    int
	}

	tests := testy.NewTable[test]()

	tests.Add("should inject disabled when the request omits thinking and the target disables it", test{
		requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery: url.Values{"thinking": {"false"}},
		wantType:    "disabled",
		wantKeys:    1,
	})
	tests.Add("should apply an override carried by a backend/model reference the request names", test{
		requestBody: `{"model":"zai/glm-5?thinking=false","messages":[{"role":"user","content":"hi"}]}`,
		wantType:    "disabled",
		wantKeys:    1,
	})
	tests.Add("should override a request's enabled thinking when the target disables it", test{
		requestBody: `{"thinking":{"type":"enabled"},"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery: url.Values{"thinking": {"false"}},
		wantType:    "disabled",
		wantKeys:    1,
	})
	tests.Add("should collapse an already-disabled request to a single thinking key", test{
		requestBody: `{"thinking":{"type":"disabled"},"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery: url.Values{"thinking": {"false"}},
		wantType:    "disabled",
		wantKeys:    1,
	})
	tests.Add("should leave thinking untouched when the target does not disable it", test{
		requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery: nil,
		wantType:    "",
		wantKeys:    0,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		var forwarded []byte
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			forwarded, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		})
		u, err := url.Parse("http://backend/v1")
		if err != nil {
			t.Fatal(err)
		}
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{
				Backends: map[string]Backend{"zai": {ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: "sk-zai", HTTPClient: client}}},
				Models:   map[string]LogicalModel{"m": {Targets: []Target{{backend: "zai", model: "glm-5", query: tt.targetQuery}}}},
			}, nil
		}}
		srv := New(validator, WithLogger(testLogger(t)))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.requestBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		srv.ServeHTTP(httptest.NewRecorder(), req)

		if got := forwardedThinkingType(t, forwarded); got != tt.wantType {
			t.Errorf("forwarded thinking type: got %q, want %q", got, tt.wantType)
		}
		if got := bytes.Count(forwarded, []byte(`"thinking"`)); got != tt.wantKeys {
			t.Errorf("forwarded thinking key count: got %d, want %d", got, tt.wantKeys)
		}
	})
}

func TestChatCompletionsPrefixScanTripwire(t *testing.T) {
	t.Parallel()

	type test struct {
		requestBody  string
		wantStatus   int
		wantErrorLog []string
	}

	tests := testy.NewTable[test]()

	tests.Add("should log ERROR and still proxy normally when the model field is beyond the 64KiB prefix scan", test{
		requestBody:  `{"filler":"` + strings.Repeat("x", 64*1024+1) + `","model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		wantStatus:   http.StatusOK,
		wantErrorLog: []string{"model field beyond prefix scan threshold"},
	})

	tests.Add("should not log when the model field is within the prefix scan", test{
		requestBody:  `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		wantStatus:   http.StatusOK,
		wantErrorLog: nil,
	})

	tests.Add("should return 400 when a body with no model key buffers past the prefix scan", test{
		requestBody:  `{"filler":"` + strings.Repeat("x", 64*1024+1) + `"}`,
		wantStatus:   http.StatusBadRequest,
		wantErrorLog: nil,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","choices":[]}`)),
				Header:     make(http.Header),
			}, nil
		})

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}
		srv := New(validator, WithLogger(logger))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.requestBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if rr.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rr.Code, tt.wantStatus)
		}

		var errorMsgs []string
		for _, line := range bytes.Split(bytes.TrimRight(logBuf.Bytes(), "\n"), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatalf("could not parse log line %q: %v", line, err)
			}
			if record["level"] == "ERROR" {
				errorMsgs = append(errorMsgs, record["msg"].(string))
			}
		}

		if d := gocmp.Diff(tt.wantErrorLog, errorMsgs); d != "" {
			t.Errorf("ERROR log messages mismatch (-want +got):\n%s", d)
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestIdleReader(t *testing.T) {
	t.Parallel()

	type test struct {
		r        *idleReader
		wantN    int
		wantErrs []error
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should surface errStreamIdle when a read blocks past the idle window", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		blocked := readerFunc(func([]byte) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})
		return test{
			r:        &idleReader{r: blocked, idle: 10 * time.Millisecond, ctx: ctx, cancel: cancel},
			wantN:    0,
			wantErrs: []error{errStreamIdle},
		}
	})

	tests.AddFunc("should pass io.EOF through unchanged", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		return test{
			r:        &idleReader{r: errReader{err: io.EOF}, idle: time.Hour, ctx: ctx, cancel: cancel},
			wantN:    0,
			wantErrs: []error{io.EOF},
		}
	})

	tests.AddFunc("should return a successful read unchanged without tripping the watchdog", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		return test{
			r:        &idleReader{r: strings.NewReader("hello"), idle: time.Hour, ctx: ctx, cancel: cancel},
			wantN:    5,
			wantErrs: []error{nil},
		}
	})

	tests.AddFunc("should surface a genuine read error even when the context is concurrently cancelled", func(t *testing.T) test {
		readErr := errors.New("connection reset")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(context.Canceled) // ctx cancelled coincidentally, distinct from the read error
		return test{
			r:        &idleReader{r: readerFunc(func([]byte) (int, error) { return 0, readErr }), idle: time.Hour, ctx: ctx, cancel: cancel},
			wantN:    0,
			wantErrs: []error{readErr},
		}
	})

	tests.AddFunc("should recover the cause when a read is aborted by a deadline", func(t *testing.T) test {
		deadlineCause := errors.New("upstream deadline")
		ctx, ctxCancel := context.WithDeadlineCause(t.Context(), time.Now().Add(-time.Hour), deadlineCause)
		t.Cleanup(ctxCancel)
		reader := readerFunc(func([]byte) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})
		return test{
			r:        &idleReader{r: reader, idle: time.Hour, ctx: ctx, cancel: func(error) {}},
			wantN:    0,
			wantErrs: []error{deadlineCause, context.DeadlineExceeded},
		}
	})

	tests.AddFunc("should pass a successful read through even when the context is already cancelled", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errStreamIdle)
		reader := readerFunc(func(p []byte) (int, error) { return copy(p, "ok"), nil })
		return test{
			r:        &idleReader{r: reader, idle: time.Hour, ctx: ctx, cancel: cancel},
			wantN:    2,
			wantErrs: []error{nil},
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		n, err := tt.r.Read(make([]byte, 8))
		for _, want := range tt.wantErrs {
			if !errors.Is(err, want) {
				t.Errorf("error chain missing %v: got %v", want, err)
			}
		}
		if n != tt.wantN {
			t.Errorf("unexpected n: got %d, want %d", n, tt.wantN)
		}
	})
}

func TestIdleReaderDoesNotDoubleWrapPlainCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(t.Context())
	readErr := fmt.Errorf("read aborted: %w", context.Canceled)
	cancel(context.Canceled)
	r := &idleReader{
		r:      readerFunc(func([]byte) (int, error) { return 0, readErr }),
		idle:   time.Hour,
		ctx:    ctx,
		cancel: cancel,
	}

	_, err := r.Read(make([]byte, 8))
	//nolint:errorlint // identity is the assertion: errors.Is can't tell the raw error from the redundant double-wrap this guards against.
	if err != readErr {
		t.Errorf("plain cancellation should pass the read error through unwrapped: got %v (%T), want the identical readErr value", err, err)
	}
}

func TestIdleReaderRearmsTimerPerRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)

		block := false
		reader := readerFunc(func(p []byte) (int, error) {
			if !block {
				return copy(p, "hi"), nil
			}
			<-ctx.Done()
			return 0, ctx.Err()
		})
		r := &idleReader{r: reader, idle: 10 * time.Millisecond, ctx: ctx, cancel: cancel}

		// A successful read must stop its own timer; advancing past the idle
		// window with no read in flight must therefore leave ctx uncancelled.
		if _, err := r.Read(make([]byte, 8)); err != nil {
			t.Fatalf("first read: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		if cause := context.Cause(ctx); cause != nil {
			t.Fatalf("watchdog fired after a successful read; timer not stopped: %v", cause)
		}

		// The next read arms a fresh timer; a stall past the window still
		// yields errStreamIdle.
		block = true
		if _, err := r.Read(make([]byte, 8)); !errors.Is(err, errStreamIdle) {
			t.Errorf("re-armed read: error chain missing errStreamIdle: %v", err)
		}
	})
}

func TestRewriteModel(t *testing.T) {
	t.Parallel()

	type test struct {
		reader   io.Reader
		replace  func(string) (string, error)
		wantBody string
		wantErr  string
	}

	tests := testy.NewTable[test]()

	tests.Add("should apply the transform to the model value while preserving the body byte-for-byte", test{
		reader: strings.NewReader(`{"model": "openai/gpt-4","temperature":3.141592653589793238462643383,"messages":[{"role":"user","content":"hi \"there\"\né"}]}`),
		replace: func(m string) (string, error) {
			return strings.TrimPrefix(m, "openai/"), nil
		},
		wantBody: `{"model": "gpt-4","temperature":3.141592653589793238462643383,"messages":[{"role":"user","content":"hi \"there\"\né"}]}`,
	})

	tests.Add("should rewrite a model that is the last top-level key", test{
		reader: strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"model":"openai/gpt-4"}`),
		replace: func(m string) (string, error) {
			return strings.TrimPrefix(m, "openai/"), nil
		},
		wantBody: `{"messages":[{"role":"user","content":"hi"}],"model":"gpt-4"}`,
	})

	tests.Add("should pass the body through unchanged when there is no top-level model key", test{
		reader: strings.NewReader(`{"a":1,"b":"c"}`),
		replace: func(string) (string, error) {
			return "", errors.New("replace must not be called for a body with no model")
		},
		wantBody: `{"a":1,"b":"c"}`,
	})

	tests.Add("should pass a non-object body through unchanged", test{
		reader: strings.NewReader(`["model","openai/gpt-4"]`),
		replace: func(string) (string, error) {
			return "", errors.New("replace must not be called for a non-object body")
		},
		wantBody: `["model","openai/gpt-4"]`,
	})

	tests.Add("should pass through unchanged when the model value is not a string", test{
		reader: strings.NewReader(`{"model":123,"messages":[{"role":"user","content":"hi"}]}`),
		replace: func(string) (string, error) {
			return "", errors.New("replace must not be called for a non-string model value")
		},
		wantBody: `{"model":123,"messages":[{"role":"user","content":"hi"}]}`,
	})

	tests.Add("should ignore a nested model decoy and rewrite only the top-level key", test{
		reader: strings.NewReader(`{"messages":[{"role":"user","model":"decoy","content":"hi"}],"model":"openai/gpt-4"}`),
		replace: func(m string) (string, error) {
			return strings.TrimPrefix(m, "openai/"), nil
		},
		wantBody: `{"messages":[{"role":"user","model":"decoy","content":"hi"}],"model":"gpt-4"}`,
	})

	tests.Add("should rewrite only the model key, not a value that equals model", test{
		reader: strings.NewReader(`{"foo":"model","model":"openai/gpt-4"}`),
		replace: func(m string) (string, error) {
			return strings.TrimPrefix(m, "openai/"), nil
		},
		wantBody: `{"foo":"model","model":"gpt-4"}`,
	})

	tests.Add("should propagate a replace-transform error unchanged", test{
		reader: strings.NewReader(`{"model":"openai/gpt-4"}`),
		replace: func(string) (string, error) {
			return "", errors.New("boom")
		},
		wantErr: "boom",
	})

	tests.Add("should return io.EOF for an empty body", test{
		reader:  strings.NewReader(""),
		replace: func(string) (string, error) { return "", nil },
		wantErr: "EOF",
	})

	tests.Add("should propagate an I/O error encountered mid-scan", test{
		reader:  io.MultiReader(strings.NewReader(`{"a":1,`), errReader{errors.New("boom")}),
		replace: func(string) (string, error) { return "", nil },
		wantErr: "boom",
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		r, err := rewriteModel(tt.reader, tt.replace)
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Fatalf("unexpected error: got %v, want /%s/", err, tt.wantErr)
		}
		if err != nil {
			return
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.wantBody {
			t.Errorf("body mismatch:\n got: %s\nwant: %s", got, tt.wantBody)
		}
	})
}

func TestChatCompletionsBoundsStalledStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			reqCtx := req.Context()
			first := true
			body := io.NopCloser(readerFunc(func(p []byte) (int, error) {
				if first {
					first = false
					return copy(p, "data"), nil
				}
				<-reqCtx.Done()
				return 0, reqCtx.Err()
			}))
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
		})
		srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}, WithLogger(testLogger(t)))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200", rr.Code)
		}
		if got := rr.Body.String(); got != "data" {
			t.Errorf("body: got %q, want %q", got, "data")
		}
	})
}

func TestChatCompletionsClosesResponseThatRacesTheStallGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := &trackingBody{Reader: strings.NewReader(`{"id":"late"}`)}
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			// Finish just after the gate fires, so the stall branch has already been
			// taken and the response arrives to a caller that stopped waiting for it.
			time.Sleep(defaultHeaderStallGate + time.Second)
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
		})
		srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}, WithLogger(testLogger(t)))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")

		srv.ServeHTTP(httptest.NewRecorder(), req)
		synctest.Wait()

		if !body.closed {
			t.Error("stalled request leaked the response that raced the gate: Body was not closed")
		}
	})
}

func TestChatCompletionsRecordsErrorOnStalledStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			reqCtx := req.Context()
			first := true
			body := io.NopCloser(readerFunc(func(p []byte) (int, error) {
				if first {
					first = false
					return copy(p, "data"), nil
				}
				<-reqCtx.Done()
				return 0, reqCtx.Err()
			}))
			return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
		})
		srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}, WithLogger(testLogger(t)))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")

		ctx := WithLogCtx(req.Context())
		srv.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))

		if rec, _ := LogRecordFromContext(ctx); rec.Err == nil {
			t.Error("expected a stalled-stream error to be recorded on the log record, got none")
		}
	})
}

func TestModelsAggregatesAcrossBackends(t *testing.T) {
	t.Parallel()

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"llama","created":111,"owned_by":"groq-co"}]}`)
	}))
	t.Cleanup(serverA.Close)
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"k2","created":222,"owned_by":"moonshot"}]}`)
	}))
	t.Cleanup(serverB.Close)

	uA, err := url.Parse(serverA.URL)
	if err != nil {
		t.Fatal(err)
	}
	uB, err := url.Parse(serverB.URL)
	if err != nil {
		t.Fatal(err)
	}

	validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
		return &BackendConfig{Backends: map[string]Backend{
			"groq": {ProviderType: "openai", Config: providers.Config{BaseURL: uA, APIKey: "ka"}},
			"kimi": {ProviderType: "openai", Config: providers.Config{BaseURL: uB, APIKey: "kb"}},
		}}, nil
	}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer user-token")

	rr := httptest.NewRecorder()
	New(validator, WithLogger(testLogger(t))).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d, want %d", rr.Code, http.StatusOK)
	}

	var got struct {
		Data []providers.Model `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	// The list aggregates across backends by ranging a map, whose order is
	// unspecified, so compare as a set.
	want := []providers.Model{
		{ID: "groq/llama", Created: 111, OwnedBy: "groq-co"},
		{ID: "kimi/k2", Created: 222, OwnedBy: "moonshot"},
	}
	if d := gocmp.Diff(want, got.Data, cmpopts.SortSlices(func(a, b providers.Model) bool { return a.ID < b.ID })); d != "" {
		t.Errorf("aggregated models mismatch (-want +got):\n%s", d)
	}
}

func TestWithStatusRecorderPreservesWrittenResponse(t *testing.T) {
	t.Parallel()

	// withStatusRecorder installs the written-response detection errToResponse
	// needs to avoid rendering its error envelope over a response a handler already
	// wrote: a handler writes a body and then returns an error, and the
	// already-written response must survive unchanged.
	handler := withStatusRecorder(errToResponse(func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "partial")
		return errors.New("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusTeapot)
	}
	if got := rec.Body.String(); got != "partial" {
		t.Errorf("body: got %q, want %q — errToResponse rendered over the already-written response", got, "partial")
	}
}

func TestErrToResponseSkipsWriteJSONErrorWhenHandlerWroteHeader(t *testing.T) {
	t.Parallel()

	handler := withStatusRecorder(errToResponse(func(w http.ResponseWriter, _ *http.Request) error {
		_, _ = io.WriteString(w, "partial")
		return errors.New("post-write failure")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := WithLogCtx(req.Context())
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req.WithContext(ctx))

	if rr.Code != http.StatusOK {
		t.Errorf("unexpected status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Body.String(); got != "partial" {
		t.Errorf("unexpected body: got %q, want %q", got, "partial")
	}
	if rec, _ := LogRecordFromContext(ctx); rec.Err == nil || rec.Err.Error() != "post-write failure" {
		t.Errorf("errToResponse did not record the error on the log record: got %v", rec.Err)
	}
}

func TestNewPopulatesLogCtxFromFullStack(t *testing.T) {
	t.Parallel()

	type test struct {
		validator   TokenValidator
		method      string
		path        string
		requestBody string
		want        map[string]any
	}

	tests := testy.NewTable[test]()

	tests.Add("should log error attr for an unroutable bare model", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: nil}, nil
			},
		},
		requestBody: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		want:        map[string]any{"error": `no backend configured to serve model "gpt-4"`},
	})

	tests.AddFunc("should log upstream_status from successful chat-completions", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","choices":[]}`)),
				Header:     http.Header{},
			}, nil
		})
		return test{
			validator: &stubValidator{
				ValidateFn: func(context.Context, string) (*BackendConfig, error) {
					return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
				},
			},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want:        map[string]any{"upstream_status": float64(200)},
		}
	})

	tests.AddFunc("should log model_requested and model_upstream from chat-completions", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","choices":[]}`)),
				Header:     http.Header{},
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"model_requested": "openai/gpt-4",
				"model_upstream":  "gpt-4",
			},
		}
	})

	tests.Add("should log error attr when the model names no configured logical model", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: map[string]Backend{
					"broken": {ProviderType: "openai"},
				}}, nil
			},
		},
		requestBody: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		want:        map[string]any{"error": `unknown logical model "gpt-4"`},
	})

	tests.AddFunc("should log backend_name from chat-completions", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","choices":[]}`)),
				Header:     http.Header{},
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want:        map[string]any{"backend_name": "openai"},
		}
	})

	tests.AddFunc("should record the upstream status of an attempt that failed", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"upstream busy"}}`)),
				Header:     http.Header{},
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want:        map[string]any{"upstream_status": float64(http.StatusServiceUnavailable)},
		}
	})

	tests.AddFunc("should record the abandoned target when a request fails over", func(t *testing.T) test {
		rateLimited := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
				Header:     http.Header{"Retry-After": {"60"}},
			}, nil
		})
		serves := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","choices":[]}`)),
				Header:     http.Header{},
			}, nil
		})
		backend := func(rawURL, key string, client *http.Client) Backend {
			u, err := url.Parse(rawURL)
			if err != nil {
				t.Fatal(err)
			}
			return Backend{ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: key, HTTPClient: client}}
		}
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{
					Backends: map[string]Backend{
						"a": backend("http://a/v1", "sk-a", rateLimited),
						"b": backend("http://b/v1", "sk-b", serves),
					},
					Models: map[string]LogicalModel{"m": {Targets: []Target{
						{backend: "a", model: "ma"},
						{backend: "b", model: "mb"},
					}}},
				}, nil
			}},
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"backend_name": "b",
				"excluded":     []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
		}
	})

	tests.AddFunc("should report the backend it skipped, and why, on the request's log record", func(*testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-good"}`)), Header: http.Header{}}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{
					Backends: map[string]Backend{
						"broken": {ProviderType: "openai", Config: providers.Config{APIKey: "sk-bad"}},
						"good":   {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "good", Path: "/v1"}, APIKey: "sk-good", HTTPClient: client}},
					},
					Models: map[string]LogicalModel{"m": {Targets: []Target{
						{backend: "broken", model: "gpt-4"},
						{backend: "good", model: "gpt-4"},
					}}},
				}, nil
			}},
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"backend_name": "good",
				"excluded":     []Attempt{{Backend: "broken", ModelUpstream: "gpt-4", Err: errors.New("must provide base_url")}},
			},
		}
	})

	tests.AddFunc("should report a target's undeclared backend as no such backend rather than an empty provider type", func(*testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-good"}`)), Header: http.Header{}}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{
					Backends: map[string]Backend{
						"good": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "good", Path: "/v1"}, APIKey: "sk-good", HTTPClient: client}},
					},
					Models: map[string]LogicalModel{"m": {Targets: []Target{
						{backend: "ghost", model: "gpt-4"},
						{backend: "good", model: "gpt-4"},
					}}},
				}, nil
			}},
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"backend_name": "good",
				"excluded":     []Attempt{{Backend: "ghost", ModelUpstream: "gpt-4", Err: errors.New("no such backend")}},
			},
		}
	})

	tests.AddFunc("should record an abandoned target and an excluded one in the order it walked them", func(*testing.T) test {
		limited := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
				Header:     http.Header{"Retry-After": {"60"}},
			}, nil
		})
		serving := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-good"}`)), Header: http.Header{}}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{
					Backends: map[string]Backend{
						"limited": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "limited", Path: "/v1"}, APIKey: "sk-l", HTTPClient: limited}},
						"broken":  {ProviderType: "openai", Config: providers.Config{APIKey: "sk-b"}},
						"good":    {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "good", Path: "/v1"}, APIKey: "sk-g", HTTPClient: serving}},
					},
					Models: map[string]LogicalModel{"m": {Targets: []Target{
						{backend: "limited", model: "ml"},
						{backend: "broken", model: "mb"},
						{backend: "good", model: "mg"},
					}}},
				}, nil
			}},
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"backend_name": "good",
				"excluded": []Attempt{
					{Backend: "limited", ModelUpstream: "ml", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true},
					{Backend: "broken", ModelUpstream: "mb", Err: errors.New("must provide base_url")},
				},
			},
		}
	})

	tests.AddFunc("should report the backend a listing left out, and why", func(*testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"gpt-4","created":1,"owned_by":"openai"}]}`)),
				Header:     http.Header{"Content-Type": {"application/json"}},
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: map[string]Backend{
					"good":   {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "good", Path: "/v1"}, APIKey: "sk-good", HTTPClient: client}},
					"broken": {ProviderType: "openai", Config: providers.Config{APIKey: "sk-bad"}},
				}}, nil
			}},
			method: http.MethodGet,
			path:   "/v1/models",
			want: map[string]any{
				"excluded": []Attempt{{Backend: "broken", Err: errors.New("must provide base_url")}},
			},
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		srv := New(tt.validator, WithLogger(testLogger(t)))

		req, err := http.NewRequestWithContext(t.Context(), cmp.Or(tt.method, http.MethodPost), cmp.Or(tt.path, "/v1/chat/completions"), strings.NewReader(tt.requestBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		ctx := WithLogCtx(req.Context())
		srv.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))

		rec, _ := LogRecordFromContext(ctx)
		got := map[string]any{
			"error":           logRecordErrString(rec.Err),
			"backend_name":    rec.BackendName,
			"model_requested": rec.ModelRequested,
			"model_upstream":  rec.ModelUpstream,
			"upstream_status": float64(rec.UpstreamStatus),
			"excluded":        rec.Excluded,
		}
		for k := range got {
			if _, present := tt.want[k]; !present {
				delete(got, k)
			}
		}
		if d := gocmp.Diff(tt.want, got, errorText); d != "" {
			t.Errorf("LogRecord mismatch (-want +got):\n%s", d)
		}
	})
}

// logRecordErrString renders a LogRecord error for comparison against a want map,
// yielding "" for a nil error so a case that expects no error omits the key.
func logRecordErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errorText compares errors by their message, so a want built with errors.New
// matches the wrapped error the proxy actually recorded.
var errorText = gocmp.Comparer(func(a, b error) bool {
	return logRecordErrString(a) == logRecordErrString(b)
})

// errorEnvelope decodes the fields of understudy's error response a case may
// assert on. Compared with assertedFields, a field left zero is one the case
// does not assert, so each case pins only the aspect its behavior names.
type errorEnvelope struct {
	Error        errorDetail `json:"error"`
	RetryAfterMS int         `json:"retry_after_ms"`
}

type errorDetail struct {
	Type string `json:"type"`
}

// assertedFields ignores every field the want value leaves zero, so one
// cmp.Diff over the whole struct still asserts only what a case set.
var assertedFields = gocmp.FilterPath(func(p gocmp.Path) bool {
	want, _ := p.Last().Values()
	return want.IsValid() && want.IsZero()
}, gocmp.Ignore())

// TODO(TODO.d/direct-target-reference-drops-query-overrides.md): every case here
// drives a logical model, but a directly-named backend/model reference now reaches
// the same health map — its failures can demote an account other requests share.
func TestChatCompletionsFailoverRouting(t *testing.T) {
	t.Parallel()

	const badGateway502 = `{"error":{"message":"Bad Gateway","type":"server_error"}}`
	const rateLimit429 = `{"error":{"message":"upstream returned status 429: slow down","type":"rate_limit_error"}}`

	resp := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}
	}
	always := func(status int, body string) func(*http.Request, int) (*http.Response, error) {
		return func(*http.Request, int) (*http.Response, error) { return resp(status, body), nil }
	}
	// stall never returns a response header, blocking until its request context is
	// cancelled — a pre-header stall. The long fallback keeps a broken gate surfacing
	// as an assertion failure rather than a hang.
	stall := func(r *http.Request, _ int) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(5 * time.Minute):
			return resp(http.StatusOK, `{"id":"unstalled"}`), nil
		}
	}

	type step struct {
		// model is what the request names; empty means the logical model "m".
		model       string
		advance     time.Duration
		wantStatus  int
		wantBody    string
		wantBackend string
		// wantExcluded is what the request did not serve from, asserted only when
		// non-nil so the cases that are not about the log record stay silent on it.
		wantExcluded []Attempt
		// wantEnvelope is the error response's asserted aspects; a field left zero
		// is one this case's behavior does not name.
		wantEnvelope errorEnvelope
	}
	// backendStub is one backend's real upstream identity — base URL and API key —
	// plus its stubbed round-trip: given the request and the 1-based call count, it
	// returns the upstream response, or blocks (a stall) until the request context
	// is cancelled.
	type backendStub struct {
		baseURL string
		apiKey  string
		resp    func(r *http.Request, call int) (*http.Response, error)
	}
	type test struct {
		backends map[string]backendStub
		targets  []Target
		steps    []step
	}

	tests := testy.NewTable[test]()

	tests.Add("should fail over to the next target after the threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`},
		},
	})

	tests.Add("should route a logical model around an account a directly-named reference demoted", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantBackend: "a"},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should reject a directly-named reference on a streak a logical model accrued past the terminal threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{
				model:        "a/ma",
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamUnavailable}},
			},
		},
	})

	// TODO(TODO.d/cover-a-reference-timed-and-immediate-demotions.md): the cases
	// above drive only the streak. A directly-named reference also benches the
	// shared account on a 429 carrying a Retry-After, and demotes it outright on a
	// refused access; neither is driven that way here.

	tests.Add("should demote a target on a 429 with no Retry-After", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantBackend: "a"},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should fail over within the request when a target stalls before its response header", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: stall},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should name the stalled target an operator would otherwise not see", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: stall},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBackend: "b", wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", Err: errHeaderStall, Called: true}}},
		},
	})

	tests.Add("should show the client 502 for an upstream 5xx", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
		},
	})

	tests.Add("should carry the upstream's own words for why a target was walked past", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"quota gemini-2.5-flash exhausted"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBackend: "b", wantExcluded: []Attempt{{
				Backend:        "a",
				ModelUpstream:  "ma",
				UpstreamStatus: http.StatusTooManyRequests,
				Err:            errors.New("upstream returned status 429: quota gemini-2.5-flash exhausted"),
				Called:         true,
			}}},
		},
	})

	tests.Add("should show the status the walked-past target answered with", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBackend: "b", wantExcluded: []Attempt{
				{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true},
			}},
		},
	})

	tests.Add("should surface a 504 when every target stalls and the replay walk is exhausted", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: stall},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: stall},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusGatewayTimeout, wantBody: `{"error":{"message":"Gateway Timeout","type":"server_error"}}`, wantBackend: "b"},
		},
	})

	tests.Add("should bench a sibling backend sharing an account and model when one is demoted", test{
		backends: map[string]backendStub{
			"acct-a": {baseURL: "http://shared/v1", apiKey: "sk-shared", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"acct-b": {baseURL: "http://shared/v1", apiKey: "sk-shared", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"acct-c": {baseURL: "http://other/v1", apiKey: "sk-other", resp: always(http.StatusOK, `{"id":"from-c"}`)},
		},
		targets: []Target{
			{backend: "acct-a", model: "glm"},
			{backend: "acct-b", model: "glm"},
			{backend: "acct-c", model: "glm"},
		},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantBackend: "acct-a"},
			// acct-b is the same account+model as the demoted acct-a, so it is
			// benched too: failover routes to the different account acct-c.
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-c"}`, wantBackend: "acct-c"},
		},
	})

	tests.Add("should not demote a target on a 429 whose Retry-After is within the threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"10"}},
				}, nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantBackend: "a"},
			{advance: time.Second, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantBackend: "a"},
		},
	})

	tests.Add("should not fail over on a non-fatal error", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusBadRequest, `{"error":{"message":"bad request"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadRequest, wantBody: `{"error":{"message":"upstream returned status 400: bad request","type":"invalid_request_error"}}`, wantBackend: "a"},
			{advance: time.Second, wantStatus: http.StatusBadRequest, wantBody: `{"error":{"message":"upstream returned status 400: bad request","type":"invalid_request_error"}}`, wantBackend: "a"},
		},
	})

	tests.Add("should route to the last target when all are failing", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBackend: "a"},
			{advance: 16 * time.Second, wantStatus: http.StatusBadGateway, wantBackend: "b"},
			{advance: 16 * time.Second, wantStatus: http.StatusBadGateway, wantBackend: "b"},
		},
	})

	tests.Add("should reject as non-retryable once a target has been failing past the terminal threshold with nowhere to fail over", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamUnavailable}},
			},
		},
	})

	tests.Add("should start a fresh streak once a demotion has gone untouched past the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{
				advance:      24*time.Hour + time.Second,
				wantStatus:   http.StatusBadGateway,
				wantBody:     badGateway502,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeServer}},
			},
		},
	})

	tests.Add("should hold a demotion open while a directly-named reference keeps failing across the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{
				model:        "a/ma",
				advance:      23 * time.Hour,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamUnavailable}},
			},
			{
				model:        "a/ma",
				advance:      2 * time.Hour,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamUnavailable}},
			},
		},
	})

	// TODO(TODO.d/sweep-health-outside-the-target-walk.md): these cases pin what the
	// window reclaims; nothing pins what it cannot until the map has a size cap and
	// a size a consumer can read.
	//
	// TODO(TODO.d/understudy-error-envelope-type.md): the refusal path takes the same
	// sweep and re-stamp, and still no case drives it across the window, because a
	// refusal's answer does not vary with age: writeRefusal renders before any streak
	// is consulted, so 0h, 23h and 48h all give 400 upstream_refused. Whether that
	// should change is the open question — only an operator clears a refusal, so a
	// streak may have nothing to say about one.

	tests.Add("should start a fresh streak once a directly-named reference's demotion has gone untouched past the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{
				model:        "a/ma",
				advance:      24*time.Hour + time.Second,
				wantStatus:   http.StatusBadGateway,
				wantBody:     badGateway502,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeServer}},
			},
		},
	})

	tests.Add("should start a fresh streak once a rate-limited reference's demotion has gone untouched past the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429},
			{
				model:      "a/ma",
				advance:    24*time.Hour + time.Second,
				wantStatus: http.StatusTooManyRequests,
				wantBody:   rateLimit429,
			},
		},
	})

	// TODO(TODO.d/pin-a-never-retryable-that-advertised-a-delay.md): this reaches the
	// never-retryable clear through shouldReject only. A 501 that also named a delay
	// takes the other branch, and no case sends one. Those belong beside this.

	tests.Add("should not invent a delay for a target no retry can help, however long it has failed", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusNotImplemented, `{"error":{"message":"not implemented"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadGateway,
				wantBody:     badGateway502,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeServer}},
			},
		},
	})

	tests.Add("should advertise the capped backoff when the terminal reject has no upstream Retry-After to relay", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{RetryAfterMS: 120000},
			},
		},
	})

	tests.Add("should keep relaying the upstream failure when a long-dead target is probed but an alternate remains untried", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantBackend: "a"},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadGateway,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeServer}},
				wantBackend:  "a",
			},
		},
	})

	tests.Add("should advertise the upstream's own backoff when the reject is terminal", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"600"}},
				}, nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadRequest, wantEnvelope: errorEnvelope{RetryAfterMS: 600000}},
			{
				advance:      10*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{RetryAfterMS: 600000},
			},
		},
	})

	tests.Add("should restore a target after it recovers", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{advance: 5 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`},
		},
	})

	tests.Add("should re-probe and restore a demoted target after the recovery interval", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`},
			{advance: 30 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`},
		},
	})

	tests.Add("should preserve the streak start across repeated failures", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{advance: 5 * time.Second, wantStatus: http.StatusBadGateway, wantBody: badGateway502},
			{advance: 11 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`},
		},
	})

	tests.Add("should fail over within the request when a target returns a sustainedRate 429", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should fail over within the request when a target's credential is out of funds", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusPaymentRequired, `{"error":{"message":"Insufficient Balance"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should fail over within the request when a target's credential is rejected", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should tell a client a refused request is terminal without repeating what the upstream said", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{
				model:      "a/ma",
				wantStatus: http.StatusBadRequest,
				wantBody:   `{"error":{"message":"no configured target could serve this request","type":"upstream_refused"}}`,
			},
		},
	})

	// TODO(TODO.d/understudy-error-envelope-type.md): the case above pins the half
	// of the disclosure rule that hides. Nothing pins the half that tells: a case
	// belongs here reading LogRecordFromContext after a refusal and asserting Err
	// carries the upstream's own words, so the operator keeps what the client loses.
	//
	// TODO(TODO.d/understudy-error-envelope-type.md): and nothing yet drives a mixed
	// walk. `[a: 429 for 60s, b: 401]` answers upstream_refused on b, telling a
	// client to escalate while a is merely throttled and will serve. Write it when
	// the verdict reads the attempts on Excluded rather than the last error.

	tests.Add("should fail over within the request when a target forbids the request", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: always(http.StatusForbidden, `{"error":{"message":"only available hosted in China and requires explicit opt in"}}`)},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	// TODO(TODO.d/specify-what-a-refusal-promises.md): the case above pins only
	// that this request gets an answer. A second case belongs here for what the
	// demotion buys the next one — a request a second later serving from "b" with
	// "a" never called, its Excluded empty because the walk skips it.

	tests.Add("should fail over across requests when a recurring 429's Retry-After is below the demotion threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"10"}},
				}, nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	// The fallback echoes the body it received, so wantBody asserts the full
	// request payload (with the model rewritten for that target) survived the replay.
	tests.Add("should replay the full request body to the fallback target", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: func(r *http.Request, _ int) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"model":"mb","messages":[{"role":"user","content":"hi"}]}`, wantBackend: "b"},
		},
	})

	// TODO(TODO.d/specify-what-a-refusal-promises.md): the refusal's counterpart to
	// the case below belongs here. A whole list refusing answers the same 400
	// upstream_refused a single target does, so the status cannot tell them apart —
	// the walk shows only on Excluded, with every candidate called and refused.

	tests.Add("should surface the 429 when every target is rate-limited past the threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: "http://a/v1", apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: "http://b/v1", apiKey: "sk-b", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429},
		},
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			var lastDialed string
			backends := map[string]Backend{}
			for name, bs := range tt.backends {
				calls := 0
				client := testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
					calls++
					lastDialed = name
					return bs.resp(r, calls)
				})
				u, err := url.Parse(bs.baseURL)
				if err != nil {
					t.Fatalf("backend %q base URL %q: %v", name, bs.baseURL, err)
				}
				backends[name] = Backend{ProviderType: "openai", Config: providers.Config{BaseURL: u, APIKey: bs.apiKey, HTTPClient: client}}
			}
			validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: tt.targets}}}, nil
			}}
			srv := New(validator, WithLogger(testLogger(t)))

			for i, s := range tt.steps {
				if s.advance > 0 {
					time.Sleep(s.advance)
					synctest.Wait()
				}
				body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, cmp.Or(s.model, "m"))
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer user-token")
				req.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				ctx := WithLogCtx(req.Context())
				srv.ServeHTTP(rr, req.WithContext(ctx))
				if rr.Code != s.wantStatus {
					t.Errorf("step %d: status got %d want %d", i, rr.Code, s.wantStatus)
				}
				if s.wantExcluded != nil {
					rec, _ := LogRecordFromContext(ctx)
					if d := gocmp.Diff(s.wantExcluded, rec.Excluded, errorText); d != "" {
						t.Errorf("step %d abandoned attempts (-want +got):\n%s", i, d)
					}
				}
				if s.wantBody != "" {
					if d := testy.DiffJSON([]byte(s.wantBody), rr.Body.Bytes()); d != nil {
						t.Errorf("step %d body: %s", i, d)
					}
				}
				// A body that is not an error envelope leaves the fields zero, which
				// only the cases asserting on them can fail on.
				var envelope errorEnvelope
				_ = json.Unmarshal(rr.Body.Bytes(), &envelope)
				if d := gocmp.Diff(s.wantEnvelope, envelope, assertedFields); d != "" {
					t.Errorf("step %d envelope (-want +got):\n%s", i, d)
				}
				if s.wantBackend != "" && lastDialed != s.wantBackend {
					t.Errorf("step %d dialed %q want %q", i, lastDialed, s.wantBackend)
				}
			}
		})
	})
}

func TestChatCompletionsTransitionLogging(t *testing.T) {
	t.Parallel()

	always502 := func(int) int { return http.StatusBadGateway }
	recoverOnProbe := func(call int) int {
		if call == 2 {
			return http.StatusOK
		}
		return http.StatusBadGateway
	}
	recoverWithinGrace := func(call int) int {
		if call >= 2 {
			return http.StatusOK
		}
		return http.StatusBadGateway
	}

	type test struct {
		aStatus    func(call int) int
		retryAfter time.Duration
		advances   []time.Duration
		wantDown   int
		wantUp     int
	}

	tests := testy.NewTable[test]()

	tests.Add("should log a target down once when it crosses the failover threshold", test{
		aStatus:  always502,
		advances: []time.Duration{16 * time.Second, time.Second},
		wantDown: 1,
		wantUp:   0,
	})
	tests.Add("should re-log a target down after it recovers and fails again", test{
		aStatus:  recoverOnProbe,
		advances: []time.Duration{16 * time.Second, 30 * time.Second, time.Second, 16 * time.Second},
		wantDown: 2,
		wantUp:   1,
	})
	tests.Add("should log a target up when a demoted target recovers", test{
		aStatus:  recoverOnProbe,
		advances: []time.Duration{16 * time.Second, 30 * time.Second},
		wantDown: 1,
		wantUp:   1,
	})
	tests.Add("should not log a transition when a target recovers within the failover threshold", test{
		aStatus:  recoverWithinGrace,
		advances: []time.Duration{5 * time.Second},
		wantDown: 0,
		wantUp:   0,
	})
	tests.Add("should log a target up when a rate-limited target is re-admitted after its retry-after", test{
		aStatus: func(call int) int {
			if call == 1 {
				return http.StatusTooManyRequests
			}
			return http.StatusOK
		},
		retryAfter: 50 * time.Second,
		advances:   []time.Duration{10 * time.Second, 50 * time.Second},
		wantDown:   1,
		wantUp:     1,
	})
	tests.Add("should keep a rate-limited target benched when its re-admission probe fails", test{
		aStatus: func(call int) int {
			if call == 1 {
				return http.StatusTooManyRequests
			}
			return http.StatusBadGateway
		},
		retryAfter: 50 * time.Second,
		advances:   []time.Duration{10 * time.Second, 50 * time.Second},
		wantDown:   1,
		wantUp:     0,
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

			callsA := 0
			clientA := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
				callsA++
				status := tt.aStatus(callsA)
				body := `{"error":{"message":"bad gateway"}}`
				if status == http.StatusOK {
					body = `{"id":"from-a"}`
				}
				header := make(http.Header)
				if status == http.StatusTooManyRequests && tt.retryAfter > 0 {
					header.Set("Retry-After", strconv.Itoa(int(tt.retryAfter/time.Second)))
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header}, nil
			})
			clientB := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-b"}`)), Header: make(http.Header)}, nil
			})
			backends := map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: clientA}},
				"b": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "b", Path: "/v1"}, APIKey: "sk-b", HTTPClient: clientB}},
			}
			validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}}}}, nil
			}}
			srv := New(validator, WithLogger(logger))

			doRequest := func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer user-token")
				req.Header.Set("Content-Type", "application/json")
				srv.ServeHTTP(httptest.NewRecorder(), req)
			}

			doRequest()
			for _, adv := range tt.advances {
				time.Sleep(adv)
				synctest.Wait()
				doRequest()
			}

			down := map[string]any{"msg": "backend down", "level": "INFO", "backend": "a", "model": "ma"}
			up := map[string]any{"msg": "backend up", "level": "INFO", "backend": "a", "model": "ma"}
			if got := slogdiff.JSONCount(logBuf.Bytes(), down); got != tt.wantDown {
				t.Errorf("%q count for backend=a model=ma: got %d, want %d; log:\n%s", "backend down", got, tt.wantDown, logBuf.String())
			}
			if got := slogdiff.JSONCount(logBuf.Bytes(), up); got != tt.wantUp {
				t.Errorf("%q count for backend=a model=ma: got %d, want %d; log:\n%s", "backend up", got, tt.wantUp, logBuf.String())
			}
		})
	})
}

func TestChatCompletionsLimiterCancelNamesTheWaitPoint(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

		// A single-slot upstream limiter. The holder takes the slot and blocks on
		// its context, so a second request queues at acquire; when its context is
		// canceled it must report that it was waiting for a slot — not surface a
		// bare "context canceled" that hides where it stopped.
		backend := testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		backends := map[string]Backend{
			"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: backend}},
		}
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}}}}}, nil
		}}
		srv := New(validator, WithLogger(logger)).(*server)
		srv.maxConcurrentPerUpstream = 1

		req := func(ctx context.Context) *http.Request {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer user-token")
			r.Header.Set("Content-Type", "application/json")
			return r
		}

		go func() { srv.ServeHTTP(httptest.NewRecorder(), req(t.Context())) }()
		synctest.Wait()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req(ctx))

		if !strings.Contains(rec.Body.String(), "upstream slot") {
			t.Errorf("limiter-canceled body = %q; want it to name waiting for an upstream slot", rec.Body.String())
		}
	})
}

func TestChatCompletionsShedsWhenProcessBudgetExhausted(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

		// The upstream blocks so the holder keeps its process slot; with a budget of
		// one, the next request must be shed rather than served.
		backend := testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		backends := map[string]Backend{
			"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: backend}},
		}
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}}}}}, nil
		}}
		// fdSlotBudget(66) = (66-64)/2 = 1: a single process-wide slot.
		srv := New(validator, WithLogger(logger), withFDSoftLimit(66)).(*server)

		req := func(ctx context.Context) *http.Request {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer user-token")
			r.Header.Set("Content-Type", "application/json")
			return r
		}

		// A holder takes the single process slot and blocks in the upstream.
		go func() { srv.ServeHTTP(httptest.NewRecorder(), req(t.Context())) }()
		synctest.Wait()

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req(t.Context()))

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		if got := rec.Header().Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want %q", got, "5")
		}
	})
}

func TestChatCompletionsRetryAfterDelaysReadmission(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

		callsA := 0
		clientA := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			callsA++
			if callsA == 1 {
				h := make(http.Header)
				h.Set("Retry-After", "50")
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)), Header: h}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-a"}`)), Header: make(http.Header)}, nil
		})
		clientB := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-b"}`)), Header: make(http.Header)}, nil
		})
		backends := map[string]Backend{
			"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: clientA}},
			"b": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "b", Path: "/v1"}, APIKey: "sk-b", HTTPClient: clientB}},
		}
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}}}}, nil
		}}
		srv := New(validator, WithLogger(logger))

		doRequest := func() *httptest.ResponseRecorder {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}

		// Request 1 hits a, gets a 429 with Retry-After:50, demoting a for 50s.
		doRequest()

		// Advance past the fixed 30s recovery interval but before the 50s retry-after.
		time.Sleep(40 * time.Second)
		synctest.Wait()

		// a is still benched for its retry-after, so b must serve this request.
		rec := doRequest()
		if d := testy.DiffJSON([]byte(`{"id":"from-b"}`), rec.Body.Bytes()); d != nil {
			t.Errorf("request routed to the wrong backend: %s", d)
		}
	})
}

func TestChatCompletionsRetryAfterOverridesUnboundedDemotion(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

		callsA := 0
		clientA := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			callsA++
			switch callsA {
			case 1:
				// Signal-less 429: demotes a with readmitAt zero.
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)), Header: make(http.Header)}, nil
			case 2:
				// Retry-after 429 on the half-open probe: must bench a until t+50s.
				h := make(http.Header)
				h.Set("Retry-After", "50")
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)), Header: h}, nil
			default:
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-a"}`)), Header: make(http.Header)}, nil
			}
		})
		clientB := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-b"}`)), Header: make(http.Header)}, nil
		})
		backends := map[string]Backend{
			"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: clientA}},
			"b": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "b", Path: "/v1"}, APIKey: "sk-b", HTTPClient: clientB}},
		}
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}}}}, nil
		}}
		srv := New(validator, WithLogger(logger))

		doRequest := func() *httptest.ResponseRecorder {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}

		// t=0: request 1 hits a, gets a signal-less 429, demoting a with readmitAt zero.
		doRequest()

		// t=30: advance past the fixed 30s recovery interval so a is half-open-probed.
		// The probe (call 2) returns a 429 with Retry-After:50, benching a until t=80.
		time.Sleep(30 * time.Second)
		synctest.Wait()
		doRequest()

		// t=70: past the buggy 60s re-probe but before the correct 80s readmit.
		time.Sleep(40 * time.Second)
		synctest.Wait()

		// a is still benched for its retry-after, so b must serve this request.
		rec := doRequest()
		if d := testy.DiffJSON([]byte(`{"id":"from-b"}`), rec.Body.Bytes()); d != nil {
			t.Errorf("request routed to the wrong backend: %s", d)
		}
	})
}

func TestWriteJSONError(t *testing.T) {
	t.Parallel()

	type test struct {
		err        error
		errType    string
		wantStatus int
		wantBody   string
	}

	tests := testy.NewTable[test]()

	tests.Add("should obfuscate a 5xx body to the status text", test{
		err:        yerrors.WithHTTPStatus(http.StatusInternalServerError, errors.New("db table missing")),
		errType:    errTypeServer,
		wantStatus: http.StatusInternalServerError,
		wantBody:   `{"error":{"message":"Internal Server Error","type":"server_error"}}`,
	})

	tests.Add("should obfuscate a 401 body to the status text", test{
		err:        yerrors.WithHTTPStatus(http.StatusUnauthorized, errors.New("token signature invalid")),
		errType:    errTypeAuth,
		wantStatus: http.StatusUnauthorized,
		wantBody:   `{"error":{"message":"Unauthorized","type":"authentication_error"}}`,
	})

	tests.Add("should obfuscate a 403 body to the status text", test{
		err:        yerrors.WithHTTPStatus(http.StatusForbidden, errors.New("user not found")),
		errType:    errTypeAuth,
		wantStatus: http.StatusForbidden,
		wantBody:   `{"error":{"message":"Forbidden","type":"authentication_error"}}`,
	})

	tests.Add("should preserve a non-auth 4xx body", test{
		err:        yerrors.WithHTTPStatus(http.StatusBadRequest, errors.New("bad request: missing model")),
		errType:    errTypeServer,
		wantStatus: http.StatusBadRequest,
		wantBody:   `{"error":{"message":"bad request: missing model","type":"server_error"}}`,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		rec := httptest.NewRecorder()
		writeJSONError(t.Context(), rec, tt.err, tt.errType)

		if rec.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rec.Code, tt.wantStatus)
		}
		if d := testy.DiffJSON([]byte(tt.wantBody), rec.Body.Bytes()); d != nil {
			t.Errorf("unexpected body: %s", d)
		}
	})
}

func TestChatCompletionsConcurrencyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var inFlight, maxInFlight, entered int
		release := make(chan struct{})

		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			inFlight++
			entered++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			<-release

			mu.Lock()
			inFlight--
			mu.Unlock()

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
				Header:     make(http.Header),
			}, nil
		})

		srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}})
		srv.logger = testLogger(t)
		srv.maxConcurrentPerUpstream = 2

		for range 3 {
			go func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
					strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer user-token")
				req.Header.Set("Content-Type", "application/json")
				srv.ServeHTTP(httptest.NewRecorder(), req)
			}()
		}

		synctest.Wait()

		mu.Lock()
		if entered != 2 || maxInFlight != 2 {
			t.Errorf("with cap 2 and 3 requests: entered=%d maxInFlight=%d, want entered=2 maxInFlight=2", entered, maxInFlight)
		}
		mu.Unlock()

		release <- struct{}{}
		synctest.Wait()

		mu.Lock()
		if entered != 3 {
			t.Errorf("after freeing a slot: entered=%d, want 3", entered)
		}
		if maxInFlight > 2 {
			t.Errorf("concurrency exceeded cap: maxInFlight=%d, want <=2", maxInFlight)
		}
		mu.Unlock()

		close(release)
		synctest.Wait()
	})
}

func TestChatCompletionsSeedsCapToInFlightOnFirstSignallessRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var calls, inFlight, maxInFlight int
		release := make(chan struct{})

		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()

			if first {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{},
				}, nil
			}

			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			<-release

			mu.Lock()
			inFlight--
			mu.Unlock()

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
				Header:     make(http.Header),
			}, nil
		})

		srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}})
		srv.logger = testLogger(t)
		srv.maxConcurrentPerUpstream = 4

		post := func() *httptest.ResponseRecorder {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				return nil
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}

		if rec := post(); rec.Code != http.StatusTooManyRequests {
			t.Errorf("tripping request: Code=%d, want %d", rec.Code, http.StatusTooManyRequests)
		}

		for range 4 {
			go post()
		}

		synctest.Wait()

		mu.Lock()
		if maxInFlight != 1 {
			t.Errorf("after seeding on the first signal-less 429: maxInFlight=%d, want 1", maxInFlight)
		}
		mu.Unlock()

		close(release)
		synctest.Wait()
	})
}

func TestChatCompletionsConcurrencyCapPreservesOnSignaledRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var calls, inFlight, maxInFlight int
		release := make(chan struct{})

		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()

			if first {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"40"}},
				}, nil
			}

			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			<-release

			mu.Lock()
			inFlight--
			mu.Unlock()

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
				Header:     make(http.Header),
			}, nil
		})

		srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}})
		srv.logger = testLogger(t)
		srv.maxConcurrentPerUpstream = 4

		post := func() *httptest.ResponseRecorder {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				return nil
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}

		if rec := post(); rec.Code != http.StatusTooManyRequests {
			t.Errorf("tripping request: Code=%d, want %d", rec.Code, http.StatusTooManyRequests)
		}

		for range 4 {
			go post()
		}

		synctest.Wait()

		mu.Lock()
		if maxInFlight != 4 {
			t.Errorf("after signaled rate-limit 429: maxInFlight=%d, want 4", maxInFlight)
		}
		mu.Unlock()

		close(release)
		synctest.Wait()
	})
}

func TestChatCompletionsConcurrencyCapGrowsOnContendedSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var calls, inFlight, maxInFlight int
		// gate releases exactly one blocked upstream call per send.
		gate := make(chan struct{})

		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			mu.Lock()
			calls++
			call := calls
			mu.Unlock()

			if call == 1 {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{},
				}, nil
			}

			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			<-gate

			mu.Lock()
			inFlight--
			mu.Unlock()

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
				Header:     make(http.Header),
			}, nil
		})

		srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}})
		srv.logger = testLogger(t)
		srv.maxConcurrentPerUpstream = 4

		post := func() *httptest.ResponseRecorder {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				return nil
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}

		if rec := post(); rec.Code != http.StatusTooManyRequests {
			t.Errorf("tripping request: Code=%d, want %d", rec.Code, http.StatusTooManyRequests)
		}

		go post()
		synctest.Wait()

		// This one finds the single seeded slot taken and waits for it.
		go post()
		synctest.Wait()

		gate <- struct{}{}
		synctest.Wait()

		gate <- struct{}{}
		synctest.Wait()

		for range 3 {
			go post()
		}

		synctest.Wait()

		mu.Lock()
		if maxInFlight != 2 {
			t.Errorf("after seed then grow on a contended success: maxInFlight=%d, want 2", maxInFlight)
		}
		mu.Unlock()

		close(gate)
		synctest.Wait()
	})
}

func TestChatCompletionsConcurrencyLimitCancelledWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})

		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			entered <- struct{}{}
			<-release
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
				Header:     make(http.Header),
			}, nil
		})

		srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}})
		srv.logger = testLogger(t)
		srv.maxConcurrentPerUpstream = 1

		go func() {
			reqA, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				return
			}
			reqA.Header.Set("Authorization", "Bearer user-token")
			reqA.Header.Set("Content-Type", "application/json")
			srv.ServeHTTP(httptest.NewRecorder(), reqA)
		}()

		synctest.Wait()
		<-entered

		ctxB, cancelB := context.WithCancel(t.Context())
		reqB, err := http.NewRequestWithContext(ctxB, http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		reqB.Header.Set("Authorization", "Bearer user-token")
		reqB.Header.Set("Content-Type", "application/json")
		recB := httptest.NewRecorder()

		go func() {
			srv.ServeHTTP(recB, reqB)
		}()

		synctest.Wait()

		cancelB()
		synctest.Wait()

		if recB.Code != statusClientClosedRequest {
			t.Errorf("cancelled wait: recB.Code=%d, want %d", recB.Code, statusClientClosedRequest)
		}

		close(release)
		synctest.Wait()
	})
}

func TestChatCompletionsSignallessRateLimitStaysUnderConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})

		var callsA int
		clientA := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			callsA++
			switch callsA {
			case 1:
				<-release
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-a"}`)), Header: make(http.Header)}, nil
			case 2:
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)), Header: http.Header{}}, nil
			default:
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-a"}`)), Header: make(http.Header)}, nil
			}
		})
		clientB := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-b"}`)), Header: make(http.Header)}, nil
		})
		backends := map[string]Backend{
			"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: clientA}},
			"b": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "b", Path: "/v1"}, APIKey: "sk-b", HTTPClient: clientB}},
		}
		srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}}}}, nil
		}})
		srv.logger = testLogger(t)

		post := func(rec *httptest.ResponseRecorder) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			srv.ServeHTTP(rec, req)
		}

		go post(httptest.NewRecorder())
		synctest.Wait()

		recR2 := httptest.NewRecorder()
		post(recR2)

		time.Sleep(time.Second)
		synctest.Wait()

		recR3 := httptest.NewRecorder()
		post(recR3)

		if recR3.Code != http.StatusOK {
			t.Errorf("R3 status: got %d, want %d", recR3.Code, http.StatusOK)
		}
		if d := testy.DiffJSON([]byte(`{"id":"from-a"}`), recR3.Body.Bytes()); d != nil {
			t.Errorf("R3 body: %s", d)
		}

		close(release)
		synctest.Wait()
	})
}

func TestChatCompletionsConcurrencyLimitPerUpstreamIdentity(t *testing.T) {
	type test struct {
		urlA, keyA   string
		urlB, keyB   string
		wantA, wantB int
	}

	tests := testy.NewTable[test]()

	tests.Add("should isolate distinct upstreams so one's full cap does not block another", test{
		urlA: "http://a/v1", keyA: "sk-a",
		urlB: "http://b/v1", keyB: "sk-b",
		wantA: 1, wantB: 1,
	})

	tests.Add("should share one cap across base URLs differing only by a trailing slash", test{
		urlA: "http://a/v1", keyA: "sk-a",
		urlB: "http://a/v1/", keyB: "sk-a",
		wantA: 1, wantB: 0,
	})

	tests.Add("should isolate upstreams that share a base URL but differ by API key", test{
		urlA: "http://a/v1", keyA: "sk-a",
		urlB: "http://a/v1", keyB: "sk-b",
		wantA: 1, wantB: 1,
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			var enteredA, enteredB int
			releaseA := make(chan struct{})
			releaseB := make(chan struct{})

			blockingClient := func(entered *int, release chan struct{}) *http.Client {
				return testy.HTTPClient(func(*http.Request) (*http.Response, error) {
					mu.Lock()
					*entered++
					mu.Unlock()
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
						Header:     make(http.Header),
					}, nil
				})
			}

			urlA, err := url.Parse(tt.urlA)
			if err != nil {
				t.Fatal(err)
			}
			urlB, err := url.Parse(tt.urlB)
			if err != nil {
				t.Fatal(err)
			}
			backend := &BackendConfig{Backends: map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{BaseURL: urlA, APIKey: tt.keyA, HTTPClient: blockingClient(&enteredA, releaseA)}},
				"b": {ProviderType: "openai", Config: providers.Config{BaseURL: urlB, APIKey: tt.keyB, HTTPClient: blockingClient(&enteredB, releaseB)}},
			}}
			srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return backend, nil
			}})
			srv.logger = testLogger(t)
			srv.maxConcurrentPerUpstream = 1

			post := func(model string) {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
					strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer user-token")
				req.Header.Set("Content-Type", "application/json")
				srv.ServeHTTP(httptest.NewRecorder(), req)
			}

			go post("a/m")
			synctest.Wait()

			go post("b/m")
			synctest.Wait()

			mu.Lock()
			if enteredA != tt.wantA || enteredB != tt.wantB {
				t.Errorf("entered counts: enteredA=%d enteredB=%d, want %d and %d", enteredA, enteredB, tt.wantA, tt.wantB)
			}
			mu.Unlock()

			close(releaseA)
			close(releaseB)
			synctest.Wait()
		})
	})
}

func TestChatCompletionsConcurrencyCapGrowth(t *testing.T) {
	type test struct {
		wantEntered int
	}

	tests := testy.NewTable[test]()

	tests.Add("should leave the cap unchanged after a success that never waited for a slot", test{
		wantEntered: 1,
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			var calls, entered int
			release := make(chan struct{})

			client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
				mu.Lock()
				calls++
				held := calls > 1
				if held {
					entered++
				}
				mu.Unlock()
				if held {
					<-release
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"id":"ok"}`)),
					Header:     make(http.Header),
				}, nil
			})

			backend := &BackendConfig{Backends: map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: client}},
			}}
			srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return backend, nil
			}})
			srv.logger = testLogger(t)
			srv.maxConcurrentPerUpstream = 1

			post := func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
					strings.NewReader(`{"model":"a/m","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer user-token")
				req.Header.Set("Content-Type", "application/json")
				srv.ServeHTTP(httptest.NewRecorder(), req)
			}

			post()
			synctest.Wait()

			go post()
			synctest.Wait()
			go post()
			synctest.Wait()

			mu.Lock()
			got := entered
			mu.Unlock()
			if got != tt.wantEntered {
				t.Errorf("requests admitted to the upstream after an uncontended success: got %d, want %d", got, tt.wantEntered)
			}

			close(release)
			synctest.Wait()
		})
	})
}

func TestUpstreamLimiterAcquire(t *testing.T) {
	t.Parallel()

	l := newUpstreamLimiter(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := l.acquire(ctx); err != nil {
		t.Errorf("acquire with a free slot and a cancelled context: got err %v, want nil", err)
	}
	if got := l.inFlight(); got != 1 {
		t.Errorf("inFlight after acquiring a free slot despite a cancelled context: got %d, want 1", got)
	}
}

// acquirable drains the limiter's free slots via tryAcquire and reports how many
// it took — the observable capacity, without reading the private limit field.
func acquirable(l *upstreamLimiter) int {
	n := 0
	for l.tryAcquire() {
		n++
	}
	return n
}

func TestUpstreamLimiterShrink(t *testing.T) {
	t.Parallel()

	type test struct {
		start     int
		wantSlots int
	}

	tests := testy.NewTable[test]()

	tests.Add("should halve the cap from four to two", test{
		start:     4,
		wantSlots: 2,
	})
	tests.Add("should floor the cap at one when shrinking from two", test{
		start:     2,
		wantSlots: 1,
	})
	tests.Add("should leave the cap at one as a no-op", test{
		start:     1,
		wantSlots: 1,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		l := newUpstreamLimiter(tt.start)
		l.shrink()

		if got := acquirable(l); got != tt.wantSlots {
			t.Errorf("acquirable slots after shrink: got %d, want %d", got, tt.wantSlots)
		}
	})
}

func TestUpstreamLimiterGrow(t *testing.T) {
	t.Parallel()

	type test struct {
		newLimiter func() *upstreamLimiter
		wantSlots  int
	}

	tests := testy.NewTable[test]()

	tests.Add("should raise the cap past the starting allowance", test{
		newLimiter: func() *upstreamLimiter {
			l := newUpstreamLimiter(2)
			l.grow()
			l.grow()
			return l
		},
		wantSlots: 4,
	})
	tests.Add("should resume growing after a shrink", test{
		newLimiter: func() *upstreamLimiter {
			l := newUpstreamLimiter(2)
			l.shrink() // -> 1
			l.grow()
			l.grow() // -> 3
			return l
		},
		wantSlots: 3,
	})

	tests.Add("should raise the cap by one slot per round of successes once it reaches the known-good boundary", test{
		newLimiter: func() *upstreamLimiter {
			l := newUpstreamLimiter(4)
			for range 4 {
				l.tryAcquire()
			}
			l.throttle() // measures a cap of 3 and records it as known-good
			for range 4 {
				l.release()
			}
			for range 3 {
				l.grow()
			}
			return l
		},
		wantSlots: 4,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		if got := acquirable(tt.newLimiter()); got != tt.wantSlots {
			t.Errorf("acquirable slots: got %d, want %d", got, tt.wantSlots)
		}
	})
}

func TestUpstreamLimiterThrottle(t *testing.T) {
	t.Parallel()

	type test struct {
		start     int
		acquire   int
		throttles int
		// releaseBetween slots are released after the first throttle, so a later
		// one can arrive with fewer in flight than the throttle that preceded it.
		releaseBetween int
		wantSlots      int
	}

	tests := testy.NewTable[test]()

	tests.Add("should seed the cap to the observed in-flight count on the first signal-less rate limit", test{
		start:     8,
		acquire:   3,
		throttles: 1,
		wantSlots: 3,
	})
	tests.Add("should measure the cap when a saturated rate limit follows one that arrived below the cap", test{
		start:     8,
		acquire:   4,
		throttles: 2,
		wantSlots: 3,
	})
	tests.Add("should set the cap just below the in-flight count when a signal-less rate limit arrives at saturation", test{
		start:     4,
		acquire:   4,
		throttles: 1,
		wantSlots: 3,
	})
	tests.Add("should measure the cap again rather than halve when a repeat rate limit arrives above the known-good boundary", test{
		start:     4,
		acquire:   4,
		throttles: 2,
		wantSlots: 3,
	})
	tests.Add("should hold the cap at one when a signal-less rate limit arrives at a saturated cap of one", test{
		start:     1,
		acquire:   1,
		throttles: 1,
		wantSlots: 1,
	})
	tests.Add("should halve the cap when a repeat rate limit arrives at or below the known-good boundary", test{
		start:          5,
		acquire:        5,
		throttles:      2,
		releaseBetween: 2,
		wantSlots:      2,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		l := newUpstreamLimiter(tt.start)
		for range tt.acquire {
			if !l.tryAcquire() {
				t.Fatal("could not fill the limiter for the throttle setup")
			}
		}

		held := tt.acquire
		for i := range tt.throttles {
			if i > 0 {
				for range tt.releaseBetween {
					l.release()
					held--
				}
			}
			l.throttle()
		}

		// Release the held slots so the drain observes the post-throttle cap, not
		// what is left above the in-flight count.
		for range held {
			l.release()
		}

		if got := acquirable(l); got != tt.wantSlots {
			t.Errorf("acquirable slots after throttle: got %d, want %d", got, tt.wantSlots)
		}
	})
}

func TestFDSlotBudget(t *testing.T) {
	t.Parallel()

	type test struct {
		soft uint64
		want int
	}

	tests := testy.NewTable[test]()

	tests.Add("should size the budget as the FD headroom over the reserve, divided by the per-slot cost", test{
		soft: 1024,
		want: 480,
	})
	tests.Add("should floor at one slot when the FD limit is at or below the reserve", test{
		soft: 64,
		want: 1,
	})
	tests.Add("should floor at one slot when the budget would round below one", test{
		soft: 65,
		want: 1,
	})
	tests.Add("should grow the budget on a host with more file descriptors", test{
		soft: 65536,
		want: 32736,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		if got := fdSlotBudget(tt.soft); got != tt.want {
			t.Errorf("fdSlotBudget(%d): got %d, want %d", tt.soft, got, tt.want)
		}
	})
}

func TestServerProcessSlotBudget(t *testing.T) {
	t.Parallel()

	type test struct {
		opt       Option
		wantSlots int
	}

	tests := testy.NewTable[test]()

	tests.Add("should size the process budget from the fallback when the FD limit is unavailable", test{
		opt:       withoutFDSoftLimit(),
		wantSlots: fdSlotBudget(defaultFDSoftLimitFallback),
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		s := newServer(&stubValidator{}, tt.opt)

		if got := acquirable(s.processLimiter); got != tt.wantSlots {
			t.Errorf("process slot budget: got %d, want %d", got, tt.wantSlots)
		}
	})
}

// retryAfterErr wraps an error with a fixed RetryAfter deadline, satisfying the
// interface classifyLimit looks up via errors.AsType.
type retryAfterErr struct {
	error
	at time.Time
}

func (e retryAfterErr) RetryAfter() time.Time { return e.at }
func (e retryAfterErr) Unwrap() error         { return e.error }

func TestClassifyLimit(t *testing.T) {
	t.Parallel()

	type test struct {
		buildErr func() error
		want     limitClassification
	}

	tests := testy.NewTable[test]()

	tests.Add("should classify a nil error as the zero classification", test{
		buildErr: func() error { return nil },
		want:     limitClassification{},
	})
	tests.Add("should classify a non-429 as not rate limited", test{
		buildErr: func() error {
			return yerrors.WithHTTPStatus(http.StatusBadGateway, errors.New("upstream down"))
		},
		want: limitClassification{
			status:      http.StatusBadGateway,
			isRateLimit: false,
			condition:   notRateLimited,
		},
	})
	tests.Add("should classify a 429 with no Retry-After as signalless", test{
		buildErr: func() error {
			return yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited"))
		},
		want: limitClassification{
			status:      http.StatusTooManyRequests,
			isRateLimit: true,
			condition:   signalless,
		},
	})
	tests.AddFunc("should classify a 429 with a Retry-After at the sustained-rate threshold as sustainedRate", func(*testing.T) test {
		return test{
			buildErr: func() error {
				return retryAfterErr{
					error: yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited")),
					at:    time.Now().Add(rateLimitDemotionThreshold),
				}
			},
			want: limitClassification{
				status:        http.StatusTooManyRequests,
				isRateLimit:   true,
				hasRetryAfter: true,
				retryAfter:    rateLimitDemotionThreshold,
				condition:     sustainedRate,
			},
		}
	})
	tests.AddFunc("should classify a 429 with a Retry-After just below the sustained-rate threshold as transientRate", func(*testing.T) test {
		return test{
			buildErr: func() error {
				return retryAfterErr{
					error: yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited")),
					at:    time.Now().Add(rateLimitDemotionThreshold - time.Second),
				}
			},
			want: limitClassification{
				status:        http.StatusTooManyRequests,
				isRateLimit:   true,
				hasRetryAfter: true,
				retryAfter:    rateLimitDemotionThreshold - time.Second,
				condition:     transientRate,
			},
		}
	})
	tests.AddFunc("should reject a 429 with a Retry-After beyond the passthrough ceiling", func(*testing.T) test {
		return test{
			buildErr: func() error {
				return retryAfterErr{
					error: yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited")),
					at:    time.Now().Add(maxPassthroughRetryAfter + time.Second),
				}
			},
			want: limitClassification{
				status:        http.StatusTooManyRequests,
				isRateLimit:   true,
				hasRetryAfter: true,
				retryAfter:    maxPassthroughRetryAfter + time.Second,
				shouldReject:  true,
				condition:     sustainedRate,
			},
		}
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			got := classifyLimit(tt.buildErr())
			if d := gocmp.Diff(tt.want, got, gocmp.AllowUnexported(limitClassification{})); d != "" {
				t.Errorf("unexpected classification: %s", d)
			}
		})
	})
}

func TestChatCompletionsResponseInterceptorBody(t *testing.T) {
	t.Parallel()

	type test struct {
		upstreamBody  string
		contentLength string // "" = no Content-Length header on the upstream response
		replacement   string // interceptor swaps the body for this
		want          string // the client should read this in full
	}

	tests := testy.NewTable[test]()

	tests.Add("should relay the interceptor's rewritten body to the client", test{
		upstreamBody: `{"id":"chatcmpl-upstream","choices":[]}`,
		replacement:  `{"intercepted":true}`,
		want:         `{"intercepted":true}`,
	})
	tests.Add("should drop a stale Content-Length so a longer rewrite reaches the client intact", test{
		upstreamBody:  `{}`,
		contentLength: "2",
		replacement:   `{"intercepted":"aaaaaaaaaaaaaaaaaaaa"}`,
		want:          `{"intercepted":"aaaaaaaaaaaaaaaaaaaa"}`,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			header := make(http.Header)
			if tt.contentLength != "" {
				header.Set("Content-Length", tt.contentLength)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(tt.upstreamBody)),
				Header:     header,
			}, nil
		})

		interceptor := func(_ context.Context, _ RequestMetadata, resp *http.Response) error {
			resp.Body = io.NopCloser(strings.NewReader(tt.replacement))
			return nil
		}

		srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}, WithLogger(testLogger(t)), WithResponseInterceptor(interceptor))

		// A real server (not httptest.NewRecorder) is required: only it enforces
		// Content-Length framing, which the drop-stale-length case observes.
		ts := httptest.NewServer(srv)
		defer ts.Close()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(body); got != tt.want {
			t.Errorf("body: got %q, want %q", got, tt.want)
		}
	})
}

func TestChatCompletionsResponseInterceptorReceivesRequestMetadata(t *testing.T) {
	t.Parallel()

	resp := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
	}
	backend := func(host, key string, respFn func() *http.Response) Backend {
		return Backend{ProviderType: "openai", Config: providers.Config{
			BaseURL:    &url.URL{Scheme: "http", Host: host, Path: "/v1"},
			APIKey:     key,
			HTTPClient: testy.HTTPClient(func(*http.Request) (*http.Response, error) { return respFn(), nil }),
		}}
	}
	okResp := func() *http.Response { return resp(http.StatusOK, `{}`) }

	type test struct {
		backends map[string]Backend
		models   map[string]LogicalModel
		model    string          // the model the request asks for
		token    string          // the bearer token the request carries
		advances []time.Duration // clock advance before each serve; one entry per serve
		want     RequestMetadata // the served target the interceptor should receive after the last serve
	}

	tests := testy.NewTable[test]()

	tests.Add("should reflect a directly-addressed backend and model", test{
		backends: map[string]Backend{"openai": backend("backend", "sk-test", okResp)},
		model:    "openai/gpt-4",
		token:    "user-token",
		advances: []time.Duration{0},
		want:     RequestMetadata{Backend: "openai", Model: "gpt-4", Token: "user-token"},
	})
	tests.Add("should reflect the failed-over target, not the requested one", test{
		backends: map[string]Backend{
			"a": backend("a", "sk-a", func() *http.Response { return resp(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`) }),
			"b": backend("b", "sk-b", func() *http.Response { return resp(http.StatusOK, `{"id":"from-b"}`) }),
		},
		models:   map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}}},
		model:    "m",
		token:    "user-token",
		advances: []time.Duration{0, 16 * time.Second},
		want:     RequestMetadata{Backend: "b", Model: "mb", Token: "user-token"},
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: tt.backends, Models: tt.models}, nil
			}}
			var got RequestMetadata
			srv := New(validator, WithLogger(testLogger(t)), WithResponseInterceptor(func(_ context.Context, served RequestMetadata, _ *http.Response) error {
				got = served
				return nil
			}))

			for _, adv := range tt.advances {
				if adv > 0 {
					time.Sleep(adv)
					synctest.Wait()
				}
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
					strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, tt.model)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+tt.token)
				req.Header.Set("Content-Type", "application/json")
				srv.ServeHTTP(httptest.NewRecorder(), req)
			}

			if d := gocmp.Diff(tt.want, got); d != "" {
				t.Errorf("served target: %s", d)
			}
		})
	})
}

type trackingBody struct {
	io.Reader
	closed bool
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

func TestChatCompletionsResponseInterceptorContracts(t *testing.T) {
	t.Parallel()

	type test struct {
		interceptor ResponseInterceptor
		upstream    *http.Response
		rr          *httptest.ResponseRecorder
		check       func()
	}

	okResponse := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"upstream-ok"}`)),
			Header:     make(http.Header),
		}
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should let the interceptor set a response header", func(t *testing.T) test {
		rr := httptest.NewRecorder()
		return test{
			interceptor: func(_ context.Context, _ RequestMetadata, resp *http.Response) error {
				resp.Header.Set("Set-Cookie", "sid=secret")
				return nil
			},
			upstream: okResponse(),
			rr:       rr,
			check: func() {
				if got := rr.Header().Get("Set-Cookie"); got != "sid=secret" {
					t.Errorf("interceptor-set response header stripped: Set-Cookie = %q, want %q", got, "sid=secret")
				}
			},
		}
	})

	tests.AddFunc("should surface the interceptor's error without relaying the upstream body", func(t *testing.T) test {
		rr := httptest.NewRecorder()
		return test{
			interceptor: func(_ context.Context, _ RequestMetadata, _ *http.Response) error {
				return yerrors.WithHTTPStatus(http.StatusBadGateway, errors.New("interceptor boom"))
			},
			upstream: okResponse(),
			rr:       rr,
			check: func() {
				if rr.Code != http.StatusBadGateway {
					t.Errorf("status: got %d, want %d", rr.Code, http.StatusBadGateway)
				}
				if strings.Contains(rr.Body.String(), "upstream-ok") {
					t.Errorf("upstream body relayed despite interceptor error: %q", rr.Body.String())
				}
			},
		}
	})

	tests.AddFunc("should release the upstream body by closing the interceptor's swapped body through to it", func(t *testing.T) test {
		rr := httptest.NewRecorder()
		orig := &trackingBody{Reader: strings.NewReader(`{"id":"upstream-ok"}`)}
		return test{
			// A well-behaved interceptor swaps in a body that closes through to the
			// original (as requestlog's closeThrough does), so closing resp.Body
			// releases the upstream connection per the normal http.Response contract.
			interceptor: func(_ context.Context, _ RequestMetadata, resp *http.Response) error {
				resp.Body = struct {
					io.Reader
					io.Closer
				}{strings.NewReader(`{"intercepted":true}`), orig}
				return nil
			},
			upstream: &http.Response{StatusCode: http.StatusOK, Body: orig, Header: make(http.Header)},
			rr:       rr,
			check: func() {
				if !orig.closed {
					t.Error("upstream body not released: closing the swapped body did not close through to the original")
				}
			},
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return tt.upstream, nil
		})
		srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
		}}, WithLogger(testLogger(t)), WithResponseInterceptor(tt.interceptor))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")

		srv.ServeHTTP(tt.rr, req)
		tt.check()
	})
}

// Remaining behavior for the response-interceptor seam.
// TODO(TODO.d/understudy-response-interceptor.md): should extend RequestMetadata with
// RequestedModel — deferred until lindy's provenance interceptor reads it.

func TestShouldPanicAtConstructionWhenWithProviderRegistersANilHandler(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("expected construction to panic on a nil provider handler")
		}
	}()
	New(&stubValidator{}, WithLogger(testLogger(t)), WithProvider("acme", nil))
}

func TestShouldPanicAtConstructionWhenTwoOptionsRegisterTheSameProviderName(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("expected construction to panic on a duplicate provider registration")
		}
	}()
	New(&stubValidator{}, WithLogger(testLogger(t)),
		WithProvider("acme", fakeChatProvider{}),
		WithProvider("acme", fakeChatProvider{}),
	)
}

type fakeChatProvider struct {
	response string
	models   []providers.Model
}

func (p fakeChatProvider) Chat(context.Context, providers.Config, io.Reader) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(p.response)),
		Header:     make(http.Header),
	}, nil
}

func (p fakeChatProvider) Models(context.Context, providers.Config) ([]providers.Model, error) {
	return p.models, nil
}

// Not parallel: swaps the process-default logger.
func TestShouldLogViaProcessDefaultLoggerWithoutLoggerOption(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default() //nolint:forbidigo // saved to restore the swapped process-default logger
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})
	srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
		return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
	}})

	filler := strings.Repeat("x", maxPrefixScan+1)
	body := fmt.Sprintf(`{"filler":%q,"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`, filler)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer user-token")
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("request did not proceed: status %d", rr.Code)
	}
	if !strings.Contains(buf.String(), "model field beyond prefix scan threshold") {
		t.Errorf("expected the prefix-scan log on the process-default logger, got: %q", buf.String())
	}
}

// TODO(TODO.d/auth-requirement-and-key-env-source.md): once auth="auto" exists,
// this becomes a table with a second case — a backend is dropped because the
// variable it names is unset. That mapping is what makes examples/free-tiers.toml
// drop-in, and the api_key_file-driven drop cases in config_test.go cannot prove
// it. Until then an unset variable silently resolves to an empty key, which is
// neither designed answer.
func TestChatCompletionsAuthenticatesUpstreamWithEnvNamedCredential(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "sk-from-env")

	cfg := Config{
		Backends: map[string]BackendSpec{
			//nolint:gosec // G101 fires on the api_key_env name; the value is the variable's name, not its contents.
			"groq": {ProviderType: "openai", BaseURL: "http://groq/v1", APIKeyEnv: "GROQ_API_KEY"},
		},
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var gotAuth string
	backend := resolved.Backends["groq"]
	backend.Config.HTTPClient = testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Header:     make(http.Header),
		}, nil
	})
	resolved.Backends["groq"] = backend

	validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
		return resolved, nil
	}}
	srv := New(validator, WithLogger(testLogger(t)))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"groq/gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer user-token")
	req.Header.Set("Content-Type", "application/json")

	srv.ServeHTTP(httptest.NewRecorder(), req)

	if want := "Bearer sk-from-env"; gotAuth != want {
		t.Errorf("upstream call authenticated with %q, want %q", gotAuth, want)
	}
}
