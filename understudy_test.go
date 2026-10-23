package understudy

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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

	tests.AddFunc("should serve a proxied request through the HTTP client injected with WithHTTPClient", func(t *testing.T) test {
		// The backend's address is unservable, so the stock client can only
		// fail the request: any response is one the injected client carried.
		injected := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"served-by-injected-client"}`)),
				Header:     make(http.Header),
			}, nil
		})
		return test{
			authHeader: "Bearer user-token",
			validate: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://127.0.0.1:1/v1", "sk-test", nil), nil
			},
			opts:       []Option{WithHTTPClient(injected)},
			wantStatus: http.StatusOK,
			wantBody:   `{"id":"served-by-injected-client"}`,
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
		return New(validator, WithLogger(testLogger(t)), WithResponseInterceptor(interceptor)).server
	}

	type test struct {
		// ctx overrides the request context (defaults to t.Context()); set it to
		// exercise cancellation paths.
		ctx         context.Context
		requestBody string
		server      *server
		wantStatus  int
		wantBody    string
		// wantRawBody is compared byte for byte; use it for non-JSON bodies
		// like streams, which wantBody's JSON comparison cannot match.
		wantRawBody         string
		wantBodyContains    []string
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

	tests.AddFunc("should proxy every frame of a streamed 200 response body to client", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				// One frame per Read, as a stream delivers.
				frames := []string{": keep-alive\n\n", "data: {\"id\":\"chatcmpl-123\"}\n\n", "data: [DONE]\n\n"}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						if len(frames) == 0 {
							return 0, io.EOF
						}
						n := copy(p, frames[0])
						if frames[0] = frames[0][n:]; frames[0] == "" {
							frames = frames[1:]
						}
						return n, nil
					})),
					Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusOK,
			wantRawBody:         ": keep-alive\n\ndata: {\"id\":\"chatcmpl-123\"}\n\ndata: [DONE]\n\n",
			wantResponseHeaders: http.Header{"Content-Type": []string{"text/event-stream"}},
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
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"5"}},
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

	tests.AddFunc("should forward an upstream 5xx's own Retry-After", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     http.Header{"Retry-After": {"20"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"try later"}}`)),
				}, nil
			}, nil),
			wantStatus:          http.StatusBadGateway,
			wantBody:            `{"error":{"message":"Bad Gateway","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"20"}},
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

	tests.AddFunc("should synthesize a backoff for a 429 whose Retry-After has already elapsed", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"Mon, 02 Jan 2006 15:04:05 GMT"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusTooManyRequests,
			wantBody:            `{"error":{"message":"upstream returned status 429: slow down","type":"rate_limit_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"5"}},
		}
	})

	tests.AddFunc("should not relay a delay sent by a failure no retry can help", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusNotImplemented,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"not implemented"}}`)),
					Header:     http.Header{"Retry-After": {"30"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusBadGateway,
			wantBody:            `{"error":{"message":"Bad Gateway","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should not reject on a long delay sent by a failure no retry can help", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusNotImplemented,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"not implemented"}}`)),
					Header:     http.Header{"Retry-After": {"600"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusBadGateway,
			wantBody:            `{"error":{"message":"Bad Gateway","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should not relay a 5xx's Retry-After once the moment it named has passed", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"back shortly"}}`)),
					Header:     http.Header{"Retry-After": {"Mon, 02 Jan 2006 15:04:05 GMT"}},
				}, nil
			}, nil),
			wantStatus:          http.StatusBadGateway,
			wantBody:            `{"error":{"message":"Bad Gateway","type":"server_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"5"}},
		}
	})

	tests.AddFunc("should relay the Retry-After a 503 sent on the 502 it answers with", func(t *testing.T) test {
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
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"5"}},
		}
	})

	tests.AddFunc("should reject a reference naming no backend rather than reading it as an unknown one", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).server,
			requestBody:         `{"model":"/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"target \"/gpt-4\" must be <backend>/<model>","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should reject a reserved override key that would rewrite what the request is", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "backend", Path: "/v1"}, APIKey: "sk-test"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).server,
			requestBody:         `{"model":"openai/gpt-4?messages=[]","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"model \"openai/gpt-4?messages=[]\": override key \"messages\" is reserved","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should reject a reserved override key that would rewrite the caller's response contract", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "backend", Path: "/v1"}, APIKey: "sk-test"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).server,
			requestBody:         `{"model":"openai/gpt-4?stream=true","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"model \"openai/gpt-4?stream=true\": override key \"stream\" is reserved","type":"invalid_request_error"}}`,
			wantResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		}
	})

	tests.AddFunc("should reject a repeated override key rather than silently keeping only one value", func(t *testing.T) test {
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return &BackendConfig{Backends: map[string]Backend{
				"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "backend", Path: "/v1"}, APIKey: "sk-test"}},
			}}, nil
		}}
		return test{
			server:              New(validator, WithLogger(testLogger(t))).server,
			requestBody:         `{"model":"openai/gpt-4?temperature=0.7&temperature=1.5","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"model \"openai/gpt-4?temperature=0.7&temperature=1.5\": override key \"temperature\" is repeated","type":"invalid_request_error"}}`,
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
			server:              New(validator, WithLogger(testLogger(t))).server,
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
			server:              New(validator, WithLogger(testLogger(t))).server,
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
			server:              New(validator, WithLogger(testLogger(t))).server,
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

	tests.AddFunc("should return 400 with invalid_request_error when a body with an override runs malformed after the model field", func(t *testing.T) test {
		return test{
			server: defaultServer(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
			}, nil),
			requestBody:         `{"model":"openai/gpt-4?temperature=0.7",`,
			wantStatus:          http.StatusBadRequest,
			wantBody:            `{"error":{"message":"malformed request body: EOF","type":"invalid_request_error"}}`,
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
				"Retry-After":  {"5"},
			},
		}
	})

	tests.AddFunc("should answer a model whose every target is unusable as one declaring no targets", func(t *testing.T) test {
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
			server:              New(validator, WithLogger(testLogger(t))).server,
			requestBody:         `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:          http.StatusNotFound,
			wantBody:            `{"error":{"message":"logical model \"m\" has no targets","type":"invalid_request_error"}}`,
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
			server:              New(validator, WithLogger(testLogger(t))).server,
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
			server:     New(validator, WithLogger(testLogger(t))).server,
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
			server:     New(validator, WithLogger(testLogger(t))).server,
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
			server:     New(validator, WithLogger(testLogger(t))).server,
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
			server:      New(validator, WithLogger(testLogger(t))).server,
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
			server:     New(validator, WithLogger(testLogger(t))).server,
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
			server:     New(validator, WithLogger(testLogger(t))).server,
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
			server:              New(validator, WithLogger(testLogger(t))).server,
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
		// These cases assert the interval exactly; scattering it is
		// TestChatCompletionsScattersBusyBackoff's subject.
		srv.jitterFactor = 0

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
		if tt.wantBody != "" {
			if d := testy.DiffJSON([]byte(tt.wantBody), rr.Body.Bytes()); d != nil {
				t.Errorf("unexpected body: %s", d)
			}
		}
		if tt.wantRawBody != "" && rr.Body.String() != tt.wantRawBody {
			t.Errorf("unexpected body: got %q, want %q", rr.Body.String(), tt.wantRawBody)
		}
		for _, want := range tt.wantBodyContains {
			if !strings.Contains(rr.Body.String(), want) {
				t.Errorf("body does not mention %q: %s", want, rr.Body.String())
			}
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
		// wantExcluded is what the request's LogRecord should name on Excluded;
		// nil asserts nothing, so only a case about a left-out backend sets it.
		wantExcluded []Attempt
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

	tests.Add("should answer an empty catalog when the only backend has no base URL", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: map[string]Backend{
					"openai": {ProviderType: "openai", Config: providers.Config{BaseURL: nil, APIKey: "sk-models"}},
				}}, nil
			},
		},
		wantStatus: http.StatusOK,
		wantBody:   `{"object":"list","data":[]}`,
	})

	tests.Add("should answer an empty catalog when no backend can be used", test{
		validator: &stubValidator{
			ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: nil}, nil
			},
		},
		wantStatus: http.StatusOK,
		wantBody:   `{"object":"list","data":[]}`,
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
			wantBody:   `{"object":"list","data":[]}`,
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
			wantBody:   `{"object":"list","data":[]}`,
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

	tests.AddFunc("should record a backend whose catalog fetch failed as an attempt it made", func(t *testing.T) test {
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
			// The listing does not fail; the operator reads the failure on Excluded,
			// where an unusable backend is already recorded.
			wantExcluded: []Attempt{{
				Backend:        "down",
				Called:         true,
				UpstreamStatus: http.StatusInternalServerError,
				Err:            errors.New("upstream returned status 500: catalog unavailable"),
			}},
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
		ctx := WithLogCtx(req.Context())
		srv.ServeHTTP(rr, req.WithContext(ctx))

		if rr.Code != tt.wantStatus {
			t.Errorf("unexpected status: got %d, want %d", rr.Code, tt.wantStatus)
		}
		if d := testy.DiffJSON([]byte(tt.wantBody), rr.Body.Bytes()); d != nil {
			t.Errorf("unexpected body: %s", d)
		}
		rec, _ := LogRecordFromContext(ctx)
		if d := gocmp.Diff(tt.wantExcluded, rec.Excluded, errorText, cmpopts.EquateEmpty(), assertedFields); d != "" {
			t.Errorf("excluded attempts (-want +got):\n%s", d)
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

func TestChatCompletionsForwardedRequest(t *testing.T) {
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

	tests.AddFunc("should send an x-opencode-session header to an opencode.ai backend", func(t *testing.T) test {
		var forwarded bool
		var session string
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			forwarded = true
			session = req.Header.Get("X-Opencode-Session")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		})
		return test{
			requestModel: "openai/gpt-4",
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "https://opencode.ai/zen/go/v1", "sk-test", client), nil
			}},
			check: func() {
				if !forwarded {
					t.Fatal("no request reached the upstream")
				}
				if session == "" {
					t.Error("upstream request carried no x-opencode-session header")
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

func TestChatCompletionsSessionHeaderScoping(t *testing.T) {
	t.Parallel()

	// call is one chat-completions request.
	type call struct {
		token        string
		conversation string
	}
	type test struct {
		callA, callB call
		wantEqual    bool
	}

	tests := testy.NewTable[test]()

	tests.Add("should send the same x-opencode-session header for a repeated call", test{
		callA:     call{token: "user-token", conversation: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"tell me about pelicans"}]}`},
		callB:     call{token: "user-token", conversation: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"tell me about pelicans"}]}`},
		wantEqual: true,
	})

	tests.Add("should send a different x-opencode-session header for a different conversation", test{
		callA: call{token: "user-token", conversation: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"tell me about pelicans"}]}`},
		callB: call{token: "user-token", conversation: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"what is the capital of France"}]}`},
	})

	tests.Add("should send a different x-opencode-session header for a different caller", test{
		callA: call{token: "user-token", conversation: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"tell me about pelicans"}]}`},
		callB: call{token: "other-token", conversation: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"tell me about pelicans"}]}`},
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		var sessions []string
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			sessions = append(sessions, req.Header.Get("X-Opencode-Session"))
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		})
		validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "https://opencode.ai/zen/go/v1", "sk-test", client), nil
		}}
		srv := New(validator, WithLogger(testLogger(t)))

		for _, c := range []call{tt.callA, tt.callB} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(c.conversation))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Content-Type", "application/json")
			srv.ServeHTTP(httptest.NewRecorder(), req)
		}

		if len(sessions) != 2 {
			t.Fatalf("upstream received %d requests, want 2", len(sessions))
		}
		if sessions[0] == "" {
			t.Fatal("x-opencode-session header must not be empty")
		}
		equal := sessions[0] == sessions[1]
		if equal != tt.wantEqual {
			t.Errorf("got %q and %q (equal=%v); want equal=%v", sessions[0], sessions[1], equal, tt.wantEqual)
		}
	})
}

func TestChatCompletionsQueryOverrides(t *testing.T) {
	t.Parallel()

	type test struct {
		requestBody string
		targetQuery url.Values
		want        map[string]json.RawMessage
		wantKeys    map[string]int
		// wantOnlyKeys asserts the forwarded body has exactly these top-level
		// keys — nothing added, nothing dropped. Always checked, every case.
		wantOnlyKeys []string
	}

	tests := testy.NewTable[test]()

	tests.Add("should forward a thinking=false override as the literal boolean it parses to", test{
		requestBody:  `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  url.Values{"thinking": {"false"}},
		want:         map[string]json.RawMessage{"thinking": json.RawMessage("false")},
		wantKeys:     map[string]int{"thinking": 1},
		wantOnlyKeys: []string{"model", "messages", "thinking"},
	})
	tests.Add("should forward an object-valued override the request omits, written explicitly", test{
		requestBody:  `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  url.Values{"thinking": {`{"type":"disabled"}`}},
		want:         map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"disabled"}`)},
		wantKeys:     map[string]int{"thinking": 1},
		wantOnlyKeys: []string{"model", "messages", "thinking"},
	})
	tests.Add("should override a value the request already carries", test{
		requestBody:  `{"thinking":true,"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  url.Values{"thinking": {"false"}},
		want:         map[string]json.RawMessage{"thinking": json.RawMessage("false")},
		wantKeys:     map[string]int{"thinking": 1},
		wantOnlyKeys: []string{"model", "messages", "thinking"},
	})
	tests.Add("should replace an object value the request already carries", test{
		requestBody:  `{"thinking":{"type":"enabled"},"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  url.Values{"thinking": {`{"type":"disabled"}`}},
		want:         map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"disabled"}`)},
		wantKeys:     map[string]int{"thinking": 1},
		wantOnlyKeys: []string{"model", "messages", "thinking"},
	})
	tests.Add("should forward a numeric override value as a JSON number", test{
		requestBody:  `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  url.Values{"temperature": {"0.7"}},
		want:         map[string]json.RawMessage{"temperature": json.RawMessage("0.7")},
		wantKeys:     map[string]int{"temperature": 1},
		wantOnlyKeys: []string{"model", "messages", "temperature"},
	})
	tests.Add("should forward a bare-word override value as a JSON string", test{
		requestBody:  `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  url.Values{"reasoning_effort": {"high"}},
		want:         map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"high"`)},
		wantKeys:     map[string]int{"reasoning_effort": 1},
		wantOnlyKeys: []string{"model", "messages", "reasoning_effort"},
	})
	tests.Add("should apply an override carried by a backend/model reference the request names", test{
		requestBody:  `{"model":"zai/glm-5?reasoning_effort=high","messages":[{"role":"user","content":"hi"}]}`,
		want:         map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"high"`)},
		wantKeys:     map[string]int{"reasoning_effort": 1},
		wantOnlyKeys: []string{"model", "messages", "reasoning_effort"},
	})
	tests.Add("should forward a quoted override value as the literal string it names, not the bool it looks like", test{
		requestBody:  `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  url.Values{"param": {`"true"`}},
		want:         map[string]json.RawMessage{"param": json.RawMessage(`"true"`)},
		wantKeys:     map[string]int{"param": 1},
		wantOnlyKeys: []string{"model", "messages", "param"},
	})

	tests.Add("should add no override key when the target carries none", test{
		requestBody:  `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		targetQuery:  nil,
		wantOnlyKeys: []string{"model", "messages"},
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

		var got map[string]json.RawMessage
		if err := json.Unmarshal(forwarded, &got); err != nil {
			t.Fatalf("could not parse forwarded body %q: %v", forwarded, err)
		}
		for key, want := range tt.want {
			if !bytes.Equal(bytes.TrimSpace(got[key]), want) {
				t.Errorf("forwarded %s: got %s, want %s", key, got[key], want)
			}
		}
		for key, want := range tt.wantKeys {
			if gotCount := bytes.Count(forwarded, []byte(`"`+key+`"`)); gotCount != want {
				t.Errorf("forwarded %s key count: got %d, want %d", key, gotCount, want)
			}
		}
		want := slices.Sorted(slices.Values(tt.wantOnlyKeys))
		if d := gocmp.Diff(want, slices.Sorted(maps.Keys(got))); d != "" {
			t.Errorf("forwarded keys (-want +got):\n%s", d)
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

// stalledStreamFirstEvent is a complete SSE event so the stream stalls only
// after the client has received content.
const stalledStreamFirstEvent = "data: {}\n\n"

// sseData returns the data of each event in an SSE body, in order, dropping
// framing so tests compare only what a client reads.
func sseData(body string) []string {
	var events []string
	var data []string
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if data != nil {
				events = append(events, strings.Join(data, "\n"))
				data = nil
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	return events
}

func TestChatCompletionsBoundsStalledStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := testy.HTTPClient(func(req *http.Request) (*http.Response, error) {
			reqCtx := req.Context()
			first := true
			body := io.NopCloser(readerFunc(func(p []byte) (int, error) {
				if first {
					first = false
					return copy(p, stalledStreamFirstEvent), nil
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
		if d := gocmp.Diff([]string{"{}"}, sseData(rr.Body.String())); d != "" {
			t.Errorf("events the client received before the stall (-want +got):\n%s", d)
		}
	})
}

// TestShouldServeABackendThatTakesAnHourToSendItsResponseHeader pins that a
// backend which stays silent for an hour before its response header still
// serves, through the shared production HTTP client: no header timeout in
// that client cuts the wait. An hour outruns the first-content timeout's
// first value, so the request gets there only because the timeout escalates
// with each re-send.
func TestShouldServeABackendThatTakesAnHourToSendItsResponseHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const completion = `{"id":"chatcmpl-slow-header","object":"chat.completion","choices":[]}`

		// The actors are released and joined however the request ends, so
		// no goroutine outlives the test.
		release := make(chan struct{})
		var actors sync.WaitGroup
		defer func() {
			close(release)
			actors.Wait()
		}()
		serveSlowHeader := func(conn net.Conn) {
			defer actors.Done()
			defer conn.Close()
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				return
			}
			_, _ = io.ReadAll(req.Body)
			select {
			case <-time.After(time.Hour):
			case <-release:
				return
			}
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(completion), completion)
		}

		// The doubled client carries the shared production client's settings,
		// with only the dial swapped for the pipes, so the header-wait policy
		// under test is the production one.
		shared := providers.Config{}.Client()
		doubled := *shared
		sharedTransport, ok := shared.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("shared client transport: got %T, want *http.Transport", shared.Transport)
		}
		transport := sharedTransport.Clone()
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			clientConn, backendConn := net.Pipe()
			actors.Add(1)
			go serveSlowHeader(backendConn)
			return clientConn, nil
		}
		doubled.Transport = transport

		srv := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
			return openaiBackend(t, "http://backend/v1", "sk-test", &doubled), nil
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
		if d := testy.DiffJSON([]byte(completion), rr.Body.Bytes()); d != nil {
			t.Errorf("body: %s", d)
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
					return copy(p, stalledStreamFirstEvent), nil
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
		opts        []Option
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

	// TODO: cases for the model a response body names:
	//   - a name longer than maxModelNameBytes should leave model_responded
	//     empty rather than truncated.
	//   - a name that decodes to more than maxModelNameBytes though its escaped
	//     spelling is within it (invalid UTF-8 becoming 3-byte U+FFFD) should
	//     leave model_responded empty.
	//   - a name with an invalid escape (`x\q`) should leave model_responded
	//     empty.
	//   - a send the admission queue preempts before any content, and sends
	//     again, should log the model the re-sent answer names, not the
	//     preempted one's.
	//   - a body longer than maxBodyPrefixBytes that names the model within its
	//     first maxBodyPrefixBytes (a JSON object's early field, a stream's
	//     first chunk) should log that model, and the client should still
	//     receive the whole body.
	//   - a body that names the model only after its first maxBodyPrefixBytes
	//     (a long "choices" content ahead of a JSON object's "model", or
	//     keep-alives and earlier chunks ahead of a stream's naming chunk)
	//     currently comes back with model_responded empty; the bound is a
	//     limitation of ModelResponded, and whether to scan the body as it
	//     passes instead is undecided.
	tests.AddFunc("should log the model the response names", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","model":"gpt-4o-2024-11-20","choices":[]}`)),
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
				"model_responded": "gpt-4o-2024-11-20",
			},
		}
	})

	tests.AddFunc("should leave model_responded empty when the response names the model as a number", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","model":42,"choices":[]}`)),
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
				"model_responded": "",
			},
		}
	})

	tests.AddFunc("should log a JSON-escaped model name as its decoded name", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","model":"meta-llama\/Llama-3","choices":[]}`)),
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
				"model_responded": "meta-llama/Llama-3",
			},
		}
	})

	tests.AddFunc("should log a name whose decoded length is within the bound though its escaped spelling is longer", func(t *testing.T) test {
		name := strings.Repeat("a/", 100)
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			body := `{"id":"chatcmpl-1","model":"` + strings.ReplaceAll(name, "/", `\/`) + `","choices":[]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{},
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"model_responded": name,
			},
		}
	})

	tests.AddFunc("should log the model a streamed response names", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-2024-11-20\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
				"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-2024-11-20\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
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
				"model_responded": "gpt-4o-2024-11-20",
			},
		}
	})

	tests.AddFunc("should log the model a stream opening with a comment or event: line names", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			body := ": keep-alive\n\n" +
				"event: message\n" +
				"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-2024-11-20\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
				"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-2024-11-20\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
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
				"model_responded": "gpt-4o-2024-11-20",
			},
		}
	})

	tests.AddFunc("should log the model a later chunk names when a stream's first chunks name none", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			body := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
				"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
				"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-2024-11-20\",\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
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
				"model_responded": "gpt-4o-2024-11-20",
			},
		}
	})

	// TODO: cases for the model a streamed response names:
	//   - a stream with no chunk naming a model (only `data: [DONE]`, or
	//     payloads that are not JSON objects) should leave model_responded
	//     empty, and so should a bare error text beginning with "d".

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

	tests.AddFunc("should record a backend twice when the walk stepped over it on two passes", func(*testing.T) test {
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
						"broken":  {ProviderType: "openai", Config: providers.Config{APIKey: "sk-b"}},
						"limited": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "limited", Path: "/v1"}, APIKey: "sk-l", HTTPClient: limited}},
						"good":    {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "good", Path: "/v1"}, APIKey: "sk-g", HTTPClient: serving}},
					},
					Models: map[string]LogicalModel{"m": {Targets: []Target{
						{backend: "broken", model: "mb"},
						{backend: "limited", model: "ml"},
						{backend: "good", model: "mg"},
					}}},
				}, nil
			}},
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			// The replay re-walks from the start, so the unusable candidate ahead of
			// the throttled one is stepped over on both passes.
			want: map[string]any{
				"backend_name": "good",
				"excluded": []Attempt{
					{Backend: "broken", ModelUpstream: "mb", Err: errors.New("must provide base_url")},
					{Backend: "limited", ModelUpstream: "ml", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true},
					{Backend: "broken", ModelUpstream: "mb", Err: errors.New("must provide base_url")},
				},
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

	tests.AddFunc("should record why each backend was unusable when a model has nothing left to serve it", func(*testing.T) test {
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{
					Backends: map[string]Backend{
						"a": {ProviderType: "openai"},
						"b": {ProviderType: "nosuchprovider", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "b"}}},
					},
					Models: map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}}},
				}, nil
			}},
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"excluded": []Attempt{
					{Backend: "a", ModelUpstream: "ma", Err: errors.New("must provide base_url")},
					{Backend: "b", ModelUpstream: "mb", Err: errors.New(`provider type "nosuchprovider" has no registered handler`)},
				},
			},
		}
	})

	tests.AddFunc("should log the upstream's own words for a refusal the client is not told", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid api key"}}`)),
				Header:     http.Header{},
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want:        map[string]any{"error": "upstream returned status 401: invalid api key"},
		}
	})

	tests.AddFunc("should log the time from sending a request upstream to its first content", func(t *testing.T) test {
		// Distinct delays: a clock stopped early (at the header, 1s, or the
		// first framing line, 3s) reports a number the exact sum (10s) tells
		// apart.
		const (
			headerDelay    = 1 * time.Second
			keepAliveDelay = 2 * time.Second
			eventDelay     = 3 * time.Second
			dataDelay      = 4 * time.Second
		)
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(headerDelay)
			// Inside the runner's bubble, so the pipe producer's waits
			// advance its clock. Started with the response, so each delay
			// follows the header's arrival.
			bodyReader, bodyWriter := io.Pipe()
			go func() {
				defer bodyWriter.Close()
				time.Sleep(keepAliveDelay)
				_, _ = io.WriteString(bodyWriter, ": keep-alive\n\n")
				time.Sleep(eventDelay)
				_, _ = io.WriteString(bodyWriter, "event: message\n\n")
				time.Sleep(dataDelay)
				_, _ = io.WriteString(bodyWriter, `data: {"id":"chatcmpl-1","choices":[]}`+"\n\n")
				_, _ = io.WriteString(bodyWriter, "data: [DONE]\n\n")
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       bodyReader,
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"time_to_first_content": headerDelay + keepAliveDelay + eventDelay + dataDelay,
			},
		}
	})

	tests.AddFunc("should log the time from sending a request upstream to its response header", func(t *testing.T) test {
		// Distinct delays, so each logged value identifies whether the
		// header or the first content produced it.
		const (
			headerDelay = 1 * time.Second
			dataDelay   = 2 * time.Second
		)
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(headerDelay)
			bodyReader, bodyWriter := io.Pipe()
			go func() {
				defer bodyWriter.Close()
				time.Sleep(dataDelay)
				_, _ = io.WriteString(bodyWriter, `data: {"id":"chatcmpl-1","choices":[]}`+"\n\n")
				_, _ = io.WriteString(bodyWriter, "data: [DONE]\n\n")
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       bodyReader,
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"time_to_response_header": headerDelay,
				"time_to_first_content":   headerDelay + dataDelay,
			},
		}
	})

	tests.AddFunc("should log zero times to response header and first content for a request no attempt served", func(t *testing.T) test {
		// The failure follows a delay, so any time recorded for the failed
		// attempt is nonzero and fails the zero expectation.
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(2 * time.Second)
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid api key"}}`)),
				Header:     http.Header{},
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"error":                   "upstream returned status 401: invalid api key",
				"time_to_response_header": time.Duration(0),
				"time_to_first_content":   time.Duration(0),
			},
		}
	})

	tests.AddFunc("should take a data: line of any length as first content", func(t *testing.T) test {
		// The later [DONE] event has its own delay, so a detector that missed
		// the long line and fired on [DONE] reports a later time.
		const (
			headerDelay = 1 * time.Second
			dataDelay   = 2 * time.Second
			doneDelay   = 3 * time.Second
		)
		// Far longer than any read, so the line spans many of them.
		line := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"` + strings.Repeat("x", 1<<20) + `"}}]}` + "\n\n"
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(headerDelay)
			bodyReader, bodyWriter := io.Pipe()
			go func() {
				defer bodyWriter.Close()
				time.Sleep(dataDelay)
				_, _ = io.WriteString(bodyWriter, line)
				time.Sleep(doneDelay)
				_, _ = io.WriteString(bodyWriter, "data: [DONE]\n\n")
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       bodyReader,
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"time_to_first_content": headerDelay + dataDelay,
			},
		}
	})

	tests.AddFunc("should log the serving attempt's time to first content for a failed-over request", func(t *testing.T) test {
		// Distinct delays so the logged time identifies the clock's start:
		// 1s if timed from the serving attempt's send, 6s from the abandoned one's.
		const (
			failDelay  = 5 * time.Second
			serveDelay = 1 * time.Second
		)
		rateLimited := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(failDelay)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
				Header:     http.Header{"Retry-After": {"60"}},
			}, nil
		})
		serves := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(serveDelay)
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
				"backend_name":          "b",
				"time_to_first_content": serveDelay,
			},
		}
	})

	tests.AddFunc("should log the first data: event's time when an event: line arrives split across reads", func(t *testing.T) test {
		// The delays differ so a detector that reads the name's second
		// half as content reports 6s, not 10s.
		const (
			headerDelay = 1 * time.Second
			headDelay   = 2 * time.Second
			tailDelay   = 3 * time.Second
			dataDelay   = 4 * time.Second
		)
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(headerDelay)
			bodyReader, bodyWriter := io.Pipe()
			go func() {
				defer bodyWriter.Close()
				time.Sleep(headDelay)
				_, _ = io.WriteString(bodyWriter, "eve")
				time.Sleep(tailDelay)
				_, _ = io.WriteString(bodyWriter, "nt: message\n\n")
				time.Sleep(dataDelay)
				_, _ = io.WriteString(bodyWriter, `data: {"id":"chatcmpl-1","choices":[]}`+"\n\n")
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       bodyReader,
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"time_to_first_content": headerDelay + headDelay + tailDelay + dataDelay,
			},
		}
	})

	tests.AddFunc("should log a zero time to first content for a stream of only keep-alive comments", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(1 * time.Second)
			bodyReader, bodyWriter := io.Pipe()
			go func() {
				defer bodyWriter.Close()
				time.Sleep(2 * time.Second)
				_, _ = io.WriteString(bodyWriter, ": keep-alive\n\n")
				time.Sleep(3 * time.Second)
				_, _ = io.WriteString(bodyWriter, ": keep-alive\n\n")
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       bodyReader,
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"time_to_first_content": time.Duration(0),
			},
		}
	})

	tests.AddFunc("should log a non-streamed reply's time to first content at its first non-whitespace byte", func(t *testing.T) test {
		// Distinct delays so the logged time shows which byte stopped the
		// clock: 6s at the '{', 3s had it stopped at the leading whitespace.
		const (
			headerDelay     = 1 * time.Second
			whitespaceDelay = 2 * time.Second
			jsonDelay       = 3 * time.Second
		)
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(headerDelay)
			bodyReader, bodyWriter := io.Pipe()
			go func() {
				defer bodyWriter.Close()
				time.Sleep(whitespaceDelay)
				_, _ = io.WriteString(bodyWriter, "\n \t\r\n")
				time.Sleep(jsonDelay)
				_, _ = io.WriteString(bodyWriter, `{"id":"chatcmpl-1","choices":[]}`)
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       bodyReader,
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"time_to_first_content": headerDelay + whitespaceDelay + jsonDelay,
			},
		}
	})

	tests.AddFunc("should log the first data: event's time when later data: events follow", func(t *testing.T) test {
		// Distinct delays so the logged time identifies which event set it:
		// first data: event gives 3s; a later event moving it would give 6s.
		const (
			headerDelay    = 1 * time.Second
			firstDataDelay = 2 * time.Second
			laterDataDelay = 3 * time.Second
		)
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			time.Sleep(headerDelay)
			bodyReader, bodyWriter := io.Pipe()
			go func() {
				defer bodyWriter.Close()
				time.Sleep(firstDataDelay)
				_, _ = io.WriteString(bodyWriter, `data: {"id":"chatcmpl-1","choices":[]}`+"\n\n")
				time.Sleep(laterDataDelay)
				_, _ = io.WriteString(bodyWriter, `data: {"id":"chatcmpl-1","choices":[]}`+"\n\n")
				_, _ = io.WriteString(bodyWriter, "data: [DONE]\n\n")
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       bodyReader,
			}, nil
		})
		return test{
			validator: &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
			}},
			requestBody: `{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{
				"time_to_first_content": headerDelay + firstDataDelay,
			},
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			srv := New(tt.validator, append([]Option{WithLogger(testLogger(t))}, tt.opts...)...)

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
				"error":                   logRecordErrString(rec.Err),
				"backend_name":            rec.BackendName,
				"model_requested":         rec.ModelRequested,
				"model_upstream":          rec.ModelUpstream,
				"model_responded":         rec.ModelResponded,
				"upstream_status":         float64(rec.UpstreamStatus),
				"excluded":                rec.Excluded,
				"time_to_response_header": rec.TimeToResponseHeader,
				"time_to_first_content":   rec.TimeToFirstContent,
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
	// throttling answers 429 with a Retry-After, so a case reads as the return each
	// candidate offered rather than as four near-identical response literals.
	throttling := func(retryAfter, message string) func(*http.Request, int) (*http.Response, error) {
		return func(*http.Request, int) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"type":"rate_limit_error","message":%q}}`, message))),
				Header:     http.Header{"Retry-After": {retryAfter}},
			}, nil
		}
	}
	// notDueUntil is the record a walk leaves for a target it stepped over: the moment
	// it is due back, d after synctest's epoch, rendered as pickTarget renders it.
	notDueUntil := func(d time.Duration, answered string) error {
		return fmt.Errorf("routed around: not due until %s, last answered: %s",
			time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(d).Local().Format(time.RFC3339), answered)
	}

	// loggedAnswer is the part of a LogRecord that names the candidate a request
	// answered from, so a case can assert it without the walk's Excluded list.
	type loggedAnswer struct {
		Backend       string
		ModelUpstream string
		// UpstreamStatus is a pointer so a case can assert that none was
		// recorded: nil asserts nothing, and &0 asserts the absence itself,
		// which a bare 0 cannot say — assertedFields reads that as unasserted.
		UpstreamStatus *int
		Err            error
	}
	type step struct {
		// model is what the request names; empty means the logical model "m".
		model string
		// messages overrides the body's messages array; empty is the single user
		// message a first turn sends, so only a later turn names one.
		messages string
		// token overrides the bearer the step presents; empty is the user-token
		// every other step sends, so a case can speak as a second caller.
		token        string
		advance      time.Duration
		wantStatus   int
		wantBody     string
		wantBackend  string
		wantExcluded []Attempt
		// wantLogged is the log record's own account of the candidate the request
		// answered from, asserted field by field wherever a case sets one.
		wantLogged loggedAnswer
		// wantEnvelope is the error response's asserted aspects; a field left zero
		// is one this case's behavior does not name.
		wantEnvelope errorEnvelope
		// wantRetryAfter is the backoff the client is handed; empty asserts that no
		// backoff was sent.
		wantRetryAfter string
	}
	// backendStub is one backend's real upstream identity — base URL and API key —
	// plus its stubbed round-trip: given the request and the 1-based call count, it
	// returns the upstream response, or blocks (a stall) until the request context
	// is cancelled.
	type backendStub struct {
		// baseURL nil is how a case declares a backend understudy cannot use, however
		// healthy its sibling targets are.
		baseURL *url.URL
		apiKey  string
		resp    func(r *http.Request, call int) (*http.Response, error)
	}
	type test struct {
		// ctx overrides the request context (defaults to t.Context()); set it to
		// exercise a client that goes away mid-walk.
		ctx      context.Context
		backends map[string]backendStub
		targets  []Target
		steps    []step
	}

	tests := testy.NewTable[test]()

	tests.Add("should fail over to the next target after the threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`},
		},
	})

	tests.Add("should keep a target in the walk after a 429 with no Retry-After", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
		},
	})

	tests.Add("should serve a conversation whose recorded target is benched", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call <= 2 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(r *http.Request, call int) (*http.Response, error) {
				// A second conversation benches b, leaving the first recorded
				// against it.
				if call == 2 {
					return throttling("120", "slow down")(r, call)
				}
				return resp(http.StatusOK, `{"id":"from-b"}`), nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				messages:   `[{"role":"user","content":"a different conversation"}]`,
				wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantBackend: "b", wantRetryAfter: "5",
			},
			{
				advance:    time.Second,
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a",
			},
		},
	})

	tests.Add("should follow a conversation to the target that takes over when its own fails", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 2 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-b"}`), nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance:    time.Second,
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a",
				wantExcluded: []Attempt{{Backend: "b", ModelUpstream: "mb", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			// Still a, though b answers again and is first in the list.
			{
				advance:    time.Second,
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"},{"role":"assistant","content":"more"},{"role":"user","content":"go on"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a",
			},
		},
	})

	tests.Add("should serve a conversation's next turn from the target that served its first", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				// A momentary throttle: by the second turn a would serve.
				if call == 1 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			// Second turn: b serves again, though a is healthy and first in the list.
			{
				advance:    time.Second,
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
			},
		},
	})

	tests.Add("should serve a conversation's later request from the target that served it last, after the conversation has been quiet for hours", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				// A momentary throttle: by the second turn a would serve.
				if call == 1 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance:    3 * time.Hour,
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
			},
		},
	})

	tests.Add("should serve a conversation's later request from the first target in the list, after the conversation has gone more than a day with no request", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance:    sessionForgetAfter + time.Second,
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a",
			},
		},
	})

	tests.Add("should not share conversation affinity between two tokens", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				// A momentary throttle: by the second turn a would serve.
				if call == 1 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			// First turn, as caller A: a is throttled, so b serves and A's
			// conversation learns b.
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			// Second turn with the same leading messages, but as caller B: B's
			// conversation has learned nothing, so the walk starts at a rather
			// than inheriting A's affinity.
			{
				advance:    time.Second,
				token:      "other-token",
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a",
			},
		},
	})

	tests.Add("should route around a target whose 429s outlast the failover threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", Err: notDueUntil(45*time.Second, "upstream returned status 429: slow down")}},
			},
		},
	})

	tests.Add("should route a logical model around an account a directly-named reference demoted", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantBackend: "a", wantRetryAfter: "5"},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should reject a directly-named reference on a streak a logical model accrued past the terminal threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{
				model:        "a/ma",
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamUnavailable}},
			},
		},
	})

	// A recovering "a" is what makes these cases discriminate: were the account
	// merely accruing a streak, it would be re-probed and serve, so an answer from
	// "b" is proof understudy did not call it.
	recovering := func(fail func(*http.Request, int) (*http.Response, error)) func(*http.Request, int) (*http.Response, error) {
		return func(r *http.Request, call int) (*http.Response, error) {
			if call > 1 {
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}
			return fail(r, call)
		}
	}

	tests.Add("should route a logical model around an account a directly-named reference benched, until that time elapses", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: recovering(throttling("60", "slow down"))},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantBackend: "a", wantRetryAfter: "60"},
			{advance: 46 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{advance: 15 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a"},
		},
	})

	tests.Add("should route a logical model around an account a directly-named reference was refused by, with no interval elapsing", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: recovering(always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`))},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				model:        "a/ma",
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamRefused}},
				wantBackend:  "a",
			},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.AddFunc("should answer for the client that went away, not the target it was walking to", func(t *testing.T) test {
		ctx, cancel := context.WithCancel(t.Context())
		return test{
			ctx: ctx,
			backends: map[string]backendStub{
				"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("60", "slow down")},
				"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(r *http.Request, _ int) (*http.Response, error) {
					cancel()
					return nil, context.Cause(r.Context())
				}},
			},
			targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
			steps: []step{
				{advance: 0, wantStatus: statusClientClosedRequest},
			},
		}
	})

	// A status-bearing cause is how a consumer renders e.g. shutdown, with no
	// shutdown concept inside understudy.
	tests.AddFunc("should surface the host's own cancellation cause raised mid-walk", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		return test{
			ctx: ctx,
			backends: map[string]backendStub{
				"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("60", "slow down")},
				"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(r *http.Request, _ int) (*http.Response, error) {
					cancel(yerrors.WithHTTPStatus(http.StatusServiceUnavailable, errors.New("lindyd: shutting down")))
					return nil, context.Cause(r.Context())
				}},
			},
			targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
			steps: []step{
				{advance: 0, wantStatus: http.StatusServiceUnavailable},
			},
		}
	})

	tests.Add("should answer a refusal itself when the only candidate left is unusable", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
			"b": {apiKey: "sk-b"},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				advance:      0,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamRefused}},
				wantBackend:  "a",
			},
		},
	})

	// TODO(TODO.d/honor-an-upstream-backoff-with-nothing-left.md): the case below
	// pins the fallback serving a target before the moment its upstream named.

	tests.Add("should serve from a benched candidate rather than answer for an unusable one that sorts after it", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: recovering(throttling("60", "slow down"))},
			"b": {apiKey: "sk-b"},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantRetryAfter: "60",
				advance:        0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantBackend: "a",
				wantExcluded: []Attempt{{Backend: "b", ModelUpstream: "mb", Err: errors.New("must provide base_url")}},
			},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a"},
		},
	})

	tests.Add("should reject once a target has been failing past the terminal threshold, though an unusable candidate is still untried", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
			"b": {apiKey: "sk-b"},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamUnavailable}},
			},
		},
	})

	tests.Add("should answer a lone busy target like any sustained rate limit, with a backoff to wait out", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"server busy","code":"unavailable"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: `{"error":{"message":"upstream returned status 503: server busy","type":"server_error"}}`, wantRetryAfter: "30"},
		},
	})

	tests.Add("should fail over within the request when a target reports itself busy", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"server busy","code":"unavailable"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should show the client 502 for an upstream 5xx", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
		},
	})

	tests.Add("should carry the upstream's own words for why a target was walked past", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"quota gemini-2.5-flash exhausted"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
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

	tests.Add("should name a benched target the request routed around", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("50", "slow down")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBackend: "b"},
			{advance: time.Second, wantStatus: http.StatusOK, wantBackend: "b", wantExcluded: []Attempt{{
				Backend:       "a",
				ModelUpstream: "ma",
				Err:           notDueUntil(50*time.Second, "upstream returned status 429: slow down"),
			}}},
		},
	})

	tests.Add("should show the status the walked-past target answered with", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBackend: "b", wantExcluded: []Attempt{
				{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true},
			}},
		},
	})

	tests.Add("should bench a sibling backend sharing an account and model when one is demoted", test{
		backends: map[string]backendStub{
			"acct-a": {baseURL: mustParseURL(t, "http://shared/v1"), apiKey: "sk-shared", resp: throttling("120", "slow down")},
			"acct-b": {baseURL: mustParseURL(t, "http://shared/v1"), apiKey: "sk-shared", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"acct-c": {baseURL: mustParseURL(t, "http://other/v1"), apiKey: "sk-other", resp: always(http.StatusOK, `{"id":"from-c"}`)},
		},
		targets: []Target{
			{backend: "acct-a", model: "glm"},
			{backend: "acct-b", model: "glm"},
			{backend: "acct-c", model: "glm"},
		},
		steps: []step{
			// acct-b is the same account+model as the demoted acct-a, so it is
			// benched too: failover routes to the different account acct-c.
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-c"}`, wantBackend: "acct-c"},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-c"}`, wantBackend: "acct-c"},
		},
	})

	tests.Add("should not demote a target on a 429 whose Retry-After is within the threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"10"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			// Still inside a's 10s Retry-After window, yet a leads the walk again:
			// the transient 429 cost it its turn in each request, not its place.
			{
				advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
		},
	})

	tests.Add("should not fail over on a non-fatal error", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusBadRequest, `{"error":{"message":"bad request"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadRequest, wantBody: `{"error":{"message":"upstream returned status 400: bad request","type":"invalid_request_error"}}`, wantBackend: "a"},
			{advance: time.Second, wantStatus: http.StatusBadRequest, wantBody: `{"error":{"message":"upstream returned status 400: bad request","type":"invalid_request_error"}}`, wantBackend: "a"},
		},
	})

	// reasoningErrorMessage is DeepSeek's verdict on a history whose assistant turns
	// carry no reasoning_content — the prose the walk matches on.
	const reasoningErrorMessage = `The reasoning_content in the thinking mode must be passed back to the API`

	tests.Add("should serve from the next target when one rejects the request's history", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":%q}}`, reasoningErrorMessage))},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusBadRequest, Err: fmt.Errorf("upstream returned status 400: %s", reasoningErrorMessage), Called: true}},
			},
			// The rejection cost a its turn, not its health: it still leads the
			// walk and is called again, still leaving b to serve.
			{
				advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusBadRequest, Err: fmt.Errorf("upstream returned status 400: %s", reasoningErrorMessage), Called: true}},
			},
		},
	})

	// unrecognized400 is arbitrary wording — the walk matches no prose here.
	const unrecognized400 = `cannot serve these messages`

	tests.Add("should serve from the next target when one answers a 400 to a conversation another target served last", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					// A bare 429, so the conversation's first request moves to
					// b and the record names b, without benching a.
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":%q}}`, unrecognized400)), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusOK, `{"id":"from-b"}`), nil
				}
				// A bare 429 again, so the conversation's next request leaves b
				// down the walk rather than waiting at it.
				return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
			}},
			"c": {baseURL: mustParseURL(t, "http://c/v1"), apiKey: "sk-c", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 2 {
					return resp(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":%q}}`, unrecognized400)), nil
				}
				return resp(http.StatusOK, `{"id":"from-c"}`), nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}, {backend: "c", model: "mc"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance:    time.Second,
				messages:   `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus: http.StatusOK, wantBody: `{"id":"from-c"}`, wantBackend: "c",
				wantExcluded: []Attempt{
					{Backend: "b", ModelUpstream: "mb", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true},
					{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusBadRequest, Err: fmt.Errorf("upstream returned status 400: %s", unrecognized400), Called: true},
				},
			},
			// c served the conversation last, so its own 400 is the client's —
			// the failover above reads the record, not the status alone.
			{
				advance:     time.Second,
				messages:    `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"},{"role":"assistant","content":"more"},{"role":"user","content":"go on"}]`,
				wantStatus:  http.StatusBadRequest,
				wantBody:    fmt.Sprintf(`{"error":{"message":"upstream returned status 400: %s","type":"invalid_request_error"}}`, unrecognized400),
				wantBackend: "c",
			},
		},
	})

	tests.Add("should relay a 400 to the client when it answers the first request of a conversation another target served last", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":%q}}`, unrecognized400)), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance:     time.Second,
				wantStatus:  http.StatusBadRequest,
				wantBody:    fmt.Sprintf(`{"error":{"message":"upstream returned status 400: %s","type":"invalid_request_error"}}`, unrecognized400),
				wantBackend: "a",
			},
		},
	})

	tests.Add("should relay a 400 to the client when it answers a conversation another target served more than a day before", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`), nil
				}
				return resp(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":%q}}`, unrecognized400)), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			{
				advance:     sessionForgetAfter + time.Second,
				messages:    `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"and then?"}]`,
				wantStatus:  http.StatusBadRequest,
				wantBody:    fmt.Sprintf(`{"error":{"message":"upstream returned status 400: %s","type":"invalid_request_error"}}`, unrecognized400),
				wantBackend: "a",
			},
		},
	})

	tests.Add("should route to the last target when all are failing", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBackend: "a", wantRetryAfter: "5"},
			{advance: 16 * time.Second, wantStatus: http.StatusBadGateway, wantBackend: "b", wantRetryAfter: "5"},
			{advance: 16 * time.Second, wantStatus: http.StatusBadGateway, wantBackend: "b", wantRetryAfter: "20"},
		},
	})

	tests.Add("should reject as non-retryable once a target has been failing past the terminal threshold with nowhere to fail over", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamUnavailable}},
			},
		},
	})

	tests.Add("should hand a client understudy's own backoff when an upstream 5xx carries none", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{advance: 5 * time.Second, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "10"},
		},
	})

	tests.Add("should tell a client the same wait every time a target answers busy", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"server busy","code":"unavailable"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantRetryAfter: "30"},
			{advance: 5 * time.Second, wantStatus: http.StatusTooManyRequests, wantRetryAfter: "30"},
			{advance: 10 * time.Second, wantStatus: http.StatusTooManyRequests, wantRetryAfter: "30"},
		},
	})

	tests.Add("should not let a quick success mid-bench clear a busy streak old enough to reject terminally", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 2 {
					return resp(http.StatusOK, `{"id":"from-a"}`), nil
				}
				return resp(http.StatusServiceUnavailable, `{"error":{"message":"server busy","code":"unavailable"}}`), nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantRetryAfter: "30"},
			// Still inside the 30s bench, so the streak survives the success
			// (recordSuccess's bench-preserving branch, not its delete branch) —
			// the target hasn't proven it's actually back, only answered once.
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`},
			// Failing again long after: the streak's start never reset, so it is
			// now well past the terminal threshold.
			{
				advance:    2*time.Minute + time.Second,
				wantStatus: http.StatusBadRequest,
				wantEnvelope: errorEnvelope{
					Error:        errorDetail{Type: errTypeUpstreamRateLimited},
					RetryAfterMS: int(rateLimitDemotionThreshold.Milliseconds()),
				},
			},
		},
	})

	tests.Add("should reject as non-retryable once a target has been busy past the terminal threshold with nowhere to fail over", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"server busy","code":"unavailable"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantRetryAfter: "30"},
			{
				advance:    2*time.Minute + time.Second,
				wantStatus: http.StatusBadRequest,
				wantEnvelope: errorEnvelope{
					Error:        errorDetail{Type: errTypeUpstreamRateLimited},
					RetryAfterMS: int(rateLimitDemotionThreshold.Milliseconds()),
				},
			},
		},
	})

	tests.Add("should start a fresh streak once a demotion has gone untouched past the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{
				wantRetryAfter: "5",
				advance:        24*time.Hour + time.Second,
				wantStatus:     http.StatusBadGateway,
				wantBody:       badGateway502,
				wantEnvelope:   errorEnvelope{Error: errorDetail{Type: errTypeServer}},
			},
		},
	})

	tests.Add("should hold a demotion open while a directly-named reference keeps failing across the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
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

	// TODO(TODO.d/understudy-error-envelope-type.md): the refusal path takes the same
	// sweep and re-stamp, and still no case drives it across the window, because a
	// refusal's answer does not vary with age: writeRefusal renders before any streak
	// is consulted, so 0h, 23h and 48h all give 400 upstream_refused. Whether that
	// should change is the open question — only an operator clears a refusal, so a
	// streak may have nothing to say about one.

	tests.Add("should start a fresh streak once a directly-named reference's demotion has gone untouched past the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{
				model:          "a/ma",
				advance:        24*time.Hour + time.Second,
				wantStatus:     http.StatusBadGateway,
				wantBody:       badGateway502,
				wantRetryAfter: "5",
				wantEnvelope:   errorEnvelope{Error: errorDetail{Type: errTypeServer}},
			},
		},
	})

	tests.Add("should start a fresh streak once a rate-limited reference's demotion has gone untouched past the eviction window", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{model: "a/ma", wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantRetryAfter: "60"},
			{
				wantRetryAfter: "60",
				model:          "a/ma",
				advance:        24*time.Hour + time.Second,
				wantStatus:     http.StatusTooManyRequests,
				wantBody:       rateLimit429,
			},
		},
	})

	tests.Add("should not invent a delay for a target no retry can help, however long it has failed", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusNotImplemented, `{"error":{"message":"not implemented"}}`)},
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

	tests.Add("should carry the streak's own backoff into a terminal reject the upstream left unbounded", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantRetryAfter: "5"},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{RetryAfterMS: 80000},
			},
		},
	})

	tests.Add("should fail over to an untried alternate even when the target probed is long-dead", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantBackend: "a", wantRetryAfter: "5"},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{
				advance:      2*time.Minute + time.Second,
				wantStatus:   http.StatusOK,
				wantBody:     `{"id":"from-b"}`,
				wantBackend:  "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusServiceUnavailable, Err: errors.New("upstream returned status 503: upstream busy"), Called: true}},
			},
		},
	})

	tests.AddFunc("should surface the client's own disconnect rather than fail over a demoted target's probe", func(t *testing.T) test {
		ctx, cancel := context.WithCancel(t.Context())
		return test{
			ctx: ctx,
			backends: map[string]backendStub{
				"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(r *http.Request, call int) (*http.Response, error) {
					if call == 1 {
						return resp(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`), nil
					}
					cancel()
					return nil, context.Cause(r.Context())
				}},
				"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
			},
			targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
			steps: []step{
				{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantBackend: "a", wantRetryAfter: "5"},
				{advance: 46 * time.Second, wantStatus: statusClientClosedRequest, wantBackend: "a"},
			},
		}
	})

	// TODO: should surface a non-fatal error (e.g. a 400) from a demoted
	// target's half-open probe rather than fail over — this replay is scoped
	// to isFatalUpstream(err) and should stay that way. Case: a always answers
	// 400, b healthy; advance past failoverThreshold+recoveryInterval so a is
	// probed; assert the response still comes from a with b untried.

	tests.Add("should send the upstream's own backoff when the reject is terminal", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
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
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{advance: 5 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`},
		},
	})

	tests.Add("should re-probe and restore a demoted target after the recovery interval", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`), nil
				}
				return resp(http.StatusOK, `{"id":"from-a"}`), nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`},
			{advance: 30 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`},
		},
	})

	tests.Add("should preserve the streak start across repeated failures", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5"},
			{advance: 5 * time.Second, wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "10"},
			{advance: 11 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`},
		},
	})

	tests.Add("should fail over within the request when a target answers with a 429 whose Retry-After is long enough to demote it", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should fail over within the request when a target answers with a 429 whose Retry-After is too short to demote it", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("5", "slow down")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should answer the client with a 429 when every target answers with a 429 whose Retry-After is too short to demote it", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("5", "slow down")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: throttling("5", "slow down")},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantRetryAfter: "5", wantBackend: "b"},
		},
	})
	tests.Add("should give the client the soonest Retry-After when every target answers with a 429 whose Retry-After is too short to demote it", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("3", "back in three")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: throttling("5", "back in five")},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: `{"error":{"message":"upstream returned status 429: back in three","type":"rate_limit_error"}}`, wantRetryAfter: "3", wantBackend: "b"},
		},
	})

	tests.Add("should fail over within the request when a target's credential is out of funds", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusPaymentRequired, `{"error":{"message":"Insufficient Balance"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should fail over within the request when a target's credential is rejected", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should tell a client a refused request is terminal without repeating what the upstream said", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
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

	tests.Add("should answer with an earlier candidate's throttle rather than the refusal that ended the walk", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantRetryAfter: "60"},
		},
	})

	tests.Add("should tell a client when a throttled target returns, not that a later one refused", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				advance:        0,
				wantStatus:     http.StatusTooManyRequests,
				wantBody:       rateLimit429,
				wantBackend:    "b",
				wantRetryAfter: "5",
				wantExcluded: []Attempt{
					{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true},
					{Backend: "b", ModelUpstream: "mb", UpstreamStatus: http.StatusUnauthorized, Err: errors.New("upstream returned status 401: invalid api key"), Called: true},
				},
			},
		},
	})

	tests.Add("should record the refused target a request did not serve from when an earlier throttle answers for it", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantRetryAfter: "60", wantExcluded: []Attempt{
				{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true},
				{Backend: "b", ModelUpstream: "mb", UpstreamStatus: http.StatusUnauthorized, Err: errors.New("upstream returned status 401: invalid api key"), Called: true},
			}},
		},
	})

	tests.Add("should keep answering with an earlier candidate's throttle once the target that refused has been failing past the terminal threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 2 {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-a"}`)), Header: http.Header{}}, nil
				}
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantRetryAfter: "60"},
			{advance: 61 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-a"}`, wantBackend: "a"},
			{advance: 61 * time.Second, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantRetryAfter: "60"},
		},
	})

	tests.Add("should answer with the soonest throttle among the candidates it walked past", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("1800", "back in half an hour")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: throttling("60", "back in a minute")},
			"c": {baseURL: mustParseURL(t, "http://c/v1"), apiKey: "sk-c", resp: throttling("900", "back in fifteen minutes")},
			"d": {baseURL: mustParseURL(t, "http://d/v1"), apiKey: "sk-d", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}, {backend: "c", model: "mc"}, {backend: "d", model: "md"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: `{"error":{"message":"upstream returned status 429: back in a minute","type":"rate_limit_error"}}`, wantRetryAfter: "60"},
		},
	})

	tests.Add("should answer with an earlier candidate's throttle rather than a failure no retry can help", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("60", "back in a minute")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusNotImplemented, `{"error":{"message":"not implemented"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: `{"error":{"message":"upstream returned status 429: back in a minute","type":"rate_limit_error"}}`, wantRetryAfter: "60"},
		},
	})

	tests.Add("should weigh what remains of each throttle, not what each first named", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("40", "back in forty")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(r *http.Request, call int) (*http.Response, error) {
				// Long enough that a's 40s has 25s left when b's 30s is weighed against it.
				time.Sleep(15 * time.Second)
				return throttling("30", "back in thirty")(r, call)
			}},
			"c": {baseURL: mustParseURL(t, "http://c/v1"), apiKey: "sk-c", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}, {backend: "c", model: "mc"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: `{"error":{"message":"upstream returned status 429: back in forty","type":"rate_limit_error"}}`, wantRetryAfter: "25"},
		},
	})

	tests.Add("should give the client an earlier target's Retry-After when it is sooner than the wait understudy gives for the last target's server error", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: throttling("3", "back in three")},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: `{"error":{"message":"upstream returned status 429: back in three","type":"rate_limit_error"}}`, wantRetryAfter: "3"},
		},
	})

	tests.Add("should give the client the wait understudy gives an earlier target's 429 without a Retry-After when it is sooner than the last target's Retry-After", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: throttling("60", "back in a minute")},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantRetryAfter: "5"},
		},
	})
	tests.Add("should give the client the wait understudy gives the last target's server error when it is sooner than the wait it gives an earlier target's 429 without a Retry-After", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(_ *http.Request, call int) (*http.Response, error) {
				if call == 1 {
					return resp(http.StatusOK, `{"id":"from-b"}`), nil
				}
				return resp(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`), nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{
				// a has been failing long enough that understudy's wait for it has
				// grown; b's failure is its first.
				advance:    5 * time.Second,
				messages:   `[{"role":"user","content":"a different conversation"}]`,
				wantStatus: http.StatusBadGateway, wantBody: badGateway502, wantRetryAfter: "5",
			},
		},
	})
	tests.Add("should give the client the last target's Retry-After when it is sooner than the wait understudy gives an earlier target's 429 without a Retry-After", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: throttling("2", "back in two")},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: `{"error":{"message":"upstream returned status 429: back in two","type":"rate_limit_error"}}`, wantRetryAfter: "2"},
		},
	})

	// TODO(TODO.d/weigh-every-candidates-contribution.md): the rows that entry leaves
	// unbuilt — a bench, a stall — belong beside these once there is something to
	// weigh them against.

	tests.Add("should fail over within the request when a target forbids the request", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusForbidden, `{"error":{"message":"only available hosted in China and requires explicit opt in"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
		},
	})

	tests.Add("should not call again, on the next request, a target that refused the account", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: recovering(always(http.StatusForbidden, `{"error":{"message":"only available hosted in China and requires explicit opt in"}}`))},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b"},
			{advance: time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b", wantExcluded: []Attempt{{
				Backend:       "a",
				ModelUpstream: "ma",
				// A refusal names no moment, so the wait is one recovery interval.
				Err: notDueUntil(30*time.Second, "upstream returned status 403: only available hosted in China and requires explicit opt in"),
			}}},
		},
	})

	tests.Add("should fail over across requests when a recurring 429's Retry-After is below the demotion threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"10"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusOK, `{"id":"from-b"}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			// The transient 429 fails this request over to b without benching a.
			{
				advance: 0, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusTooManyRequests, Err: errors.New("upstream returned status 429: slow down"), Called: true}},
			},
			// The recurring streak is what benches a across requests: it now leads
			// nowhere, the walk goes straight to b without dialing a.
			{
				advance: 16 * time.Second, wantStatus: http.StatusOK, wantBody: `{"id":"from-b"}`, wantBackend: "b",
				wantExcluded: []Attempt{{Backend: "a", ModelUpstream: "ma", Err: notDueUntil(45*time.Second, "upstream returned status 429: slow down")}},
			},
		},
	})

	// The fallback echoes the body it received, so wantBody asserts the full
	// request payload (with the model rewritten for that target) survived the replay.
	tests.Add("should replay the full request body to the fallback target", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(r *http.Request, _ int) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusOK, wantBody: `{"model":"mb","messages":[{"role":"user","content":"hi"}]}`, wantBackend: "b"},
		},
	})

	tests.Add("should name the candidate a refused request answered from, with the ones it moved past on Excluded", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: always(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: always(http.StatusForbidden, `{"error":{"message":"not permitted"}}`)},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{
				advance:      0,
				wantStatus:   http.StatusBadRequest,
				wantEnvelope: errorEnvelope{Error: errorDetail{Type: errTypeUpstreamRefused}},
				wantExcluded: []Attempt{
					{Backend: "a", ModelUpstream: "ma", UpstreamStatus: http.StatusUnauthorized, Err: errors.New("upstream returned status 401: invalid api key"), Called: true},
				},
				wantLogged: loggedAnswer{
					Backend:        "b",
					ModelUpstream:  "mb",
					UpstreamStatus: new(http.StatusForbidden),
					Err:            errors.New("upstream returned status 403: not permitted"),
				},
			},
		},
	})

	tests.Add("should surface the 429 when every target is rate-limited past the threshold", test{
		backends: map[string]backendStub{
			"a": {baseURL: mustParseURL(t, "http://a/v1"), apiKey: "sk-a", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
			"b": {baseURL: mustParseURL(t, "http://b/v1"), apiKey: "sk-b", resp: func(*http.Request, int) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					Header:     http.Header{"Retry-After": {"60"}},
				}, nil
			}},
		},
		targets: []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}},
		steps: []step{
			{advance: 0, wantStatus: http.StatusTooManyRequests, wantBody: rateLimit429, wantRetryAfter: "60"},
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
				backends[name] = Backend{ProviderType: "openai", Config: providers.Config{BaseURL: bs.baseURL, APIKey: bs.apiKey, HTTPClient: client}}
			}
			validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: tt.targets}}}, nil
			}}
			srv := New(validator, WithLogger(testLogger(t))).server
			// These cases assert the interval exactly; scattering it is
			// TestGraduatedBackoff's subject.
			srv.jitterFactor = 0

			for i, s := range tt.steps {
				if s.advance > 0 {
					time.Sleep(s.advance)
					synctest.Wait()
				}
				body := fmt.Sprintf(`{"model":%q,"messages":%s}`, cmp.Or(s.model, "m"), cmp.Or(s.messages, `[{"role":"user","content":"hi"}]`))
				req, err := http.NewRequestWithContext(cmp.Or(tt.ctx, t.Context()), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+cmp.Or(s.token, "user-token"))
				req.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				ctx := WithLogCtx(req.Context())
				srv.ServeHTTP(rr, req.WithContext(ctx))
				if rr.Code != s.wantStatus {
					t.Errorf("step %d: status got %d want %d", i, rr.Code, s.wantStatus)
				}
				rec, _ := LogRecordFromContext(ctx)
				if d := gocmp.Diff(s.wantExcluded, rec.Excluded, errorText, cmpopts.EquateEmpty(), assertedFields); d != "" {
					t.Errorf("step %d abandoned attempts (-want +got):\n%s", i, d)
				}
				answered := loggedAnswer{Backend: rec.BackendName, ModelUpstream: rec.ModelUpstream, UpstreamStatus: new(rec.UpstreamStatus), Err: rec.Err}
				if d := gocmp.Diff(s.wantLogged, answered, errorText, assertedFields); d != "" {
					t.Errorf("step %d log record (-want +got):\n%s", i, d)
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
				if got := rr.Header().Get("Retry-After"); got != s.wantRetryAfter {
					t.Errorf("step %d Retry-After: got %q want %q", i, got, s.wantRetryAfter)
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

	always502 := func(int, context.CancelFunc) int { return http.StatusBadGateway }
	recoverOnProbe := func(call int, _ context.CancelFunc) int {
		if call == 2 {
			return http.StatusOK
		}
		return http.StatusBadGateway
	}
	recoverWithinGrace := func(call int, _ context.CancelFunc) int {
		if call >= 2 {
			return http.StatusOK
		}
		return http.StatusBadGateway
	}
	const badGateway = `{"error":{"message":"bad gateway"}}`
	bothTargets := []Target{{backend: "a", model: "ma"}, {backend: "b", model: "mb"}}

	type test struct {
		aStatus func(call int, clientLeaves context.CancelFunc) int
		// aBody is the error body a answers with.
		aBody      string
		retryAfter time.Duration
		// targets is the logical model's candidate list.
		targets []Target
		// advances is the request sequence: one entry per request, slept
		// before it fires (0 = immediate).
		advances []time.Duration
		// seed drives a health transition directly through the real
		// production methods before requests start — the Given for a case
		// about a transition already in place, not one built up by requests.
		seed func(srv *server, backends map[string]Backend)
		// dialA2 lets a2 actually be dialed (and succeed); by default a2 must
		// never be dialed, only reached as an untried alias.
		dialA2   bool
		wantDown int
		// downFields are additional fields the "backend down" records must carry.
		downFields map[string]any
		wantUp     int
		// upFields are additional fields the "backend up" records must carry.
		upFields map[string]any
	}

	logTime := func(d time.Duration) string {
		return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(d).Local().Format(time.RFC3339Nano)
	}

	tests := testy.NewTable[test]()

	tests.Add("should log a target down once when it crosses the failover threshold", test{
		aStatus:  always502,
		aBody:    badGateway,
		targets:  bothTargets,
		advances: []time.Duration{0, 16 * time.Second, time.Second},
		wantDown: 1,
		wantUp:   0,
	})
	tests.Add("should re-log a target down after it recovers and fails again", test{
		aStatus:  recoverOnProbe,
		aBody:    badGateway,
		targets:  bothTargets,
		advances: []time.Duration{0, 16 * time.Second, 30 * time.Second, time.Second, 16 * time.Second},
		wantDown: 2,
		wantUp:   1,
	})
	tests.Add("should log a target up when a demoted target recovers", test{
		aStatus:  recoverOnProbe,
		aBody:    badGateway,
		targets:  bothTargets,
		advances: []time.Duration{0, 16 * time.Second, 30 * time.Second},
		wantDown: 1,
		wantUp:   1,
	})
	tests.Add("should not log a target up that was never down", test{
		aBody:    badGateway,
		targets:  bothTargets,
		aStatus:  func(int, context.CancelFunc) int { return http.StatusOK },
		advances: []time.Duration{0, time.Second},
		wantDown: 0,
		wantUp:   0,
	})
	tests.Add("should log a target down that the departed client's walk discovered", test{
		aBody:   badGateway,
		targets: bothTargets,
		aStatus: func(call int, clientLeaves context.CancelFunc) int {
			if call == 1 {
				clientLeaves()
			}
			return http.StatusUnauthorized
		},
		advances:   []time.Duration{0, time.Second},
		wantDown:   1,
		downFields: map[string]any{"reason": "probe not yet due"},
		wantUp:     0,
	})
	tests.Add("should date a refused target's streak at the refusal", test{
		aBody:    badGateway,
		targets:  bothTargets,
		aStatus:  func(int, context.CancelFunc) int { return http.StatusUnauthorized },
		advances: []time.Duration{0, time.Second},
		wantDown: 1,
		downFields: map[string]any{
			"reason":        "probe not yet due",
			"failing_since": logTime(0),
		},
		wantUp: 0,
	})
	tests.Add("should announce a demotion the streak never reported", test{
		aBody:   badGateway,
		targets: bothTargets,
		aStatus: func(call int, _ context.CancelFunc) int {
			if call == 1 {
				return http.StatusTooManyRequests
			}
			return http.StatusUnauthorized
		},
		retryAfter: time.Second,
		advances:   []time.Duration{0, 2 * time.Second},
		wantDown:   1,
		downFields: map[string]any{"reason": "probe not yet due"},
		wantUp:     0,
	})
	tests.Add("should stay silent when a target is demoted twice in one streak", test{
		aBody:   badGateway,
		targets: bothTargets,
		aStatus: func(call int, _ context.CancelFunc) int {
			if call == 1 {
				return http.StatusUnauthorized
			}
			return http.StatusTooManyRequests
		},
		retryAfter: 50 * time.Second,
		advances:   []time.Duration{0, 31 * time.Second},
		wantDown:   1,
		wantUp:     0,
	})
	tests.Add("should report a refused target when it is refused, not when a walk later finds it", test{
		aBody:    badGateway,
		targets:  bothTargets,
		aStatus:  func(int, context.CancelFunc) int { return http.StatusUnauthorized },
		advances: []time.Duration{0},
		wantDown: 1,
		downFields: map[string]any{
			"reason":     "probe not yet due",
			"readmit_at": slogdiff.Absent(),
		},
		wantUp: 0,
	})
	tests.Add("should report a throttled target when it is throttled, not when a walk later finds it", test{
		aBody:      badGateway,
		targets:    bothTargets,
		aStatus:    func(int, context.CancelFunc) int { return http.StatusTooManyRequests },
		retryAfter: 50 * time.Second,
		advances:   []time.Duration{0},
		wantDown:   1,
		downFields: map[string]any{
			"reason":        "upstream retry-after",
			"failing_since": logTime(0),
			"readmit_at":    logTime(50 * time.Second),
		},
		wantUp: 0,
	})
	tests.Add("should log a transition the departed client discovered", test{
		aBody:   badGateway,
		targets: bothTargets,
		aStatus: func(call int, clientLeaves context.CancelFunc) int {
			if call < 2 {
				return http.StatusBadGateway
			}
			clientLeaves()
			return http.StatusOK
		},
		advances: []time.Duration{0, 16 * time.Second, 30 * time.Second},
		wantDown: 1,
		wantUp:   1,
	})
	tests.Add("should not log a transition when a target recovers within the failover threshold", test{
		aStatus:  recoverWithinGrace,
		aBody:    badGateway,
		targets:  bothTargets,
		advances: []time.Duration{0, 5 * time.Second},
		wantDown: 0,
		wantUp:   0,
	})
	tests.Add("should log a target up when a rate-limited target is re-admitted after its retry-after", test{
		aStatus: func(call int, _ context.CancelFunc) int {
			if call == 1 {
				return http.StatusTooManyRequests
			}
			return http.StatusOK
		},
		aBody:      badGateway,
		targets:    bothTargets,
		retryAfter: 50 * time.Second,
		advances:   []time.Duration{0, 10 * time.Second, 50 * time.Second},
		wantDown:   1,
		wantUp:     1,
	})
	tests.Add("should keep a rate-limited target benched when its re-admission probe fails", test{
		aStatus: func(call int, _ context.CancelFunc) int {
			if call == 1 {
				return http.StatusTooManyRequests
			}
			return http.StatusBadGateway
		},
		aBody:      badGateway,
		targets:    bothTargets,
		retryAfter: 50 * time.Second,
		advances:   []time.Duration{0, 10 * time.Second, 50 * time.Second},
		wantDown:   1,
		wantUp:     0,
	})
	// synctest's clock starts at midnight UTC 2000-01-01, so every moment below is
	// an offset from it; slog renders them local.
	tests.Add("should say understudy's own probe pacing holds a target back", test{
		aBody:    badGateway,
		targets:  bothTargets,
		aStatus:  always502,
		advances: []time.Duration{0, 16 * time.Second, time.Second},
		wantDown: 1,
		downFields: map[string]any{
			"reason":        "probe not yet due",
			"failing_since": logTime(0),
			// recordFailure seeds lastProbe at the demotion moment (t+15s), so the
			// first half-open probe waits a full recovery interval past it.
			"next_probe": logTime(45 * time.Second),
			"readmit_at": slogdiff.Absent(),
		},
		wantUp: 0,
	})
	tests.Add("should say an upstream's own backoff holds a target back", test{
		aBody:   badGateway,
		targets: bothTargets,
		aStatus: func(call int, _ context.CancelFunc) int {
			if call == 1 {
				return http.StatusTooManyRequests
			}
			return http.StatusOK
		},
		retryAfter: 50 * time.Second,
		advances:   []time.Duration{0, 10 * time.Second, 50 * time.Second},
		wantDown:   1,
		downFields: map[string]any{
			"reason":     "upstream retry-after",
			"readmit_at": logTime(50 * time.Second),
			"next_probe": slogdiff.Absent(),
		},
		wantUp: 1,
	})
	tests.Add("should say what a target answered when a walk discovers its streak", test{
		aBody:      badGateway,
		targets:    bothTargets,
		aStatus:    func(int, context.CancelFunc) int { return http.StatusTooManyRequests },
		retryAfter: time.Second,
		advances:   []time.Duration{0, 16 * time.Second},
		wantDown:   1,
		downFields: map[string]any{
			"reason":         "probe not yet due",
			"upstream_error": "upstream returned status 429: bad gateway",
		},
		wantUp: 0,
	})
	tests.Add("should say what a backend answered when it went down", test{
		aBody:    badGateway,
		targets:  bothTargets,
		aStatus:  func(int, context.CancelFunc) int { return http.StatusUnauthorized },
		advances: []time.Duration{0},
		wantDown: 1,
		downFields: map[string]any{
			"upstream_error": "upstream returned status 401: bad gateway",
		},
		wantUp: 0,
	})

	tests.Add("should announce a lone busy target down like any other sustained rate limit, even with nowhere to fail over", test{
		aStatus:  func(int, context.CancelFunc) int { return http.StatusServiceUnavailable },
		aBody:    `{"error":{"message":"server busy","code":"unavailable"}}`,
		targets:  []Target{{backend: "a", model: "ma"}},
		advances: []time.Duration{0, 16 * time.Second, 16 * time.Second},
		wantDown: 1,
		wantUp:   0,
	})

	tests.Add("should name the target that failed, not a sibling reached later through a different config name", test{
		targets: []Target{{backend: "a2", model: "shared"}, {backend: "b", model: "mb"}},
		seed: func(srv *server, backends map[string]Backend) {
			srv.recordFailure(Target{backend: "a", model: "shared"}, backends, errors.New("bad gateway"))
		},
		advances:   []time.Duration{16 * time.Second},
		wantDown:   1,
		downFields: map[string]any{"model": "shared"},
		wantUp:     0,
	})

	tests.Add("should log a backend up naming the same backend a paired backend down named, not the sibling a success arrived through", test{
		targets: []Target{{backend: "a2", model: "shared"}},
		seed: func(srv *server, backends map[string]Backend) {
			srv.recordImmediateFailure(context.Background(), Target{backend: "a", model: "shared"}, backends, errors.New("bad gateway"))
		},
		advances:   []time.Duration{0},
		dialA2:     true,
		wantDown:   1,
		downFields: map[string]any{"model": "shared"},
		wantUp:     1,
		upFields:   map[string]any{"model": "shared"},
	})

	tests.Add("should name the paired backend down's backend when a mid-bench success is later followed by one that ends the streak", test{
		targets: []Target{{backend: "a2", model: "shared"}},
		seed: func(srv *server, backends map[string]Backend) {
			srv.recordRateLimited(context.Background(), Target{backend: "a", model: "shared"}, 30*time.Second, backends, errors.New("bad gateway"))
		},
		// The first success lands mid-bench (readmitAt at t=30s) and must not
		// end the streak; the second lands after it elapses and does.
		advances:   []time.Duration{15 * time.Second, 30 * time.Second},
		dialA2:     true,
		wantDown:   1,
		downFields: map[string]any{"model": "shared"},
		wantUp:     1,
		upFields:   map[string]any{"model": "shared"},
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(droppingHandler{slog.NewJSONHandler(&logBuf, nil)})

			ctx, clientGone := context.WithCancel(t.Context())
			callsA := 0
			clientA := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
				callsA++
				status := tt.aStatus(callsA, clientGone)
				body := tt.aBody
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
			// a2 shares a's account (base URL + key): a second config name
			// reaching the same health entry. Must never be dialed unless
			// dialA2 lets it actually serve.
			clientA2 := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
				if !tt.dialA2 {
					t.Fatal("a2 dialed: it should only ever be reached as an untried alias of a's account")
					return nil, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"from-a2"}`)), Header: make(http.Header)}, nil
			})
			backends := map[string]Backend{
				"a":  {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: clientA}},
				"a2": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: clientA2}},
				"b":  {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "b", Path: "/v1"}, APIKey: "sk-b", HTTPClient: clientB}},
			}
			validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{Backends: backends, Models: map[string]LogicalModel{"m": {Targets: tt.targets}}}, nil
			}}
			srv := New(validator, WithLogger(logger)).server

			doRequest := func(model string) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer user-token")
				req.Header.Set("Content-Type", "application/json")
				srv.ServeHTTP(httptest.NewRecorder(), req)
			}

			if tt.seed != nil { // Given: a transition already in place.
				tt.seed(srv, backends)
			}
			for _, adv := range tt.advances {
				if adv > 0 {
					time.Sleep(adv)
					synctest.Wait()
				}
				doRequest("m")
			}

			down := map[string]any{"msg": "backend down", "level": "INFO", "backend": "a", "model": "ma"}
			maps.Copy(down, tt.downFields)
			up := map[string]any{"msg": "backend up", "level": "INFO", "backend": "a", "model": "ma"}
			maps.Copy(up, tt.upFields)
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
		srv := New(validator, WithLogger(logger)).server
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

// TestChatCompletionsProcessBudgetShed covers what a client turned away by the
// process-wide FD budget is told. Each step is one request, advanced by the wait
// the previous step was handed — a client doing as it was told.
func TestChatCompletionsProcessBudgetShed(t *testing.T) {
	t.Parallel()

	type step struct {
		wantStatus     int
		wantRetryAfter string
	}
	type test struct {
		steps []step
	}

	tests := testy.NewTable[test]()

	tests.Add("should turn a client away when the budget is exhausted", test{
		steps: []step{{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "5"}},
	})
	tests.Add("should make a client wait longer the longer the budget stays full", test{
		steps: []step{
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "5"},
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "10"},
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "20"},
		},
	})

	// TODO(TODO.d/understudy-process-budget-shed.md): add "should start the wait over
	// once the budget has had room again" — a case needs the holder to let its slot
	// go and a later one to take it, which the steps cannot express today.
	// TODO(TODO.d/understudy-process-budget-shed.md): add "should not send every shed
	// client back at the same moment". The scatter is applied here but driven only
	// through the busy path, so dropping it here would go unnoticed.
	tests.Add("should stop lengthening the wait once it reaches the ceiling", test{
		steps: []step{
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "5"},
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "10"},
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "20"},
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "30"},
			{wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "30"},
		},
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			// The upstream blocks so the holder keeps its process slot; with a budget
			// of one, every later request must be shed rather than served.
			backend := testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return &BackendConfig{
					Backends: map[string]Backend{"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: backend}}},
					Models:   map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}}}},
				}, nil
			}}
			// fdSlotBudget(66) = (66-64)/2 = 1: a single process-wide slot.
			srv := New(validator, WithLogger(testLogger(t)), withFDSoftLimit(66)).server
			// These cases assert the interval exactly; scattering it is
			// TestChatCompletionsScattersBusyBackoff's subject.
			srv.jitterFactor = 0

			req := func() *http.Request {
				r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Authorization", "Bearer user-token")
				r.Header.Set("Content-Type", "application/json")
				return r
			}

			go func() { srv.ServeHTTP(httptest.NewRecorder(), req()) }()
			synctest.Wait()

			for i, s := range tt.steps {
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req())

				if rec.Code != s.wantStatus {
					t.Errorf("step %d: status got %d, want %d", i, rec.Code, s.wantStatus)
				}
				if got := rec.Header().Get("Retry-After"); got != s.wantRetryAfter {
					t.Errorf("step %d: Retry-After got %q, want %q", i, got, s.wantRetryAfter)
				}

				waited, err := strconv.Atoi(s.wantRetryAfter)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Duration(waited) * time.Second)
				synctest.Wait()
			}
		})
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

		// t=0: request 1 hits a, gets a bare 429, demoting a with readmitAt zero.
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

func TestChatCompletionsConcurrentSuccessKeepsReadmitBench(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// TODO: should not log "backend up" when a concurrent success lands while
		// the target is still benched by readmitAt. Capture logs here (as
		// TestChatCompletionsTransitionLogging does) instead of discarding them, and
		// assert zero "backend up" records once recSucceeded is read below.
		logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

		// The success is gated so the demotion's health write lands first: the
		// demoting request answers only after demoting, so its completion is the
		// signal that the bench is in the map.
		release := make(chan struct{})
		clientA := testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "bench-me") {
				h := make(http.Header)
				h.Set("Retry-After", "300")
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)), Header: h}, nil
			}
			<-release
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

		newReq := func(model, content string) *http.Request {
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, model, content)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer user-token")
			req.Header.Set("Content-Type", "application/json")
			return req
		}
		serve := func(req *http.Request) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}

		// Two concurrent requests against account "a", one per route to it: the
		// logical model's walk is demoted by a 429 with Retry-After:300 while a
		// request naming a/ma directly succeeds against the same canonical account.
		demoted := make(chan *httptest.ResponseRecorder, 1)
		succeeded := make(chan *httptest.ResponseRecorder, 1)
		go func() { demoted <- serve(newReq("m", "bench-me")) }()
		go func() { succeeded <- serve(newReq("a/ma", "concurrent success")) }()

		recDemoted := <-demoted
		if d := testy.DiffJSON([]byte(`{"id":"from-b"}`), recDemoted.Body.Bytes()); d != nil {
			t.Errorf("demoting request answered from the wrong backend: %s", d)
		}
		close(release)
		recSucceeded := <-succeeded
		if d := testy.DiffJSON([]byte(`{"id":"from-a"}`), recSucceeded.Body.Bytes()); d != nil {
			t.Errorf("concurrent request answered from the wrong backend: %s", d)
		}
		synctest.Wait()

		// Past the 30s recovery interval, well before the 300s readmitAt: the
		// account stays benched, so the walk must keep serving from b.
		time.Sleep(60 * time.Second)
		synctest.Wait()

		rec := serve(newReq("m", "after the race"))
		if d := testy.DiffJSON([]byte(`{"id":"from-b"}`), rec.Body.Bytes()); d != nil {
			t.Errorf("request routed to the wrong backend: %s", d)
		}

		// TODO: should log exactly one "backend up" once readmitAt elapses and a
		// later probe against a actually succeeds. Advance past the 300s readmitAt,
		// have clientA answer 200 on the next call, issue one more request, and
		// assert the transition log (see the first TODO above for the harness).
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

// TestShouldSendTheHigherRankedSessionsTurnToAContendedTargetFirst verifies
// that, given two sessions' turns contending for a busy target, Understudy
// sends the higher-ranked session's turn to the target before the
// lower-ranked one's, however the queue comes to order them: held at
// Understudy, already waiting at the target, both admitted and waiting for
// the upstream account's one slot, or ranked by a forgotten session's
// return. Only the order it sends in is Understudy's: once turns are at the
// target, which the target serves first is the target's own business, so the
// attempts compared are the ones the target keeps, not those Understudy
// withdraws.
func TestShouldSendTheHigherRankedSessionsTurnToAContendedTargetFirst(t *testing.T) {
	type test struct {
		backend  *http.Client
		attempts *targetAttempts
		turns    []clientTurn
		// opens is how long after the test starts the target opens.
		opens time.Duration
		// first and second label the contending turns in the order
		// Understudy should send them.
		first, second string
		// maxConcurrentPerUpstream is how many requests the target's upstream
		// account may hold at a time; zero leaves the server's default.
		maxConcurrentPerUpstream int
	}

	// opensAfter is when the target opens: the turns' total wait plus the
	// grace period, so every turn waiting at it has waited past its grace
	// period before it opens.
	opensAfter := func(turns []clientTurn) time.Duration {
		var d time.Duration
		for _, tn := range turns {
			d += tn.after
		}
		return d + admissionGracePeriod + time.Second
	}
	busyTargetCase := func(t *testing.T, turns []clientTurn, first, second string, atOnce ...string) test {
		t.Helper()
		attempts := &targetAttempts{}
		return test{
			backend:  busyTarget(t, attempts, opensAfter(turns), atOnce...),
			attempts: attempts,
			turns:    turns,
			opens:    opensAfter(turns),
			first:    first,
			second:   second,
		}
	}

	const (
		occupier = "the occupying session asks"
		older    = admissionOlderMessage
		younger  = admissionYoungerMessage
	)

	tests := testy.NewTable[test]()

	tests.AddFunc("should send the older session's turn first when Understudy holds both turns behind a higher-ranked occupier, the younger's having arrived first", func(t *testing.T) test {
		turns := []clientTurn{
			{label: "occupier-opening", userMessages: []string{occupier}},
			{label: "older-opening", after: time.Second, userMessages: []string{older}},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}},
			{label: "occupier", after: time.Second, userMessages: []string{occupier, admissionLongMessage}},
			{label: "younger", after: admissionGracePeriod + time.Second, userMessages: []string{younger, younger + " again"}},
			{label: "older", after: 500 * time.Millisecond, userMessages: []string{older, older + " again"}},
		}
		return busyTargetCase(t, turns, "older", "younger", occupier, older, younger)
	})
	tests.AddFunc("should send the older session's turn first when both turns already wait at the target, the younger's having arrived first", func(t *testing.T) test {
		turns := []clientTurn{
			{label: "older-opening", userMessages: []string{older}},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}},
			{label: "occupier", after: time.Second, userMessages: []string{admissionLongMessage}},
			{label: "younger", after: 500 * time.Millisecond, userMessages: []string{younger, younger + " again"}},
			{label: "older", after: 500 * time.Millisecond, userMessages: []string{older, older + " again"}},
		}
		return busyTargetCase(t, turns, "older", "younger", older, younger)
	})
	// The control for the next case: the same arrival, with the older session
	// still remembered, keeps its turn first.
	tests.AddFunc("should send the older session's turn first when the younger's arrives behind it", func(t *testing.T) test {
		turns := []clientTurn{
			{label: "older-opening", userMessages: []string{older}},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}},
			{label: "occupier", after: time.Second, userMessages: []string{admissionLongMessage}},
			{label: "older", after: time.Second, userMessages: []string{older, older + " again"}},
			{label: "younger", after: admissionGracePeriod + time.Second, userMessages: []string{younger, younger + " again"}},
		}
		return busyTargetCase(t, turns, "older", "younger", older, younger)
	})
	// The younger session keeps taking turns through the older one's absence,
	// each quiet stretch well short of a day, so it is still remembered when
	// the older session returns after more than a day.
	tests.AddFunc("should send the younger session's turn first when the older session was away too long to remember", func(t *testing.T) test {
		turns := []clientTurn{
			{label: "older-opening", userMessages: []string{older}},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}},
			{label: "younger-second", after: 10 * time.Hour, userMessages: []string{younger, younger + " (2)"}},
			{label: "younger-third", after: 10 * time.Hour, userMessages: []string{younger, younger + " (3)"}},
			{label: "occupier", after: 10 * time.Hour, userMessages: []string{admissionLongMessage}},
			{label: "older", after: time.Second, userMessages: []string{older, older + " again"}},
			{label: "younger", after: admissionGracePeriod + time.Second, userMessages: []string{younger, younger + " again"}},
		}
		return busyTargetCase(t, turns, "younger", "older", older, younger, younger+" (2)", younger+" (3)")
	})
	// The older session returns compacted, so only the identifier its
	// client sends names it.
	tests.AddFunc("should send the younger session's request first when the client names the older session and it was away too long to remember", func(t *testing.T) test {
		const (
			olderSession = "older-session-id"
			compacted    = "the older session asks after compaction"
		)
		turns := []clientTurn{
			{label: "older-opening", userMessages: []string{older}, sessionID: olderSession},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}},
			{label: "younger-second", after: 10 * time.Hour, userMessages: []string{younger, younger + " (2)"}},
			{label: "younger-third", after: 10 * time.Hour, userMessages: []string{younger, younger + " (3)"}},
			{label: "occupier", after: 10 * time.Hour, userMessages: []string{admissionLongMessage}},
			{label: "older", after: time.Second, userMessages: []string{compacted, compacted + " again"}, sessionID: olderSession},
			{label: "younger", after: admissionGracePeriod + time.Second, userMessages: []string{younger, younger + " again"}},
		}
		return busyTargetCase(t, turns, "younger", "older", older, younger, younger+" (2)", younger+" (3)")
	})
	// The compacted conversation opens on messages it did not open on, so
	// only the identifier its client sends names the session.
	tests.AddFunc("should send the older session's request first when the client names its session and its conversation was compacted", func(t *testing.T) test {
		const (
			olderSession   = "older-session-id"
			youngerSession = "younger-session-id"
			compacted      = "the older session asks after compaction"
		)
		turns := []clientTurn{
			{label: "older-opening", userMessages: []string{older}, sessionID: olderSession},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}, sessionID: youngerSession},
			{label: "occupier", after: time.Second, userMessages: []string{admissionLongMessage}},
			{label: "younger", after: 500 * time.Millisecond, userMessages: []string{younger, younger + " again"}, sessionID: youngerSession},
			{label: "older", after: 500 * time.Millisecond, userMessages: []string{compacted, compacted + " again"}, sessionID: olderSession},
		}
		return busyTargetCase(t, turns, "older", "younger", older, younger)
	})
	// The younger client's request arrives first, so it waits behind the
	// older one only if its own place in line is scoped to its token, not
	// to the session identifier it shares with the earlier-opening client.
	tests.AddFunc("should send the older session's request first when the younger session's client sends a session identifier another client opened a session under before it", func(t *testing.T) test {
		const (
			sharedSession = "shared-session-id"
			otherOpening  = "the other client's session opens"
			youngerToken  = "younger-client-token"
		)
		turns := []clientTurn{
			{label: "other-opening", userMessages: []string{otherOpening}, sessionID: sharedSession},
			{label: "older-opening", after: time.Second, userMessages: []string{older}},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}, sessionID: sharedSession, token: youngerToken},
			{label: "occupier", after: time.Second, userMessages: []string{admissionLongMessage}},
			{label: "younger", after: 500 * time.Millisecond, userMessages: []string{younger, younger + " again"}, sessionID: sharedSession, token: youngerToken},
			{label: "older", after: 500 * time.Millisecond, userMessages: []string{older, older + " again"}},
		}
		return busyTargetCase(t, turns, "older", "younger", otherOpening, older, younger)
	})
	// A request admitted by a refill that stays unconfirmed past its grace
	// period is displaced by an outranking arrival like any other
	// unconfirmed request, and waits in line for its next turn.
	tests.AddFunc("should send the older session's turn first when it displaces a younger turn that had already waited in line once", func(t *testing.T) test {
		const (
			streamingOccupier = "the streaming occupier asks"
			silentOccupier    = "the silent occupier asks"
		)
		// The target opens at 19s, after the older session's turn has
		// displaced the younger's at t=16s.
		const opens = 19 * time.Second
		attempts := &targetAttempts{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			i := attempts.add(inboundMessage)
			switch inboundMessage {
			case older, younger:
				// The opening turns answer at once, fixing each session's
				// rank.
				return admissionJSON(inboundMessage), nil
			case streamingOccupier:
				// Streams first content at once, confirming it, and ends
				// the stream 10s later, so its completion refills a place
				// before the target opens.
				reads := 0
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						reads++
						if reads == 1 {
							return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
						}
						time.Sleep(10 * time.Second)
						return 0, io.EOF
					})),
				}, nil
			case silentOccupier:
				// Silent until the target opens, then streams first
				// content and holds the stream open, so it confirms
				// without completing and admits only one request.
				if !attempts.waitUntil(r, i, opens) {
					return nil, context.Cause(r.Context())
				}
				return admissionStream(r, nil, nil), nil
			}
			if !attempts.waitUntil(r, i, opens) {
				return nil, context.Cause(r.Context())
			}
			return admissionJSON(inboundMessage), nil
		})
		turns := []clientTurn{
			{label: "streaming-occupier", userMessages: []string{streamingOccupier}},
			{label: "silent-occupier", after: time.Second, userMessages: []string{silentOccupier}},
			{label: "older-opening", after: time.Second, userMessages: []string{older}},
			{label: "younger-opening", after: time.Second, userMessages: []string{younger}},
			// The silent occupier is unconfirmed past its grace period, so
			// the younger's turn waits. The streaming occupier's completion
			// at t=10s refills a place and admits it headerless.
			{label: "younger", after: 3 * time.Second, userMessages: []string{younger, younger + " again"}},
			// At t=16s the younger's turn is still unconfirmed past its
			// grace period, so this one outranks and displaces it.
			{label: "older", after: 10 * time.Second, userMessages: []string{older, older + " again"}},
		}
		return test{
			backend:  backend,
			attempts: attempts,
			turns:    turns,
			opens:    opens,
			first:    "older",
			second:   "younger",
		}
	})

	tests.AddFunc("should give the target's freed slot to the waiting request from the oldest session", func(t *testing.T) test {
		const streaming = "the streaming session asks"
		attempts := &targetAttempts{}
		backend := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			attempts.add(inboundMessage)
			if inboundMessage != streaming {
				return admissionJSON(inboundMessage), nil
			}
			reads := 0
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
					reads++
					if reads == 1 {
						// First content at once, so the queue holds no
						// later arrival behind the stream.
						return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
					}
					// The stream ends before either waiting request's grace
					// period runs out, so the queue is not what orders them.
					time.Sleep(admissionGracePeriod)
					return 0, io.EOF
				})),
			}, nil
		})
		turns := []clientTurn{
			{label: "older-opening", userMessages: []string{older}},
			{label: "streamer", after: time.Second, userMessages: []string{streaming}},
			{label: "younger", after: time.Second, userMessages: []string{younger}},
			{label: "older", after: admissionGracePeriod / 4, userMessages: []string{older, older + " again"}},
		}
		return test{
			backend:  backend,
			attempts: attempts,
			turns:    turns,
			// Past the stream's end, with both waiting requests answered.
			opens:                    time.Second + admissionGracePeriod + time.Second,
			first:                    "older",
			second:                   "younger",
			maxConcurrentPerUpstream: 1,
		}
	})
	tests.AddFunc("should give the target's freed slot to a waiting request when the client of an older session's request waiting for the slot has disconnected", func(t *testing.T) test {
		const (
			streaming = "the streaming session asks"
			departing = "the departing session asks"
		)
		attempts := &targetAttempts{}
		backend := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			attempts.add(inboundMessage)
			if inboundMessage != streaming {
				return admissionJSON(inboundMessage), nil
			}
			reads := 0
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
					reads++
					if reads == 1 {
						// First content at once, so the waiters queue
						// behind a submitted stream, not a held one.
						return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
					}
					// Ends within the waiters' grace period, so rank
					// alone decides who goes next.
					time.Sleep(admissionGracePeriod)
					return 0, io.EOF
				})),
			}, nil
		})
		turns := []clientTurn{
			{label: "streamer", userMessages: []string{streaming}},
			// The departing session is seen before the younger one, so
			// its request outranks the younger's until its client leaves.
			{label: "departing", after: admissionGracePeriod / 8, userMessages: []string{departing}, clientLeavesAfter: admissionGracePeriod / 4},
			{label: "younger", after: admissionGracePeriod / 8, userMessages: []string{younger}},
		}
		return test{
			backend:                  backend,
			attempts:                 attempts,
			turns:                    turns,
			opens:                    admissionGracePeriod + time.Second,
			first:                    "streamer",
			second:                   "younger",
			maxConcurrentPerUpstream: 1,
		}
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			srv := newAdmissionServer(t, tt.backend)
			srv.maxConcurrentPerUpstream = cmp.Or(tt.maxConcurrentPerUpstream, defaultMaxConcurrentPerUpstream)
			poster := newAdmissionPoster(t, srv)
			labels := map[string]string{}
			for _, tn := range tt.turns {
				labels[tn.userMessages[len(tn.userMessages)-1]] = tn.name()
			}
			start := time.Now()
			postTurns(t, poster, tt.turns)

			time.Sleep(time.Until(start.Add(tt.opens)))
			synctest.Wait()

			messages := tt.attempts.kept()
			kept := make([]string, 0, len(messages))
			for _, message := range messages {
				kept = append(kept, labels[message])
			}
			i, j := slices.Index(kept, tt.first), slices.Index(kept, tt.second)
			if i < 0 || j < 0 || i > j {
				t.Errorf("the backend kept attempts in order %v; Understudy should send %s before %s", kept, tt.first, tt.second)
			}
		})
	})
}

func TestCounts(t *testing.T) {
	type test struct {
		// requests is one request per conversation, in the order the
		// conversations send them.
		requests []clientTurn
		// want is the operator's counts once every request has been
		// answered.
		want Counts
	}

	tests := testy.NewTable[test]()

	tests.Add("should count only the conversations that sent a request within the last day, once another conversation sends one", test{
		requests: []clientTurn{
			{label: "conversation A", userMessages: []string{"conversation A asks"}},
			{label: "conversation B", after: 12 * time.Hour, userMessages: []string{"conversation B asks"}},
			// More than a day after conversation A's request, and less
			// than a day after conversation B's.
			{label: "conversation C", after: 13 * time.Hour, userMessages: []string{"conversation C asks"}},
		},
		want: Counts{Conversations: 2},
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			backend := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
				return admissionJSON(inboundMessage), nil
			})
			handler := New(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
				return openaiBackend(t, "http://backend/v1", "sk-test", backend), nil
			}}, WithLogger(testLogger(t)))
			poster := newAdmissionPoster(t, handler)
			postTurns(t, poster, tt.requests)
			labels := make([]string, 0, len(tt.requests))
			for _, request := range tt.requests {
				labels = append(labels, request.name())
			}
			poster.answeredOK(labels...)

			if d := gocmp.Diff(tt.want, handler.Counts()); d != "" {
				t.Errorf("unexpected operator counts (-want +got):\n%s", d)
			}
		})
	})
}

// clientTurn is one turn of one session's conversation: the chat request a
// test's client sends to Understudy.
type clientTurn struct {
	// label names the turn to the test; with none, it is the turn's last
	// user message.
	label string
	// model is the model the turn names; with none, the admission tests'
	// default model.
	model string
	// after is how long the client waits, after the previous turn, before
	// sending this one.
	after time.Duration
	// userMessages are the user messages of the turn's conversation, in
	// order: the first names its session, and the last is the one the
	// target sees. Each is unique across the case.
	userMessages []string
	// sessionID is the session identifier the client's x-session-affinity
	// header carries; empty sends none.
	sessionID string
	// token is the bearer token the client sends; empty sends the admission
	// tests' default.
	token string
	// clientLeavesAfter is how long after sending the turn the client gives
	// up waiting; zero, it waits for its answer.
	clientLeavesAfter time.Duration
}

// name is the turn's label, or its last user message when it has none.
func (tn clientTurn) name() string {
	return cmp.Or(tn.label, tn.userMessages[len(tn.userMessages)-1])
}

// turn is the chat request the turn sends over messages.
func (tn clientTurn) turn(messages ...string) string {
	return admissionModelTurn(cmp.Or(tn.model, "openai/gpt-4"), messages...)
}

// postTurns sends turns through poster, each after its delay and labeled by
// its name, letting each settle before the next.
func postTurns(t *testing.T, poster *admissionPoster, turns []clientTurn) {
	t.Helper()
	for _, tn := range turns {
		time.Sleep(tn.after)
		ctx, clientLeaves := context.WithTimeout(t.Context(), cmp.Or(tn.clientLeavesAfter, time.Hour))
		t.Cleanup(clientLeaves)
		messages := make([]string, 0, 2*len(tn.userMessages))
		for i, message := range tn.userMessages {
			if i > 0 {
				messages = append(messages, admissionMsg("assistant", "word"))
			}
			messages = append(messages, admissionMsg("user", message))
		}
		go poster.postSession(ctx, tn.name(), tn.turn(messages...), tn.token, tn.sessionID)
		synctest.Wait()
	}
}

// targetAttempts records, in order, the inbound messages of the requests a
// fake target receives, and which of them Understudy withdrew before the
// target answered.
type targetAttempts struct {
	mu       sync.Mutex
	start    time.Time
	received []targetAttempt
}

// targetAttempt is one request a fake target received.
type targetAttempt struct {
	inboundMessage string
	withdrawn      bool
}

// add records a request's arrival and returns its index, for use with
// [targetAttempts.waitUntil] and [targetAttempts.after], which measure their
// durations from the first arrival.
func (a *targetAttempts) add(inboundMessage string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.start.IsZero() {
		a.start = time.Now()
	}
	a.received = append(a.received, targetAttempt{inboundMessage: inboundMessage})
	return len(a.received) - 1
}

// waitUntil holds request i until d after the target's first request, and
// reports whether d passed before the client withdrew the request, marking it
// withdrawn if so.
func (a *targetAttempts) waitUntil(r *http.Request, i int, d time.Duration) bool {
	a.mu.Lock()
	at := a.start.Add(d)
	a.mu.Unlock()
	select {
	case <-time.After(time.Until(at)):
		return true
	case <-r.Context().Done():
		a.mu.Lock()
		a.received[i].withdrawn = true
		a.mu.Unlock()
		return false
	}
}

// after delivers d after the target's first request.
func (a *targetAttempts) after(d time.Duration) <-chan time.Time {
	a.mu.Lock()
	at := a.start.Add(d)
	a.mu.Unlock()
	return time.After(time.Until(at))
}

// kept is the inbound messages of the requests the target kept, in the order
// it received them.
func (a *targetAttempts) kept() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var kept []string
	for _, r := range a.received {
		if !r.withdrawn {
			kept = append(kept, r.inboundMessage)
		}
	}
	return kept
}

// busyTarget is a fake target busy until opens after its first request: it
// answers the requests named in atOnce as soon as they arrive, and holds every
// other request until it opens. Then it streams the occupying session's
// ([admissionLongMessage]) first content and keeps that stream open, so
// Understudy confirms the turns it sends one at a time, and answers the
// rest in full.
func busyTarget(t *testing.T, attempts *targetAttempts, opens time.Duration, atOnce ...string) *http.Client {
	return admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
		i := attempts.add(inboundMessage)
		if slices.Contains(atOnce, inboundMessage) {
			return admissionJSON(inboundMessage), nil
		}
		if !attempts.waitUntil(r, i, opens) {
			return nil, context.Cause(r.Context())
		}
		if inboundMessage != admissionLongMessage {
			return admissionJSON(inboundMessage), nil
		}
		return admissionStream(r, nil, nil), nil
	})
}

// TODO(TODO.d/admission-reservation-window.md): a turn arriving within the
// reservation window keeps the session's place uncontested.

// admissionLongMessage is the occupying session's message, whose turn holds
// the target's only slot until the test ends its generation
// ([admissionTarget.serve]).
const admissionLongMessage = "the occupying session asks for a long answer"

// admissionOlderMessage and admissionYoungerMessage open two sessions whose
// turns contend for one target; the older session is the one seen first.
const (
	admissionOlderMessage   = "the older session asks"
	admissionYoungerMessage = "the younger session asks"
)

// admissionTarget doubles a model server with a single decode slot: it serves
// one request at a time, admits waiters in the order they reached it, and
// answers nothing — status, header, or body — until it starts serving one.
type admissionTarget struct {
	mu      sync.Mutex
	serving bool
	waiters []chan struct{}

	longGenerationEnds chan struct{}
	endOnce            sync.Once
}

// newAdmissionTarget registers [admissionTarget.endLongGeneration] as a
// cleanup, so a test that fails before ending the generation leaves nothing
// blocked.
func newAdmissionTarget(t *testing.T) *admissionTarget {
	a := &admissionTarget{longGenerationEnds: make(chan struct{})}
	t.Cleanup(a.endLongGeneration)
	return a
}

// endLongGeneration ends the occupying session's generation.
func (a *admissionTarget) endLongGeneration() {
	a.endOnce.Do(func() { close(a.longGenerationEnds) })
}

func (a *admissionTarget) acquire(ctx context.Context) error {
	a.mu.Lock()
	if !a.serving {
		a.serving = true
		a.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	a.waiters = append(a.waiters, ch)
	a.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		a.mu.Lock()
		a.waiters = slices.DeleteFunc(a.waiters, func(c chan struct{}) bool { return c == ch })
		a.mu.Unlock()
		return ctx.Err()
	}
}

func (a *admissionTarget) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.waiters) > 0 {
		close(a.waiters[0])
		a.waiters = a.waiters[1:]
		return
	}
	a.serving = false
}

// serve runs one request through the target's slot, holding it until
// [admissionTarget.endLongGeneration] when inboundMessage is the occupying
// session's. A canceled generation still occupies the slot to the end: the
// target offers no finer-grained interruption than that.
func (a *admissionTarget) serve(ctx context.Context, inboundMessage string) error {
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if inboundMessage != admissionLongMessage {
		return nil
	}
	select {
	case <-ctx.Done():
		<-a.longGenerationEnds
		return ctx.Err()
	case <-a.longGenerationEnds:
	}
	return nil
}

// admissionMessage is one chat message in a request body.
type admissionMessage struct {
	Content string `json:"content"`
}

func admissionMsg(role, content string) string {
	return fmt.Sprintf(`{"role":%q,"content":%q}`, role, content)
}

func admissionTurn(messages ...string) string {
	return admissionModelTurn("openai/gpt-4", messages...)
}

// admissionModelTurn is [admissionTurn] naming model.
func admissionModelTurn(model string, messages ...string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[%s]}`, model, strings.Join(messages, ","))
}

// admissionInboundMessage reports a request's last message, which names the
// request for a doubled target: a session's second turn stays
// distinguishable from its first.
func admissionInboundMessage(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return ""
	}
	var chat struct {
		Messages []admissionMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Errorf("unparsable chat body %q: %v", body, err)
		return ""
	}
	if len(chat.Messages) == 0 {
		t.Errorf("chat body %q carries no messages", body)
		return ""
	}
	return chat.Messages[len(chat.Messages)-1].Content
}

func admissionJSON(id string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q}`, id))),
	}
}

// admissionStream answers with first content at once and keeps the stream
// open until ends delivers, then runs ended, if given, and ends the stream; a
// nil ends keeps it open until the request is canceled.
func admissionStream(r *http.Request, ends <-chan time.Time, ended func()) *http.Response {
	reads := 0
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
			reads++
			if reads == 1 {
				return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
			}
			select {
			case <-r.Context().Done():
				return 0, context.Cause(r.Context())
			case <-ends:
			}
			if ended != nil {
				ended()
			}
			return 0, io.EOF
		})),
	}
}

// admissionStreamContentAt is admissionStream with its first content
// withheld until contentAt delivers; when ends delivers it ends the stream,
// and a nil ends keeps it open until the request is canceled.
func admissionStreamContentAt(r *http.Request, contentAt, ends <-chan time.Time) *http.Response {
	reads := 0
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
			reads++
			wait := ends
			if reads == 1 {
				wait = contentAt
			}
			select {
			case <-r.Context().Done():
				return 0, context.Cause(r.Context())
			case <-wait:
			}
			if reads == 1 {
				return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
			}
			return 0, io.EOF
		})),
	}
}

// admissionServerBusy is a server's refusal of a request because the models
// it is already running leave no room for the request's model.
func admissionServerBusy() *http.Response {
	return &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"server busy","code":"unavailable"}}`)),
	}
}

// admissionClient hands each request to handle along with the inbound
// message that names it. A nil handle serves every request through the slot
// and answers with that message as its id.
func admissionClient(t *testing.T, target *admissionTarget, handle func(r *http.Request, inboundMessage string) (*http.Response, error)) *http.Client {
	if handle == nil {
		handle = func(r *http.Request, inboundMessage string) (*http.Response, error) {
			if err := target.serve(r.Context(), inboundMessage); err != nil {
				return nil, err
			}
			return admissionJSON(inboundMessage), nil
		}
	}
	return testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
		return handle(r, admissionInboundMessage(t, r))
	})
}

// newAdmissionServer builds the server an admission case drives: one backend,
// reached through client.
func newAdmissionServer(t *testing.T, client *http.Client) *server {
	t.Helper()
	srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
		return openaiBackend(t, "http://backend/v1", "sk-test", client), nil
	}})
	srv.logger = testLogger(t)
	return srv
}

// newTargetsServer builds a server with a backend per client in clients,
// each named for its key and reached through that client, and models as its
// logical models.
func newTargetsServer(t *testing.T, clients map[string]*http.Client, models map[string]LogicalModel) *server {
	t.Helper()
	backends := make(map[string]Backend, len(clients))
	for name, client := range clients {
		backends[name] = targetBackend(name, name, client)
	}
	return newBackendsServer(t, backends, models)
}

func targetBackend(name, host string, client *http.Client) Backend {
	return Backend{ProviderType: "openai", Config: providers.Config{
		BaseURL:    &url.URL{Scheme: "http", Host: host, Path: "/v1"},
		APIKey:     "sk-" + name,
		HTTPClient: client,
	}}
}

func newBackendsServer(t *testing.T, backends map[string]Backend, models map[string]LogicalModel) *server {
	t.Helper()
	srv := newTestServer(&stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
		return &BackendConfig{Backends: backends, Models: models}, nil
	}})
	srv.logger = testLogger(t)
	return srv
}

// admissionPoster posts chat turns at a server and keeps each answer and
// each request's log record under its label, so a case can ask whether a
// request has been answered yet, in what order the answers reached their
// clients, and what the request's handler recorded.
type admissionPoster struct {
	t       *testing.T
	srv     http.Handler
	mu      sync.Mutex
	answers map[string]*httptest.ResponseRecorder
	records map[string]LogRecord
	order   []string
}

func newAdmissionPoster(t *testing.T, srv http.Handler) *admissionPoster {
	return &admissionPoster{t: t, srv: srv, answers: map[string]*httptest.ResponseRecorder{}, records: map[string]LogRecord{}}
}

func (p *admissionPoster) post(ctx context.Context, label, body string) {
	p.postSession(ctx, label, body, "", "")
}

// postSession posts a chat request that carries token as its bearer token,
// the default when empty, and session in its x-session-affinity header.
func (p *admissionPoster) postSession(ctx context.Context, label, body, token, session string) {
	ctx = WithLogCtx(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		p.t.Error(err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cmp.Or(token, "user-token"))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("x-session-affinity", session)
	}
	rec := httptest.NewRecorder()
	p.srv.ServeHTTP(rec, req)
	p.mu.Lock()
	p.answers[label] = rec
	p.order = append(p.order, label)
	if record, ok := LogRecordFromContext(ctx); ok {
		p.records[label] = record
	}
	p.mu.Unlock()
}

func (p *admissionPoster) answered(label string) (*httptest.ResponseRecorder, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.answers[label]
	return rec, ok
}

// logRecord returns the log record of the request posted under label, once
// that request's answer has reached its client.
func (p *admissionPoster) logRecord(label string) (LogRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.records[label]
	return rec, ok
}

// answeredOK fails the case unless every label named has been answered 200.
func (p *admissionPoster) answeredOK(labels ...string) {
	p.t.Helper()
	statuses := make(map[string]int, len(labels))
	for _, label := range labels {
		statuses[label] = http.StatusOK
	}
	p.answeredWith(statuses)
}

// answeredWith fails the case unless every label in statuses has been
// answered with its status.
func (p *admissionPoster) answeredWith(statuses map[string]int) {
	p.t.Helper()
	for label, status := range statuses {
		rec, ok := p.answered(label)
		if !ok {
			p.t.Errorf("%s never received an answer", label)
			continue
		}
		if rec.Code != status {
			p.t.Errorf("%s: status %d, want %d", label, rec.Code, status)
		}
	}
}

// admissionQueueFormed posts the turns that leave the younger session's turn
// queued behind the older session's second turn: the occupying session takes
// the target's only slot, the older session's opening turn fixes its age, the
// younger session's turn waits at the target past the grace period with no
// header, and the older session's second turn outranks it and displaces it.
// The occupying session asks first so it outranks the older one: an arrival
// displaces every headerless request ranked below it, and a displaced
// occupying generation would re-run ahead of the younger session's turn. The
// younger session posts under youngerCtx so a case can take its client away
// while it waits.
func admissionQueueFormed(ctx, youngerCtx context.Context, p *admissionPoster) {
	// The older session's second turn repeats its opening turn's messages,
	// which fix its rank, so only this function needs them by name.
	const olderSystem = "answer in one word"

	go p.post(ctx, "occupier", admissionTurn(admissionMsg("user", admissionLongMessage)))
	synctest.Wait()
	time.Sleep(time.Second)

	go p.post(ctx, "older-opening", admissionTurn(admissionMsg("system", olderSystem), admissionMsg("user", admissionOlderMessage)))
	synctest.Wait()
	time.Sleep(time.Second)

	go p.post(youngerCtx, "younger", admissionTurn(admissionMsg("user", admissionYoungerMessage)))
	synctest.Wait()
	time.Sleep(3 * time.Second)

	go p.post(ctx, "older", admissionTurn(
		admissionMsg("system", olderSystem),
		admissionMsg("user", admissionOlderMessage),
		admissionMsg("assistant", "word"),
		admissionMsg("user", admissionOlderMessage+" again"),
	))
	synctest.Wait()
}

// TestShouldHoldTheNextWaiterUntilAStreamedTargetReachesItsFirstDataEvent
// verifies that a framing line is not content, whatever the Content-Type
// claims.
func TestShouldHoldTheNextWaiterUntilAStreamedTargetReachesItsFirstDataEvent(t *testing.T) {
	type test struct {
		// opening is the stream's first framing line.
		opening string
	}

	tests := testy.NewTable[test]()

	tests.Add("should hold the next waiter while the stream opens with a keep-alive comment", test{
		opening: ": keep-alive\n\n",
	})
	tests.Add("should hold the next waiter while the stream opens with an event framing line", test{
		opening: "event: ping\n\n",
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			const (
				streamedMessage = "the streamed session asks"
				heldMessage     = "the held session asks"
			)
			// Holds the streamed answer back past the turn's grace period.
			answerBegins := make(chan struct{})
			// Holds the stream's first data: event behind the opening framing line.
			dataArrives := make(chan struct{})
			// Keeps the stream open past the first data: event, so only that
			// event can admit the held turn.
			streamEnds := make(chan struct{})
			t.Cleanup(func() { close(streamEnds) })

			client := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
				if inboundMessage != streamedMessage {
					return admissionJSON(inboundMessage), nil
				}
				select {
				case <-r.Context().Done():
					return nil, context.Cause(r.Context())
				case <-answerBegins:
				}
				reads := 0
				return &http.Response{
					StatusCode: http.StatusOK,
					// Mislabeled on purpose: the body's own bytes, not the
					// Content-Type, decide what counts as content.
					Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						reads++
						switch reads {
						case 1:
							return copy(p, tt.opening), nil
						case 2:
							<-dataArrives
							return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
						}
						<-streamEnds
						return 0, io.EOF
					})),
				}, nil
			})

			poster := newAdmissionPoster(t, newAdmissionServer(t, client))
			post := func(label, message string) {
				go poster.post(t.Context(), label, admissionTurn(admissionMsg("user", message)))
			}

			post("streamed", streamedMessage)
			synctest.Wait()
			time.Sleep(admissionGracePeriod + time.Second)

			post("held", heldMessage)
			synctest.Wait()

			close(answerBegins)
			synctest.Wait()
			if _, ok := poster.answered("held"); ok {
				t.Errorf("the held turn was answered while the stream ahead of it had sent only %q; a framing line is not content whatever the Content-Type claims", strings.TrimSpace(tt.opening))
			}

			close(dataArrives)
			synctest.Wait()
			poster.answeredOK("held")
		})
	})
}

// statusWritingRecorder records whether a status was written; a plain
// recorder defaults its code to 200 before anything is written, so it cannot.
type statusWritingRecorder struct {
	*httptest.ResponseRecorder
	wroteStatus bool
}

func (r *statusWritingRecorder) WriteHeader(code int) {
	r.wroteStatus = true
	r.ResponseRecorder.WriteHeader(code)
}

func (r *statusWritingRecorder) Write(p []byte) (int, error) {
	r.wroteStatus = true
	return r.ResponseRecorder.Write(p)
}

// TestShouldReceiveTheUpstreamsStatusHeadersAndBytesOnlyAtItsFirstContentOrItsEnd
// verifies that an upstream answer ahead of its first content reaches the
// client as nothing, and that the upstream's first content or the end of its
// body releases the whole answer: status, headers, and every byte sent, in
// order.
func TestShouldReceiveTheUpstreamsStatusHeadersAndBytesOnlyAtItsFirstContentOrItsEnd(t *testing.T) {
	// read is one read of the upstream's body.
	type read struct {
		data string
		err  error
	}
	type test struct {
		// opening is the upstream's body before the test holds it; not content.
		opening string
		// next is what the body yields after opening, once the test releases it.
		next []read
	}

	const keepAlives = ": keep-alive\n\n: keep-alive\n\n"

	tests := testy.NewTable[test]()
	tests.Add("should receive the upstream's answer when its first content arrives", test{
		opening: keepAlives,
		next:    []read{{data: "data: {\"id\":\"streamed\"}\n\n"}},
	})
	tests.Add("should receive the upstream's answer when its body ends after only keep-alive lines", test{
		opening: keepAlives,
		next:    []read{{err: io.EOF}},
	})
	tests.Add("should receive the upstream's answer when its body ends after only leading whitespace", test{
		opening: "\n  \r\n\t",
		next:    []read{{err: io.EOF}},
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			const message = "the streaming session asks"
			upstreamGoesOn := make(chan struct{})
			// streamEnds keeps the stream open past the assertions, so the answer
			// under them is exactly the bytes the upstream has sent.
			streamEnds := make(chan struct{})
			t.Cleanup(func() { close(streamEnds) })

			client := admissionClient(t, nil, func(r *http.Request, _ string) (*http.Response, error) {
				reads := 0
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": {"text/event-stream"},
						"X-Upstream":   {"upstreams-own-header"},
					},
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						reads++
						if reads == 1 {
							return copy(p, tt.opening), nil
						}
						if reads == 2 {
							select {
							case <-r.Context().Done():
								return 0, context.Cause(r.Context())
							case <-upstreamGoesOn:
							}
						}
						if i := reads - 2; i < len(tt.next) {
							return copy(p, tt.next[i].data), tt.next[i].err
						}
						<-streamEnds
						return 0, io.EOF
					})),
				}, nil
			})

			rec := &statusWritingRecorder{ResponseRecorder: httptest.NewRecorder()}
			go func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
					strings.NewReader(admissionTurn(admissionMsg("user", message))))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer user-token")
				req.Header.Set("Content-Type", "application/json")
				newAdmissionServer(t, client).ServeHTTP(rec, req)
			}()
			synctest.Wait()

			if rec.wroteStatus {
				t.Error("the client was sent a status while the upstream had sent no content, want none")
			}
			if len(rec.Header()) != 0 {
				t.Errorf("the client was sent headers %v while the upstream had sent no content, want none", rec.Header())
			}
			if rec.Body.Len() != 0 {
				t.Errorf("the client was sent body %q while the upstream had sent no content, want none", rec.Body.String())
			}

			close(upstreamGoesOn)
			synctest.Wait()

			if !rec.wroteStatus || rec.Code != http.StatusOK {
				t.Errorf("once the upstream went on the client was sent status %d (written %t), want the upstream's %d", rec.Code, rec.wroteStatus, http.StatusOK)
			}
			if got := rec.Header().Get("X-Upstream"); got != "upstreams-own-header" {
				t.Errorf("once the upstream went on the client was sent X-Upstream %q, want the upstream's %q", got, "upstreams-own-header")
			}
			if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Errorf("once the upstream went on the client was sent Content-Type %q, want the upstream's %q", got, "text/event-stream")
			}
			var want strings.Builder
			want.WriteString(tt.opening)
			for _, r := range tt.next {
				want.WriteString(r.data)
			}
			if got := rec.Body.String(); got != want.String() {
				t.Errorf("once the upstream went on the client was sent body %q, want every byte the upstream had sent, in order: %q", got, want.String())
			}
		})
	})
}

// TestShouldAdmitAHeldTurn verifies that a turn held behind an unconfirmed
// request at its target is admitted when the fake target opens, and only
// then: each case keeps another turn unresolved throughout.
func TestShouldAdmitAHeldTurn(t *testing.T) {
	type test struct {
		backend  *http.Client
		attempts *targetAttempts
		turns    []clientTurn
		// opens is how long after the first turn the target opens.
		opens time.Duration
		// held is the turn Understudy holds back, not forwarding it to the
		// target until the target opens.
		held string
		// pending is a turn that stays unresolved throughout the test, so
		// the opening alone can have admitted held.
		pending string
	}

	// opensAfter is when a target opens on turns: a second after the last
	// turn is sent.
	opensAfter := func(turns []clientTurn) time.Duration {
		var d time.Duration
		for _, tn := range turns {
			d += tn.after
		}
		return d + time.Second
	}
	// stream is a target's streamed answer: headers at once, its first
	// content when contentAt delivers, and its end, with endErr, when endsAt
	// delivers. A nil channel never delivers.
	stream := func(r *http.Request, contentAt, endsAt <-chan time.Time, endErr error) *http.Response {
		reads := 0
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
				reads++
				if reads == 1 {
					select {
					case <-r.Context().Done():
						return 0, context.Cause(r.Context())
					case <-contentAt:
					}
					return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
				}
				select {
				case <-r.Context().Done():
					return 0, context.Cause(r.Context())
				case <-endsAt:
					return 0, endErr
				}
			})),
		}
	}
	// confirmedThenEnds is a target whose "confirmed" turn streams at once
	// and ends, with endErr, when the target opens, whose "unconfirmed" turn
	// sends headers and never any content, and which answers every other
	// turn at once.
	confirmedThenEnds := func(t *testing.T, attempts *targetAttempts, opens time.Duration, endErr error) *http.Client {
		return admissionClient(t, nil, func(r *http.Request, message string) (*http.Response, error) {
			attempts.add(message)
			switch message {
			case "confirmed":
				return stream(r, time.After(0), attempts.after(opens), endErr), nil
			case "unconfirmed":
				return stream(r, nil, nil, nil), nil
			}
			return admissionJSON(message), nil
		})
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should admit the held turn at the occupying turn's first content, while its stream stays open", func(t *testing.T) test {
		turns := []clientTurn{
			{userMessages: []string{"occupier"}},
			{userMessages: []string{"held"}, after: admissionGracePeriod + time.Second},
		}
		attempts := &targetAttempts{}
		backend := admissionClient(t, nil, func(r *http.Request, message string) (*http.Response, error) {
			attempts.add(message)
			if message == "occupier" {
				return stream(r, attempts.after(opensAfter(turns)), nil, nil), nil
			}
			return admissionJSON(message), nil
		})
		return test{backend: backend, attempts: attempts, turns: turns, opens: opensAfter(turns), held: "held", pending: "occupier"}
	})
	tests.AddFunc("should admit the held turn when a confirmed turn completes while another turn at the target is unconfirmed", func(t *testing.T) test {
		turns := []clientTurn{
			{userMessages: []string{"confirmed"}},
			{userMessages: []string{"unconfirmed"}, after: time.Second},
			{userMessages: []string{"held"}, after: admissionGracePeriod + time.Second},
		}
		attempts := &targetAttempts{}
		return test{
			backend: confirmedThenEnds(t, attempts, opensAfter(turns), io.EOF), attempts: attempts,
			turns: turns, opens: opensAfter(turns), held: "held", pending: "unconfirmed",
		}
	})
	tests.AddFunc("should send a request held back from a busy backend as soon as one of the backend's streaming answers breaks off with an error", func(t *testing.T) test {
		turns := []clientTurn{
			{userMessages: []string{"confirmed"}},
			{userMessages: []string{"unconfirmed"}, after: time.Second},
			{userMessages: []string{"held"}, after: admissionGracePeriod + time.Second},
		}
		attempts := &targetAttempts{}
		return test{
			backend: confirmedThenEnds(t, attempts, opensAfter(turns), io.ErrUnexpectedEOF), attempts: attempts,
			turns: turns, opens: opensAfter(turns), held: "held", pending: "unconfirmed",
		}
	})
	tests.AddFunc("should send the request behind a held one whose client has left, as soon as one of a busy backend's streaming answers completes", func(t *testing.T) test {
		turns := []clientTurn{
			{userMessages: []string{"confirmed"}},
			{userMessages: []string{"unconfirmed"}, after: time.Second},
			// Sent before "held", so it sits at the head of the wait list;
			// its client leaves before the confirmed turn's stream ends.
			{userMessages: []string{"departed"}, after: admissionGracePeriod + time.Second, clientLeavesAfter: time.Second / 4},
			{userMessages: []string{"held"}, after: time.Second / 2},
		}
		attempts := &targetAttempts{}
		return test{
			backend: confirmedThenEnds(t, attempts, opensAfter(turns), io.EOF), attempts: attempts,
			turns: turns, opens: opensAfter(turns), held: "held", pending: "unconfirmed",
		}
	})
	tests.AddFunc("should hold an older session's request that displaces a younger one until the younger one's canceled send has ended", func(t *testing.T) test {
		turns := []clientTurn{
			{userMessages: []string{"older"}},
			{userMessages: []string{"younger"}, after: time.Second / 2},
			{userMessages: []string{"older", "held"}, after: admissionGracePeriod + time.Second},
		}
		attempts := &targetAttempts{}
		var displacedSendEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, message string) (*http.Response, error) {
			if message == "held" && !displacedSendEnded.Load() {
				t.Errorf("the target received %q while the send it displaced was still open", message)
			}
			if message != "younger" {
				return admissionJSON(message), nil
			}
			if attempts.add(message) == 0 {
				// Slow to let go once canceled, so a request let in before
				// this send ends reaches the target while it is still open.
				<-r.Context().Done()
				time.Sleep(time.Second / 2)
				displacedSendEnded.Store(true)
				return nil, context.Cause(r.Context())
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{backend: backend, attempts: attempts, turns: turns, opens: opensAfter(turns), held: "held", pending: "younger"}
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			poster := newAdmissionPoster(t, newAdmissionServer(t, tt.backend))
			start := time.Now()
			postTurns(t, poster, tt.turns)
			if _, ok := poster.answered(tt.held); ok {
				t.Fatalf("setup: %q was answered on arrival; the case needs it held", tt.held)
			}

			time.Sleep(time.Until(start.Add(tt.opens)))
			synctest.Wait()
			if _, ok := poster.answered(tt.pending); ok {
				t.Fatalf("setup: %q resolved; the case needs it pending, so that only the opening can admit %q", tt.pending, tt.held)
			}
			poster.answeredOK(tt.held)
		})
	})
}

// sseAnswer is a fake backend's whole answer as a stream of one event whose
// data is {"id": id}.
func sseAnswer(id string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf("data: {\"id\":%q}\n\n", id))),
	}
}

// TestShouldSendARequestAgainOnlyWhenItsAttemptEndsBeforeContent verifies
// when Understudy sends a request to its target again. An attempt that ends
// before any content without failing — displaced by an older session's
// arrival, or cut by the first-content timeout — is sent again, and its
// client receives the second send's answer. An attempt whose content has
// begun is relayed to its end and never sent again.
func TestShouldSendARequestAgainOnlyWhenItsAttemptEndsBeforeContent(t *testing.T) {
	// arrival is one request reaching Understudy.
	type arrival struct {
		// inboundMessage is the last user message the client sends; it
		// names the request.
		inboundMessage string
		body           string
		// after is how long the client waits after the previous arrival
		// before sending it.
		after time.Duration
	}
	type test struct {
		// backends are the fake targets the server is configured with,
		// keyed by backend name.
		backends map[string]*http.Client
		models   map[string]LogicalModel
		// received records the inbound messages of the requests that reach
		// the followed request's target.
		received *receivedMessages
		arrivals []arrival
		// wait is how long the test waits after the last arrival before
		// looking at the answer.
		wait time.Duration
		// followed is the inbound message of the request the test follows;
		// events is the data of the events its client should receive, and
		// sends how many times the backend serving it should receive it.
		followed string
		events   []string
		sends    int
		// handlerDelay is how long a first-content timeout's handler waits
		// after its timer fires: zero, at once; more, a firing Stop can no
		// longer prevent.
		handlerDelay time.Duration
		// check, when set, runs the case's own assertions after the
		// standard ones.
		check func()
	}

	tests := testy.NewTable[test]()
	tests.AddFunc("should give the client the answer to the re-sent attempt when a displaced attempt's target responds anyway", func(t *testing.T) test {
		const (
			olderMessage     = "the older session asks"
			olderMessage2    = "the older session asks again"
			displacedMessage = "the displaced session asks"
			raced            = "displaced raced answer"
			resent           = "displaced resent answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage != displacedMessage {
				return sseAnswer(inboundMessage), nil
			}
			if received.count(inboundMessage) == 1 {
				// The displaced attempt answers anyway: its cancel lands as
				// the target is already returning a response.
				<-r.Context().Done()
				return sseAnswer(raced), nil
			}
			return sseAnswer(resent), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				// The older session's first turn fixes its rank ahead of the
				// displaced session, which has not arrived yet.
				{inboundMessage: olderMessage, body: admissionTurn(admissionMsg("user", olderMessage))},
				// The displaced session's turn reaches the target directly
				// and waits there headerless past its grace period.
				{inboundMessage: displacedMessage, body: admissionTurn(admissionMsg("user", displacedMessage)), after: time.Second},
				// The older session's second turn outranks it, displacing the
				// headerless attempt.
				{inboundMessage: olderMessage2, body: admissionTurn(
					admissionMsg("user", olderMessage),
					admissionMsg("assistant", "answered"),
					admissionMsg("user", olderMessage2),
				), after: admissionGracePeriod + time.Second},
			},
			wait:     time.Second,
			followed: displacedMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, resent)},
			sends:    2,
		}
	})
	tests.AddFunc("should give the client the second send's answer when the target stays silent past the first-content timeout, which doubles for that send", func(t *testing.T) test {
		const message = "the lone session asks a target that stays silent"
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			if received.count(sentMessage) == 1 {
				// The target answers the first send with a header at once and
				// then sends no content, until the attempt is withdrawn.
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func([]byte) (int, error) {
						<-r.Context().Done()
						return 0, context.Cause(r.Context())
					})),
				}, nil
			}
			// The target answers the next send 90 seconds after receiving
			// it: past the first timeout, within a doubled one.
			time.Sleep(90 * time.Second)
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{{inboundMessage: message, body: admissionTurn(admissionMsg("user", message))}},
			wait:     minFirstContentTimeout + 90*time.Second + time.Second,
			followed: message,
			events:   []string{fmt.Sprintf(`{"id":%q}`, message)},
			sends:    2,
		}
	})
	tests.AddFunc("should wait for a target's first content for twice the longest it has taken before, sending the request again only after that", func(t *testing.T) test {
		const (
			earlierMessage = "an earlier conversation waits fifty seconds for its answer"
			laterMessage   = "a later conversation waits ninety-nine seconds for its answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(_ *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			// The later request's first content is just inside twice the
			// earlier one's, so its send stands.
			if sentMessage == earlierMessage {
				time.Sleep(50 * time.Second)
			} else {
				time.Sleep(99 * time.Second)
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: earlierMessage, body: admissionTurn(admissionMsg("user", earlierMessage))},
				{inboundMessage: laterMessage, body: admissionTurn(admissionMsg("user", laterMessage)), after: 51 * time.Second},
			},
			// Long enough that the later answer arrives even if the send is
			// cut and sent again, so the send count tells the first value
			// apart.
			wait:     165 * time.Second,
			followed: laterMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, laterMessage)},
			sends:    1,
		}
	})
	tests.AddFunc("should wait for a target's first content for twice the longest it has taken before, including a request it answered only after the first-content timeout cut its earlier send", func(t *testing.T) test {
		const (
			earlierMessage = "an earlier conversation's first send is cut, and its second answered in ninety seconds"
			laterMessage   = "a later conversation waits a hundred and seventy seconds for its answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			if sentMessage == earlierMessage && received.count(sentMessage) == 1 {
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			// The later request's first content is just inside twice the
			// earlier request's second send, so its send stands.
			if sentMessage == earlierMessage {
				time.Sleep(90 * time.Second)
			} else {
				time.Sleep(170 * time.Second)
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: earlierMessage, body: admissionTurn(admissionMsg("user", earlierMessage))},
				// Arrives after the earlier request's second send completes,
				// so that send sets the target's first value.
				{inboundMessage: laterMessage, body: admissionTurn(admissionMsg("user", laterMessage)), after: minFirstContentTimeout + 91*time.Second},
			},
			wait:     171 * time.Second,
			followed: laterMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, laterMessage)},
			sends:    1,
		}
	})
	tests.AddFunc("should cut a request's send at the first-content timeout once the target's slow request's first content is more than 10 minutes old", func(t *testing.T) test {
		const (
			slowMessage  = "an earlier conversation waits fifty seconds for its answer"
			laterMessage = "a later conversation waits ninety-nine seconds for its answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			if sentMessage == slowMessage {
				time.Sleep(50 * time.Second)
				return sseAnswer(sentMessage), nil
			}
			// The later request's first content sits inside the doubled
			// 2×50s timeout the slow request's first content still holds
			// up, and past [minFirstContentTimeout] once it stops counting;
			// the doubled re-send of a cut attempt outlasts it.
			select {
			case <-r.Context().Done():
				return nil, context.Cause(r.Context())
			case <-time.After(99 * time.Second):
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: slowMessage, body: admissionTurn(admissionMsg("user", slowMessage))},
				// The slow request's first content arrives fifty seconds
				// after its send; the later request reaches the target one
				// second past [firstContentWindow] after that, so the slow
				// content is just outside the window.
				{inboundMessage: laterMessage, body: admissionTurn(admissionMsg("user", laterMessage)), after: 50*time.Second + 10*time.Minute + time.Second},
			},
			// The first send is cut at [minFirstContentTimeout]; the
			// doubled re-send answers inside it.
			wait:     minFirstContentTimeout + 99*time.Second + time.Second,
			followed: laterMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, laterMessage)},
			sends:    2,
		}
	})
	tests.AddFunc("should keep raising a target's first-content timeout with a slow request's first content while it is less than 10 minutes old", func(t *testing.T) test {
		const (
			slowMessage  = "an earlier conversation waits fifty seconds for its answer"
			laterMessage = "a later conversation waits ninety-nine seconds for its answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(_ *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			if sentMessage == slowMessage {
				time.Sleep(50 * time.Second)
			} else {
				time.Sleep(99 * time.Second)
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: slowMessage, body: admissionTurn(admissionMsg("user", slowMessage))},
				// Still inside [firstContentWindow] aged from the slow
				// request's first content, but past the same window aged
				// from its send, so only the slow request's first content
				// still holds the timeout up.
				{inboundMessage: laterMessage, body: admissionTurn(admissionMsg("user", laterMessage)), after: 50*time.Second + 10*time.Minute - time.Second},
			},
			wait:     99*time.Second + time.Second,
			followed: laterMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, laterMessage)},
			sends:    1,
		}
	})
	tests.AddFunc("should relay the backend's answer to Understudy's second send, and none of the first send's content, when that content arrives just after the first-content timeout cut the first send", func(t *testing.T) test {
		const (
			message = "the lone session's target answers just after the timeout"
			late    = "the timed-out send's late content"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			if received.count(sentMessage) == 1 {
				// First read blocks until the first-content timeout cancels
				// the request context, then delivers content; the next read
				// returns the cancellation, ending the send.
				reads := 0
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						reads++
						if reads == 1 {
							<-r.Context().Done()
							return copy(p, fmt.Sprintf("data: {\"id\":%q}\n\n", late)), nil
						}
						return 0, context.Cause(r.Context())
					})),
				}, nil
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{{inboundMessage: message, body: admissionTurn(admissionMsg("user", message))}},
			wait:     minFirstContentTimeout + time.Second,
			followed: message,
			events:   []string{fmt.Sprintf(`{"id":%q}`, message)},
			sends:    2,
		}
	})
	tests.AddFunc("should relay the backend's answer to Understudy's second send, and none of the first send's content, when that content arrives just after the first-content timeout fired and the backend then ends that send cleanly", func(t *testing.T) test {
		const (
			message = "the lone session's target answers just after the timeout and ends cleanly"
			late    = "the timed-out send's late content"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			if received.count(sentMessage) == 1 {
				// First read blocks until the first-content timeout cancels
				// the request context, then delivers content; the next read
				// ends the body cleanly, before the cancellation reaches it.
				reads := 0
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						reads++
						if reads == 1 {
							<-r.Context().Done()
							return copy(p, fmt.Sprintf("data: {\"id\":%q}\n\n", late)), nil
						}
						return 0, io.EOF
					})),
				}, nil
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{{inboundMessage: message, body: admissionTurn(admissionMsg("user", message))}},
			wait:     minFirstContentTimeout + time.Second,
			followed: message,
			events:   []string{fmt.Sprintf(`{"id":%q}`, message)},
			sends:    2,
		}
	})
	tests.AddFunc("should cut a displaced request's re-sent attempt at the first-content timeout's first value, sending it a third time", func(t *testing.T) test {
		const (
			olderMessage     = "the older session butts in"
			olderMessage2    = "the older session butts in again"
			displacedMessage = "the displaced session asks on"
			thirdAnswer      = "displaced third answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage != displacedMessage {
				return sseAnswer(inboundMessage), nil
			}
			switch received.count(inboundMessage) {
			case 1:
				// The first attempt waits at the target with no header,
				// until the older session's second turn displaces it.
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			case 2:
				// The re-sent attempt answers with a header at once and
				// then sends no content, until the attempt is withdrawn.
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func([]byte) (int, error) {
						<-r.Context().Done()
						return 0, context.Cause(r.Context())
					})),
				}, nil
			}
			return sseAnswer(thirdAnswer), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				// The older session's first turn fixes its rank ahead of the
				// displaced session, which has not arrived yet.
				{inboundMessage: olderMessage, body: admissionTurn(admissionMsg("user", olderMessage))},
				// The displaced session's turn reaches the target directly
				// and waits there headerless past its grace period.
				{inboundMessage: displacedMessage, body: admissionTurn(admissionMsg("user", displacedMessage)), after: time.Second},
				// The older session's second turn outranks it, displacing
				// the headerless attempt; the re-send the displacement
				// causes then waits out the timeout.
				{inboundMessage: olderMessage2, body: admissionTurn(
					admissionMsg("user", olderMessage),
					admissionMsg("assistant", "answered"),
					admissionMsg("user", olderMessage2),
				), after: admissionGracePeriod + time.Second},
			},
			// The re-sent attempt reaches the target at the last arrival
			// and is cut at the timeout's first value, 60s later. Had the
			// displacement doubled the timeout, the third send would still
			// be a minute away at this check.
			wait:     70 * time.Second,
			followed: displacedMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, thirdAnswer)},
			sends:    3,
		}
	})
	tests.AddFunc("should relay a stream past the first-content timeout once its content has begun, without sending the request again", func(t *testing.T) test {
		const message = "the streaming session asks"
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			reads := 0
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
					reads++
					switch reads {
					case 1:
						return copy(p, "data: {\"n\":1}\n\n"), nil
					case 2:
						// The next event comes past the first-content
						// timeout.
						time.Sleep(minFirstContentTimeout + 30*time.Second)
						if err := context.Cause(r.Context()); err != nil {
							return 0, err
						}
						return copy(p, "data: {\"n\":2}\n\n"), nil
					}
					return 0, io.EOF
				})),
			}, nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{{inboundMessage: message, body: admissionTurn(admissionMsg("user", message))}},
			wait:     minFirstContentTimeout + 30*time.Second + time.Second,
			followed: message,
			events:   []string{`{"n":1}`, `{"n":2}`},
			sends:    1,
		}
	})
	tests.AddFunc("should cut a failover's new target's first attempt at the first-content timeout's first value, sending the request again", func(t *testing.T) test {
		const message = "the lone session asks a logical model whose first target refuses it"
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		targetA := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			receivedA.add(sentMessage)
			if receivedA.count(sentMessage) == 1 {
				// The first target sends no content, until the timeout
				// cuts the attempt.
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			// It then refuses the re-sent request, so the walk fails over
			// to the second target.
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"only available hosted in China and requires explicit opt in"}}`)),
			}, nil
		})
		targetB := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			receivedB.add(sentMessage)
			if receivedB.count(sentMessage) == 1 {
				// The second target's first attempt sends no content, until
				// the timeout cuts it at its first value.
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"a": targetA, "b": targetB},
			models: map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}},
			received: receivedB,
			arrivals: []arrival{{inboundMessage: message, body: fmt.Sprintf(`{"model":"m","messages":[%s]}`, admissionMsg("user", message))}},
			// The first target's attempt is cut at 60s and its re-send
			// refused, so the second target receives the request then and is
			// cut at the timeout's first value, 60s later. At 125s the
			// answer has arrived only if that timeout was not doubled.
			wait:     125 * time.Second,
			followed: message,
			events:   []string{fmt.Sprintf(`{"id":%q}`, message)},
			sends:    2,
		}
	})
	tests.AddFunc("should let a first-content timeout end only the send it was started for, when its handler runs after the request was sent again", func(t *testing.T) test {
		const (
			olderMessage     = "the older session asks"
			olderMessage2    = "the older session asks again"
			displacedMessage = "the displaced session asks"
			resentAnswer     = "the second send's answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch {
			case inboundMessage == olderMessage2:
				// Holds the target headerless past the displaced send's timeout
				// firing at t=61s, so the re-send goes out before the late
				// handler runs; its answer then triggers the re-send.
				time.Sleep(58 * time.Second)
				return sseAnswer(inboundMessage), nil
			case inboundMessage != displacedMessage:
				return sseAnswer(inboundMessage), nil
			case received.count(inboundMessage) == 1:
				// The first send waits at the target with no header, until
				// the older session's second turn displaces it.
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			// The second send is answered 30s in, within its own 60s.
			select {
			case <-r.Context().Done():
				return nil, context.Cause(r.Context())
			case <-time.After(30 * time.Second):
				return sseAnswer(resentAnswer), nil
			}
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				// The older session's first turn fixes its rank ahead of the
				// displaced session, which has not arrived yet.
				{inboundMessage: olderMessage, body: admissionTurn(admissionMsg("user", olderMessage))},
				// The displaced session's turn reaches the target at t=1s and
				// waits there with no header; its timeout fires at t=61s.
				{inboundMessage: displacedMessage, body: admissionTurn(admissionMsg("user", displacedMessage)), after: time.Second},
				// The older session's second turn outranks it at t=4s,
				// displacing the headerless send.
				{inboundMessage: olderMessage2, body: admissionTurn(
					admissionMsg("user", olderMessage),
					admissionMsg("assistant", "answered"),
					admissionMsg("user", olderMessage2),
				), after: admissionGracePeriod + time.Second},
			},
			// The displaced send's timer fires at t=61s, but its handler runs
			// only at t=71s — after the request was sent again at t=62s.
			handlerDelay: 10 * time.Second,
			// Had the late handler cut the second send, a third would still
			// be unanswered at this check.
			wait:     90 * time.Second,
			followed: displacedMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, resentAnswer)},
			sends:    2,
		}
	})
	tests.AddFunc("should send each request with no response header at the target again, the older session's first, when the older session's request reaches the first-content timeout/both requests were sent as refills", func(t *testing.T) test {
		const (
			firstStream    = "the first streaming session asks"
			secondStream   = "the second streaming session asks"
			unconfirmed    = "the unconfirmed session asks"
			olderMessage   = "the older session asks"
			youngerMessage = "the younger session asks"
		)
		var (
			secondStreamArrives   = time.Second / 2
			unconfirmedArrives    = time.Second
			olderArrives          = unconfirmedArrives + admissionGracePeriod + time.Second
			youngerArrives        = olderArrives + time.Second/2
			firstStreamEnds       = youngerArrives + 5*time.Second
			secondStreamEnds      = firstStreamEnds + 3*time.Second
			unconfirmedAnsweredAt = secondStreamEnds + 7*time.Second
			olderTimeoutExpires   = firstStreamEnds + minFirstContentTimeout
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			switch {
			case sentMessage == firstStream:
				return admissionStream(r, time.After(firstStreamEnds), nil), nil
			case sentMessage == secondStream:
				return admissionStream(r, time.After(secondStreamEnds-secondStreamArrives), nil), nil
			case sentMessage == unconfirmed:
				// Holds the target's arrivals until both refills are out,
				// then answers and leaves before the timeout expires.
				time.Sleep(unconfirmedAnsweredAt - unconfirmedArrives)
				return sseAnswer(sentMessage), nil
			case received.count(sentMessage) == 1:
				// The refills' first sends wait with no response header
				// until their attempts are withdrawn.
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: firstStream, body: admissionTurn(admissionMsg("user", firstStream))},
				{inboundMessage: secondStream, body: admissionTurn(admissionMsg("user", secondStream)), after: secondStreamArrives},
				// Its request holds the target's arrivals past its grace
				// period, so both sessions wait in the target's queue.
				{inboundMessage: unconfirmed, body: admissionTurn(admissionMsg("user", unconfirmed)), after: unconfirmedArrives - secondStreamArrives},
				{inboundMessage: olderMessage, body: admissionTurn(admissionMsg("user", olderMessage)), after: olderArrives - unconfirmedArrives},
				{inboundMessage: youngerMessage, body: admissionTurn(admissionMsg("user", youngerMessage)), after: youngerArrives - olderArrives},
			},
			// The older request's timeout began at its refill, so this look
			// falls just past its expiry and before the younger request's.
			wait:     olderTimeoutExpires + time.Second - youngerArrives,
			followed: olderMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, olderMessage)},
			sends:    2,
			check: func() {
				// Only a restart at the older request's expiry has sent the
				// younger request a second time by now.
				want := []string{firstStream, secondStream, unconfirmed, olderMessage, youngerMessage, olderMessage, youngerMessage}
				if d := gocmp.Diff(want, received.all()); d != "" {
					t.Errorf("sends that reached the target (-want +got):\n%s", d)
				}
			},
		}
	})
	tests.AddFunc("should keep another session's request with no response header at the target when a request with its response header reaches the first-content timeout/the other request was sent as a refill", func(t *testing.T) test {
		const (
			stream   = "the streaming session asks"
			headered = "the headered session asks"
			refilled = "the refilled session asks"
		)
		var (
			headeredArrives        = time.Second
			refilledArrives        = headeredArrives + admissionGracePeriod + time.Second
			streamEnds             = refilledArrives + 2*time.Second
			headeredTimeoutExpires = headeredArrives + minFirstContentTimeout
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			switch {
			case sentMessage == stream:
				return admissionStream(r, time.After(streamEnds), nil), nil
			case sentMessage == headered && received.count(sentMessage) == 1:
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func([]byte) (int, error) {
						<-r.Context().Done()
						return 0, context.Cause(r.Context())
					})),
				}, nil
			case sentMessage == refilled && received.count(sentMessage) == 1:
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			return sseAnswer(sentMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: stream, body: admissionTurn(admissionMsg("user", stream))},
				{inboundMessage: headered, body: admissionTurn(admissionMsg("user", headered)), after: headeredArrives},
				// The headered request holds the target's arrivals past its
				// grace period, so this one waits until the stream ends.
				{inboundMessage: refilled, body: admissionTurn(admissionMsg("user", refilled)), after: refilledArrives - headeredArrives},
			},
			// Past the headered request's expiry and before the refilled
			// request's, which began at the stream's end.
			wait:     headeredTimeoutExpires + time.Second - refilledArrives,
			followed: stream,
			events:   []string{`{"id":"streamed"}`},
			sends:    1,
			check: func() {
				want := []string{stream, headered, refilled}
				if d := gocmp.Diff(want, received.all()); d != "" {
					t.Errorf("sends that reached the target (-want +got):\n%s", d)
				}
			},
		}
	})
	tests.AddFunc("should send the request waiting behind a displaced request once the displaced request's second send shows content, when another request at the target reached the first-content timeout/the displaced send had not yet ended", func(t *testing.T) test {
		const (
			stream     = "the streaming session asks"
			timedOut   = "the timed-out session asks"
			older      = "the older session asks"
			olderAgain = "the older session asks again"
			displaced  = "the displaced session asks"
			waiting    = "the waiting session asks"
		)
		var (
			timedOutArrives = time.Second
			// Within the grace period, so it is sent at once and ranks
			// between the timed-out and displaced requests.
			olderArrives      = timedOutArrives + admissionGracePeriod/2
			displacedArrives  = timedOutArrives + admissionGracePeriod + time.Second
			streamEnds        = displacedArrives + 2*time.Second
			timedOutExpires   = timedOutArrives + minFirstContentTimeout
			olderAgainArrives = timedOutExpires - time.Second
			waitingArrives    = olderAgainArrives + time.Second/2
			displacedSendEnds = timedOutExpires + time.Second
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, sentMessage string) (*http.Response, error) {
			received.add(sentMessage)
			switch {
			case sentMessage == stream:
				// Its end sends the displaced session's request as a refill.
				return admissionStream(r, time.After(streamEnds), nil), nil
			case sentMessage == older:
				return sseAnswer(sentMessage), nil
			case sentMessage == displaced && received.count(sentMessage) == 1:
				// The displacement cancels this send before the timed-out
				// request's timeout fires, but the send ends after it: the
				// condition under test.
				<-r.Context().Done()
				time.Sleep(displacedSendEnds - olderAgainArrives)
				return nil, context.Cause(r.Context())
			case sentMessage == waiting, sentMessage == timedOut && received.count(sentMessage) == 1:
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			// The remaining sends never finish, so none triggers sending
			// a further request.
			return admissionStream(r, nil, nil), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: stream, body: admissionTurn(admissionMsg("user", stream))},
				{inboundMessage: timedOut, body: admissionTurn(admissionMsg("user", timedOut)), after: timedOutArrives},
				{inboundMessage: older, body: admissionTurn(admissionMsg("user", older)), after: olderArrives - timedOutArrives},
				// The timed-out request holds the target's arrivals past its
				// grace period, so this one waits until the stream ends.
				{inboundMessage: displaced, body: admissionTurn(admissionMsg("user", displaced)), after: displacedArrives - olderArrives},
				// Outranks the displaced session's request, and is outranked
				// by the timed-out one, so it displaces only the first.
				{inboundMessage: olderAgain, body: admissionTurn(
					admissionMsg("user", older),
					admissionMsg("assistant", "answered"),
					admissionMsg("user", olderAgain),
				), after: olderAgainArrives - displacedArrives},
				{inboundMessage: waiting, body: admissionTurn(admissionMsg("user", waiting)), after: waitingArrives - olderAgainArrives},
			},
			wait:     displacedSendEnds + time.Second - waitingArrives,
			followed: older,
			events:   []string{fmt.Sprintf(`{"id":%q}`, older)},
			sends:    1,
			check: func() {
				// The re-sent requests show content and stay open, so the
				// waiting request is sent last, when the displaced
				// request's re-send shows content.
				want := []string{stream, timedOut, older, displaced, timedOut, olderAgain, displaced, waiting}
				if d := gocmp.Diff(want, received.all()); d != "" {
					t.Errorf("sends that reached the target (-want +got):\n%s", d)
				}
			},
		}
	})
	tests.AddFunc("should withdraw an older session's request silent past its grace period and send it again when a younger session's headered request holds the target", func(t *testing.T) test {
		const (
			olderMessage   = "the older session asks a target a younger session holds"
			youngerMessage = "the younger session's request has its header"
			resent         = "the older session's second send's answer"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch {
			case inboundMessage == youngerMessage:
				reads := 0
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						reads++
						if reads > 1 {
							return 0, io.EOF
						}
						// Stays unconfirmed past the older request's grace
						// period, so it holds the target when that expires.
						select {
						case <-r.Context().Done():
							return 0, context.Cause(r.Context())
						case <-time.After(admissionGracePeriod + 3*time.Second):
						}
						return copy(p, fmt.Sprintf("data: {\"id\":%q}\n\n", inboundMessage)), nil
					})),
				}, nil
			case received.count(inboundMessage) == 1:
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			return sseAnswer(resent), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: olderMessage, body: admissionTurn(admissionMsg("user", olderMessage))},
				// Arrives before the older request's grace period ends, so
				// its headered send is unconfirmed at the target when the
				// wait sweep runs.
				{inboundMessage: youngerMessage, body: admissionTurn(admissionMsg("user", youngerMessage)), after: time.Second},
			},
			// Well short of the first-content timeout, so only the wait
			// sweep can have withdrawn the first send.
			wait:     admissionGracePeriod + 5*time.Second,
			followed: olderMessage,
			events:   []string{fmt.Sprintf(`{"id":%q}`, resent)},
			sends:    2,
		}
	})

	tests.AddFunc("should send a request once when it has its header and a newer request at the target goes silent past its grace period", func(t *testing.T) test {
		const (
			headered = "the older session's request has its header"
			silent   = "the newer session's request is silent"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage != headered {
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			// A header at once, and content only well after the newer
			// request's grace period has run out.
			reads := 0
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
					reads++
					if reads > 1 {
						return 0, io.EOF
					}
					select {
					case <-r.Context().Done():
						return 0, context.Cause(r.Context())
					case <-time.After(admissionGracePeriod + 6*time.Second):
					}
					return copy(p, fmt.Sprintf("data: {\"id\":%q}\n\n", headered)), nil
				})),
			}, nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: headered, body: admissionTurn(admissionMsg("user", headered))},
				{inboundMessage: silent, body: admissionTurn(admissionMsg("user", silent)), after: time.Second / 2},
			},
			wait:     admissionGracePeriod + 8*time.Second,
			followed: headered,
			events:   []string{fmt.Sprintf(`{"id":%q}`, headered)},
			sends:    1,
		}
	})
	tests.AddFunc("should send a request once when it reaches the target shortly before another request there goes silent past its grace period", func(t *testing.T) test {
		const (
			oldest = "the oldest session's request is silent"
			middle = "the middle session's request is silent"
			fresh  = "the newest session's request has only just arrived"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage != fresh {
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			// Answers inside its own grace period, after the middle
			// request's has run out.
			time.Sleep(admissionGracePeriod * 3 / 4)
			return sseAnswer(inboundMessage), nil
		})
		return test{
			backends: map[string]*http.Client{"openai": backend},
			received: received,
			arrivals: []arrival{
				{inboundMessage: oldest, body: admissionTurn(admissionMsg("user", oldest))},
				{inboundMessage: middle, body: admissionTurn(admissionMsg("user", middle)), after: time.Second / 2},
				// Arrives before anything at the target is past its grace
				// period, and is still inside its own when the middle
				// request's runs out.
				{inboundMessage: fresh, body: admissionTurn(admissionMsg("user", fresh)), after: time.Second},
			},
			wait:     5 * time.Second,
			followed: fresh,
			events:   []string{fmt.Sprintf(`{"id":%q}`, fresh)},
			sends:    1,
		}
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			srv := newTargetsServer(t, tt.backends, tt.models)
			srv.firstContentSchedule = func(d time.Duration, fire func()) *time.Timer {
				return time.AfterFunc(d, func() {
					time.Sleep(tt.handlerDelay)
					fire()
				})
			}
			poster := newAdmissionPoster(t, srv)

			for _, a := range tt.arrivals {
				time.Sleep(a.after)
				go poster.post(t.Context(), a.inboundMessage, a.body)
				synctest.Wait()
			}
			time.Sleep(tt.wait)
			synctest.Wait()

			rec, ok := poster.answered(tt.followed)
			if !ok {
				t.Fatalf("%q was never answered", tt.followed)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("%q's client got status %d, want %d", tt.followed, rec.Code, http.StatusOK)
			}
			if d := gocmp.Diff(tt.events, sseData(rec.Body.String())); d != "" {
				t.Errorf("events %q's client received (-want +got):\n%s", tt.followed, d)
			}
			if got := tt.received.count(tt.followed); got != tt.sends {
				t.Errorf("the backend received %q %d times, want %d", tt.followed, got, tt.sends)
			}
			if tt.check != nil {
				tt.check()
			}
		})
	})
}

func TestShouldReturnAHeldRequestWhoseClientGoesAway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := newAdmissionTarget(t)

		poster := newAdmissionPoster(t, newAdmissionServer(t, admissionClient(t, target, nil)))
		youngerCtx, clientGoesAway := context.WithCancel(t.Context())
		admissionQueueFormed(t.Context(), youngerCtx, poster)

		clientGoesAway()
		synctest.Wait()

		rec, ok := poster.answered("younger")
		if !ok {
			t.Fatal("the younger session's request was still held after its client went away; a held request does not outlive the client waiting for it")
		}
		if rec.Code == http.StatusOK {
			t.Errorf("the younger session's client got status %d after going away; want an error", rec.Code)
		}

		// A departed client leaves the queue intact for the requests it held
		// up.
		target.endLongGeneration()
		synctest.Wait()
		if _, ok := poster.answered("older"); !ok {
			t.Error("the older session's second turn never completed after the held request's client left")
		}
	})
}

// TestShouldSendTwoHeldRequestsAtOnceWhenTwoStreamingAnswersEndTogether
// verifies that two held requests are both sent the moment two confirmed
// streaming answers end, so both are answered within one answer's time, not
// one after the other.
func TestShouldSendTwoHeldRequestsAtOnceWhenTwoStreamingAnswersEndTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			ends       = 10 * time.Second
			answerTime = 5 * time.Second
		)
		attempts := &targetAttempts{}
		backend := admissionClient(t, nil, func(r *http.Request, message string) (*http.Response, error) {
			attempts.add(message)
			switch message {
			case "first confirmed", "second confirmed":
				// Sends first content immediately (confirming it) and
				// ends at ends.
				return admissionStream(r, attempts.after(ends), nil), nil
			case "unconfirmed":
				// Never answers, keeping the target unconfirmed.
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			// A held request's answer is slow, so serialized admission
			// would show.
			time.Sleep(answerTime)
			return admissionJSON(message), nil
		})
		poster := newAdmissionPoster(t, newAdmissionServer(t, backend))
		start := time.Now()
		post := func(after time.Duration, message string) {
			time.Sleep(after)
			go poster.post(t.Context(), message, admissionTurn(admissionMsg("user", message)))
			synctest.Wait()
		}

		post(0, "first confirmed")
		post(time.Second/2, "second confirmed")
		post(time.Second/2, "unconfirmed")
		// Held: the unconfirmed request is past its grace period.
		post(admissionGracePeriod+time.Second, "first held")
		post(time.Second/2, "second held")
		if _, ok := poster.answered("first held"); ok {
			t.Fatal("setup: the first held request was answered on arrival; the case needs it held")
		}

		time.Sleep(time.Until(start.Add(ends + answerTime + time.Second)))
		synctest.Wait()
		if _, ok := poster.answered("unconfirmed"); ok {
			t.Fatal("setup: the unconfirmed request resolved; the case needs it pending, so only the streams' ends can let the held requests in")
		}
		poster.answeredOK("first held", "second held")
	})
}

// receivedMessages records, in order, the inbound messages of the requests
// that reach a fake backend.
type receivedMessages struct {
	mu       sync.Mutex
	messages []string
}

func (r *receivedMessages) add(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, message)
}

func (r *receivedMessages) count(message string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.messages {
		if a == message {
			n++
		}
	}
	return n
}

func (r *receivedMessages) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.messages)
}

// silentBackend is a fake backend that records every inbound message it
// receives into received and sends no response until the request's client
// goes away, failing with the client's cancellation cause.
func silentBackend(t *testing.T, received *receivedMessages) *http.Client {
	return admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
		received.add(inboundMessage)
		<-r.Context().Done()
		return nil, context.Cause(r.Context())
	})
}

// TestShouldSendOrHoldARequestOnArrival verifies which arriving requests
// Understudy sends to the backend. A request at the backend that has been
// silent past the grace period is the sign the backend is full.
func TestShouldSendOrHoldARequestOnArrival(t *testing.T) {
	type test struct {
		server   *server
		received map[string]*receivedMessages
		arrivals []clientTurn
		// reached is the inbound messages each backend received by the time
		// the last arrival was sent, in order.
		reached map[string][]string
		// secondCheckAt is how long after the first arrival the case checks
		// reachedAtSecondCheck.
		secondCheckAt time.Duration
		// reachedAtSecondCheck is the inbound messages each backend has
		// received by secondCheckAt, in order; nil means no second check.
		reachedAtSecondCheck map[string][]string
		// answered is the status each inbound message's client must have
		// received by the second check; nil means no answer is checked.
		answered map[string]int
		// waited is how long each inbound message's log record must report
		// the request waited for its target, read at the second check;
		// nil means no record is checked.
		waited map[string]time.Duration
	}

	tests := testy.NewTable[test]()
	tests.AddFunc("should send every request of a burst on arrival, while none has been silent at the backend past the grace period", func(t *testing.T) test {
		received := &receivedMessages{}
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": silentBackend(t, received)}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{"the first session asks"}},
				{userMessages: []string{"the second session asks"}, after: admissionGracePeriod / 4},
				{userMessages: []string{"the third session asks"}, after: admissionGracePeriod / 4},
			},
			reached: map[string][]string{"openai": {"the first session asks", "the second session asks", "the third session asks"}},
		}
	})
	tests.AddFunc("should hold a newer session's request arriving while another at the backend has been silent past the grace period", func(t *testing.T) test {
		received := &receivedMessages{}
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": silentBackend(t, received)}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{"the older session asks"}},
				{userMessages: []string{"the newer session asks"}, after: admissionGracePeriod + time.Second},
			},
			reached: map[string][]string{"openai": {"the older session asks"}},
		}
	})
	// A target that admits to its own internal queue sends the header
	// before serving, so the header alone doesn't show the request is
	// served.
	tests.AddFunc("should hold a newer session's request arriving while another has had its header but no content past the grace period", func(t *testing.T) test {
		const headered = "the older session asks"
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage == headered {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func([]byte) (int, error) {
						<-r.Context().Done()
						return 0, context.Cause(r.Context())
					})),
				}, nil
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{headered}},
				{userMessages: []string{"the newer session asks"}, after: admissionGracePeriod + time.Second},
			},
			reached: map[string][]string{"openai": {headered}},
		}
	})
	// The arrival outranks the younger request, but a headered request is
	// never displaced, so the arrival waits behind it: rank decides who is
	// displaced, not who waits.
	tests.AddFunc("should hold an older session's request arriving while a younger one has had its header but no content past the grace period", func(t *testing.T) test {
		const (
			older    = "the older session asks"
			headered = "the younger session asks"
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case older:
				return admissionJSON(inboundMessage), nil
			case headered:
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func([]byte) (int, error) {
						<-r.Context().Done()
						return 0, context.Cause(r.Context())
					})),
				}, nil
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{older}},
				{userMessages: []string{headered}, after: time.Second / 2},
				{userMessages: []string{older, older + " again"}, after: admissionGracePeriod + time.Second},
			},
			reached: map[string][]string{"openai": {older, headered}},
		}
	})
	// Nothing records that a request completed with nobody waiting, so the
	// place it freed is not owed to a later arrival.
	tests.AddFunc("should hold a newer session's request arriving while another at the backend has been silent past the grace period, though a request there completed with nobody waiting", func(t *testing.T) test {
		const completes = "the middle session asks"
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage == completes {
				return admissionJSON(inboundMessage), nil
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{"the oldest session asks"}},
				{userMessages: []string{completes}, after: admissionGracePeriod / 2},
				{userMessages: []string{"the newest session asks"}, after: admissionGracePeriod + time.Second},
			},
			reached: map[string][]string{"openai": {"the oldest session asks", completes}},
		}
	})
	tests.AddFunc("should send a request on arrival again once a queue that formed at the backend has drained", func(t *testing.T) test {
		const slow = "the oldest session asks"
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage == slow {
				// Unanswered past the grace period, so the next
				// arrival is held behind it; its answer drains the
				// queue.
				time.Sleep(10 * time.Second)
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{slow}},
				{userMessages: []string{"the newer session asks"}, after: admissionGracePeriod + time.Second},
				{userMessages: []string{"the latest session asks"}, after: 17 * time.Second},
			},
			reached: map[string][]string{"openai": {slow, "the newer session asks", "the latest session asks"}},
		}
	})

	tests.AddFunc("should send a new session's first turn to the second target when the first has been busy for the failover threshold", func(t *testing.T) test {
		const (
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": silentBackend(t, receivedB),
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			// Nothing arrives at the first target between the end of the
			// other session's grace period and the new session's first turn.
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{occupier}},
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold + time.Second},
			},
			reached: map[string][]string{"a": {occupier}, "b": {newcomer}},
		}
	})
	tests.AddFunc("should hold a new session's first turn arriving at the first target when it has been busy for less than the failover threshold", func(t *testing.T) test {
		const (
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": silentBackend(t, receivedB),
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{occupier}},
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold/2},
			},
			reached: map[string][]string{"a": {occupier}},
		}
	})
	// The new session's turn arrives two seconds before the first target's
	// busy clock crosses the failover threshold, so it is already waiting
	// there when the leave fires.
	tests.AddFunc("should send a new session's first turn waiting at the first target to the second target when the first has been busy for the failover threshold", func(t *testing.T) test {
		const (
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": silentBackend(t, receivedB),
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{occupier}},
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold - 2*time.Second},
			},
			reached:              map[string][]string{"a": {occupier}},
			secondCheckAt:        admissionGracePeriod + defaultFailoverThreshold + time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier}, "b": {newcomer}},
		}
	})
	tests.AddFunc("should keep a new session's first request waiting at the first target when the first has been busy for the failover threshold and the second target is failing past the failover threshold and not yet due a recovery probe", func(t *testing.T) test {
		const (
			demoter  = "the session naming the second target asks"
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		refusing := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedB.add(inboundMessage)
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"only available hosted in China and requires explicit opt in"}}`)),
			}, nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": refusing,
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				// Names the second target's backend and model directly; it shares that
				// target's health key, so its refusal demotes the target.
				{model: "b/mb", userMessages: []string{demoter}},
				{model: "m", userMessages: []string{occupier}, after: time.Second},
				// Arrives while the first target's busy clock is short of the
				// failover threshold, so it is waiting in its line at the
				// crossing.
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold - 2*time.Second},
			},
			reached: map[string][]string{"a": {occupier}, "b": {demoter}},
			// Past the first target's crossing, and short of the demoted
			// target's next recovery probe.
			secondCheckAt:        time.Second + admissionGracePeriod + defaultFailoverThreshold + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier}, "b": {demoter}},
		}
	})
	tests.AddFunc("should send a new session's first request waiting at the first target to the second target when the first has been busy for the failover threshold and the second target is due a recovery probe", func(t *testing.T) test {
		const (
			demoter  = "the session naming the second target asks"
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		refusingDemoter := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedB.add(inboundMessage)
			if inboundMessage != demoter {
				return admissionJSON(inboundMessage), nil
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"only available hosted in China and requires explicit opt in"}}`)),
			}, nil
		})
		// Times are from the demoter's arrival: the second target's recovery
		// probe falls due one recovery interval after its demotion, and the
		// first target's busy clock crosses the failover threshold after
		// that.
		crossing := defaultRecoveryInterval + time.Second
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": refusingDemoter,
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				// Names the second target's backend and model directly; it shares that
				// target's health key, so its refusal demotes the target.
				{model: "b/mb", userMessages: []string{demoter}},
				{model: "m", userMessages: []string{occupier}, after: crossing - admissionGracePeriod - defaultFailoverThreshold},
				// Arrives before the first target's busy clock crosses the
				// failover threshold, so it is waiting in that target's line
				// at the crossing, with the second target already due its
				// probe.
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold - 2*time.Second},
			},
			reached:              map[string][]string{"a": {occupier}, "b": {demoter}},
			secondCheckAt:        crossing + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier}, "b": {demoter, newcomer}},
			answered:             map[string]int{newcomer: http.StatusOK},
		}
	})
	tests.AddFunc("should send a new session's first request waiting at the first target to the second target when the second target becomes due its recovery probe after the first has been busy for the failover threshold", func(t *testing.T) test {
		const (
			demoter  = "the session naming the second target asks"
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		refusingDemoter := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedB.add(inboundMessage)
			if inboundMessage != demoter {
				return admissionJSON(inboundMessage), nil
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"only available hosted in China and requires explicit opt in"}}`)),
			}, nil
		})
		// Times are from the demoter's arrival: the second target's recovery
		// probe falls due one recovery interval after its demotion, and the
		// first target's busy clock crosses the failover threshold before
		// that.
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": refusingDemoter,
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				// Names the second target's backend and model directly; it shares that
				// target's health key, so its refusal demotes the target.
				{model: "b/mb", userMessages: []string{demoter}},
				{model: "m", userMessages: []string{occupier}, after: time.Second},
				// Arrives before the first target's busy clock crosses the
				// failover threshold, so it is waiting in that target's line
				// at the crossing, with the second target's recovery probe
				// still to fall due.
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold - 2*time.Second},
			},
			reached:              map[string][]string{"a": {occupier}, "b": {demoter}},
			secondCheckAt:        defaultRecoveryInterval + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier}, "b": {demoter, newcomer}},
			answered:             map[string]int{newcomer: http.StatusOK},
		}
	})
	tests.AddFunc("should cancel a newer session's request at the target and hold it in line until the oldest session's request there is answered, when both have been silent at the target past their grace period", func(t *testing.T) test {
		const (
			oldest = "the oldest session asks"
			newer  = "the newer session asks"
		)
		received := &receivedMessages{}
		var oldestAnswered atomic.Bool
		oldestAnswers := 2 * (admissionGracePeriod + time.Second)
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage == oldest {
				select {
				case <-r.Context().Done():
					t.Errorf("the target saw %q cancelled", oldest)
					return nil, context.Cause(r.Context())
				case <-time.After(oldestAnswers):
				}
				oldestAnswered.Store(true)
				return admissionJSON(inboundMessage), nil
			}
			if received.count(newer) > 1 && !oldestAnswered.Load() {
				t.Errorf("the target received %q again before %q was answered", newer, oldest)
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{oldest}},
				{userMessages: []string{newer}, after: time.Second / 2},
			},
			reached: map[string][]string{"openai": {oldest, newer}},
			// The newer turn appears twice: its first send ends only in
			// cancellation, and the queue re-sends it when it admits it.
			secondCheckAt:        oldestAnswers + time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {oldest, newer, newer}},
			answered:             map[string]int{oldest: http.StatusOK},
		}
	})
	tests.AddFunc("should cancel a younger session's request that has sent nothing past its grace period and send an older session's later request at once/the older session was quiet for five minutes and four seconds", func(t *testing.T) test {
		const (
			older   = "the older session asks"
			younger = "the younger session asks"
		)
		youngerArrives := 5*time.Minute + time.Second
		olderLaterArrives := admissionGracePeriod + time.Second
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage == younger && received.count(younger) == 1 {
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{older}},
				{userMessages: []string{younger}, after: youngerArrives},
				{userMessages: []string{older, older + " again"}, after: olderLaterArrives},
			},
			reached:              map[string][]string{"openai": {older, younger, older + " again", younger}},
			secondCheckAt:        youngerArrives + olderLaterArrives + time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {older, younger, older + " again", younger}},
			answered:             map[string]int{older + " again": http.StatusOK, younger: http.StatusOK},
		}
	})
	tests.AddFunc("should take a request Understudy has already sent to the target again back into its queue, when an older session's request reaches the target during that send's grace period", func(t *testing.T) test {
		const (
			oldestOpening = "the oldest session asks"
			oldestLater   = "the oldest session asks again"
			middle        = "the middle session asks"
			youngest      = "the youngest session asks"
		)
		// Times are from the first arrival.
		middleArrives := time.Second / 4
		youngestArrives := time.Second / 2
		// When the middle request's content starts, Understudy sends the
		// youngest request to the target a second time; the oldest session's
		// later request arrives within that second send's grace period.
		middleConfirms := youngestArrives + 2*admissionGracePeriod
		oldestLaterArrives := middleConfirms + admissionGracePeriod/4
		oldestLaterAnswers := oldestLaterArrives + 2*admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case oldestOpening:
				return admissionJSON(inboundMessage), nil
			case middle:
				reads := 0
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func(p []byte) (int, error) {
						reads++
						if reads == 1 {
							select {
							case <-r.Context().Done():
								return 0, context.Cause(r.Context())
							case <-time.After(middleConfirms - middleArrives):
							}
							return copy(p, "data: {\"id\":\"streamed\"}\n\n"), nil
						}
						<-r.Context().Done()
						return 0, context.Cause(r.Context())
					})),
				}, nil
			case oldestLater:
				select {
				case <-r.Context().Done():
					return nil, context.Cause(r.Context())
				case <-time.After(oldestLaterAnswers - oldestLaterArrives):
				}
				return admissionJSON(inboundMessage), nil
			case youngest:
				if received.count(youngest) == 3 {
					return admissionJSON(inboundMessage), nil
				}
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{oldestOpening}},
				{userMessages: []string{middle}, after: middleArrives},
				{userMessages: []string{youngest}, after: youngestArrives - middleArrives},
				{userMessages: []string{oldestOpening, oldestLater}, after: oldestLaterArrives - youngestArrives},
			},
			reached:       map[string][]string{"openai": {oldestOpening, middle, youngest, youngest, oldestLater}},
			secondCheckAt: oldestLaterAnswers + time.Second,
			reachedAtSecondCheck: map[string][]string{
				"openai": {oldestOpening, middle, youngest, youngest, oldestLater, youngest},
			},
			answered: map[string]int{youngest: http.StatusOK},
		}
	})
	tests.AddFunc("should send a request again once the target's streaming answer to another request has ended, when the target answered the request with a rate limit and sent no retry-after", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
		)
		// The stream ends well after the refusal is answered, so a second
		// send can only follow the stream's end.
		streamEndsAfter := admissionGracePeriod + 3*time.Second
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					}, nil
				}
				return admissionJSON(inboundMessage), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{streamingMessage}},
				{userMessages: []string{refusedMessage}, after: time.Second},
			},
			// The refusal has been answered and the stream is still open.
			reached: map[string][]string{"openai": {streamingMessage, refusedMessage}},
			// The refused request appears twice: its second send follows the
			// stream's end, and its client receives that send's answer.
			secondCheckAt:        time.Second + streamEndsAfter + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage}},
			answered:             map[string]int{refusedMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should hold a request that a server refused as busy while streaming a request for another of its models, and send it to the server once that stream ends", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
		)
		// The stream ends well after the refusal, so a second send can only
		// follow the stream's end.
		streamEndsAfter := 3 * admissionGracePeriod
		received := &receivedMessages{}
		var streamEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), func() { streamEnded.Store(true) }), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				if !streamEnded.Load() {
					t.Errorf("the server received %q again before %q's stream ended", refusedMessage, streamingMessage)
				}
				return admissionJSON(inboundMessage), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: admissionGracePeriod / 4},
			},
			// The stream is still open, and each request has been sent once.
			reached: map[string][]string{"openai": {streamingMessage, refusedMessage}},
			// The refused request appears twice: its second send follows the
			// stream's end, and its client receives that send's answer.
			secondCheckAt:        admissionGracePeriod/4 + streamEndsAfter + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage}},
			answered:             map[string]int{refusedMessage: http.StatusOK},
		}
	})
	// The refused request's session is seen before the new request's, so it
	// is the one waiting that ranks first among the server's.
	tests.AddFunc("should hold a new request for the model the server is streaming while an older session's request for another model, which the server refused as busy, waits, and once the stream ends send the refused request and then the held new request", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			newMessage       = "the new session asks"
		)
		// The stream ends well after the new request arrives, so the new
		// request's hold, not timing, is what keeps it from the server.
		streamEndsAfter := 3 * admissionGracePeriod
		received := &receivedMessages{}
		var streamEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), func() { streamEnded.Store(true) }), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				if !streamEnded.Load() {
					t.Errorf("the server received %q again before %q's stream ended", refusedMessage, streamingMessage)
				}
				return admissionJSON(inboundMessage), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: admissionGracePeriod / 4},
				{model: "openai/a", userMessages: []string{newMessage}, after: admissionGracePeriod / 4},
			},
			// The stream is still open, the refused request waits, and the
			// new request is held.
			reached: map[string][]string{"openai": {streamingMessage, refusedMessage}},
			// The refused request appears twice: its second send follows the
			// stream's end, and its client receives that send's answer. The
			// held new request is sent after that second send, and its
			// client receives an answer too.
			secondCheckAt:        streamEndsAfter + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage, newMessage}},
			answered:             map[string]int{refusedMessage: http.StatusOK, newMessage: http.StatusOK},
		}
	})
	// The older session's first request is answered before the refusal,
	// so the session ranks before the refused one.
	tests.AddFunc("should send an older session's later request for the model the server is streaming at once, and answer its client before the stream ends/a younger session's request for another model, which the server refused as busy, waits", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			olderMessage     = "the older session asks"
			refusedMessage   = "the refused session asks"
		)
		// Times are from the first arrival.
		olderArrives := admissionGracePeriod / 4
		refusedArrives := olderArrives + admissionGracePeriod/4
		olderLaterArrives := refusedArrives + admissionGracePeriod/4
		// The stream outlives every arrival, so its end cannot be what
		// sends the older session's later request.
		streamEndsAfter := 3 * admissionGracePeriod
		// Past the later request's arrival and short of the stream's end,
		// so an answer the check sees reached its client while the stream
		// was open.
		checkAt := olderLaterArrives + admissionGracePeriod/4
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case refusedMessage:
				return admissionServerBusy(), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/a", userMessages: []string{olderMessage}, after: olderArrives},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: refusedArrives - olderArrives},
				{model: "openai/a", userMessages: []string{olderMessage, olderMessage + " again"}, after: olderLaterArrives - refusedArrives},
			},
			// The stream is still open and the refused request waits, yet
			// the server has taken the older session's later request.
			reached: map[string][]string{"openai": {streamingMessage, olderMessage, refusedMessage, olderMessage + " again"}},
			// Nothing more reaches the server while the stream runs, and
			// the older session's later request is answered in that
			// window.
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, olderMessage, refusedMessage, olderMessage + " again"}},
			answered:             map[string]int{olderMessage + " again": http.StatusOK},
		}
	})
	// The streaming session is first seen, then the oldest, the second
	// refused, the middle, then the first refused, so the middle ranks
	// between the two refused sessions.
	tests.AddFunc("should hold a session's later request for the model the server is streaming while an older session's request for another model, which the server refused as busy, waits, and send a later request from a session older than both at once/a younger session's request for another model was refused first", func(t *testing.T) test {
		const (
			streamingMessage     = "the streaming session asks"
			oldestMessage        = "the oldest session asks"
			secondRefusedMessage = "the second refused session asks"
			middleMessage        = "the middle session asks"
			firstRefusedMessage  = "the first refused session asks"
		)
		gap := admissionGracePeriod / 4
		// The stream outlives every arrival, so its end cannot be what sends
		// or holds any later request.
		streamEndsAfter := 3 * admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case firstRefusedMessage, secondRefusedMessage + " again":
				return admissionServerBusy(), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		reached := []string{
			streamingMessage, oldestMessage, secondRefusedMessage, middleMessage,
			firstRefusedMessage, secondRefusedMessage + " again", oldestMessage + " again",
		}
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/a", userMessages: []string{oldestMessage}, after: gap},
				{model: "openai/a", userMessages: []string{secondRefusedMessage}, after: gap},
				{model: "openai/a", userMessages: []string{middleMessage}, after: gap},
				{model: "openai/b", userMessages: []string{firstRefusedMessage}, after: gap},
				{model: "openai/c", userMessages: []string{secondRefusedMessage, secondRefusedMessage + " again"}, after: gap},
				{model: "openai/a", userMessages: []string{oldestMessage, oldestMessage + " again"}, after: gap},
				{model: "openai/a", userMessages: []string{middleMessage, middleMessage + " again"}, after: gap},
			},
			// The stream is still open at the first check.
			reached:              map[string][]string{"openai": reached},
			secondCheckAt:        8 * gap,
			reachedAtSecondCheck: map[string][]string{"openai": reached},
			answered:             map[string]int{oldestMessage + " again": http.StatusOK},
		}
	})
	// The refused session is first seen before the younger session's request
	// arrives, so it outranks that request.
	tests.AddFunc("should keep a younger session's request for the model the server is streaming held when the stream's first content arrives, and send it after the request the server refused as busy/the younger request was waiting when the refusal came", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			youngerMessage   = "the younger session asks"
		)
		refusedFirstArrives := admissionGracePeriod / 4
		// Past the stream's grace period, so the younger request waits
		// behind the stream, which has sent no content yet.
		youngerArrives := admissionGracePeriod + admissionGracePeriod/4
		refusedLaterArrives := youngerArrives + admissionGracePeriod/4
		contentAfter := 2 * admissionGracePeriod
		streamEndsAfter := 4 * admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStreamContentAt(r, time.After(contentAfter), time.After(streamEndsAfter)), nil
			case refusedMessage + " again":
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/a", userMessages: []string{refusedMessage}, after: refusedFirstArrives},
				{model: "openai/a", userMessages: []string{youngerMessage}, after: youngerArrives - refusedFirstArrives},
				{model: "openai/b", userMessages: []string{refusedMessage, refusedMessage + " again"}, after: refusedLaterArrives - youngerArrives},
			},
			reached:              map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage + " again"}},
			secondCheckAt:        streamEndsAfter + admissionGracePeriod,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage + " again", refusedMessage + " again", youngerMessage}},
			answered:             map[string]int{refusedMessage + " again": http.StatusOK, youngerMessage: http.StatusOK},
		}
	})
	// The older session's first request completes before the refused
	// session's first arrives, so the older session outranks the refused
	// one.
	tests.AddFunc("should send an older session's request for the model the server is streaming once the stream's first content arrives, and answer its client before the stream ends/the request was waiting when the server refused a younger session's request for another model as busy", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			olderMessage     = "the older session asks"
			refusedMessage   = "the refused session asks"
		)
		olderFirstArrives := admissionGracePeriod / 4
		refusedFirstArrives := olderFirstArrives + admissionGracePeriod/4
		// Past the stream's grace period, so the older session's later
		// request waits behind the stream, which has sent no content yet.
		olderLaterArrives := admissionGracePeriod + admissionGracePeriod/4
		refusedLaterArrives := olderLaterArrives + admissionGracePeriod/4
		contentAfter := 2 * admissionGracePeriod
		streamEndsAfter := 4 * admissionGracePeriod
		// Past the first content and short of the stream's end, so what the
		// check sees reached the server, and its answer reached its client,
		// while the stream was open.
		checkAt := contentAfter + admissionGracePeriod/4
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStreamContentAt(r, time.After(contentAfter), time.After(streamEndsAfter)), nil
			case refusedMessage + " again":
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/a", userMessages: []string{olderMessage}, after: olderFirstArrives},
				{model: "openai/a", userMessages: []string{refusedMessage}, after: refusedFirstArrives - olderFirstArrives},
				{model: "openai/a", userMessages: []string{olderMessage, olderMessage + " again"}, after: olderLaterArrives - refusedFirstArrives},
				{model: "openai/b", userMessages: []string{refusedMessage, refusedMessage + " again"}, after: refusedLaterArrives - olderLaterArrives},
			},
			// The stream is still open, the refused request waits, and the
			// older session's later request has not reached the server.
			reached: map[string][]string{"openai": {streamingMessage, olderMessage, refusedMessage, refusedMessage + " again"}},
			// The first content has arrived and the stream is still open:
			// the server has taken the older session's later request, the
			// refused request only once, and the older session's client has
			// its answer.
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, olderMessage, refusedMessage, refusedMessage + " again", olderMessage + " again"}},
			answered:             map[string]int{olderMessage + " again": http.StatusOK},
		}
	})
	tests.AddFunc("should hold a new request for the model the server is streaming while an older session's request for another model, which the server refused as busy, waits, and once the stream ends send the refused request and then the held new request/the server answers the refused request's second send with an error naming the request", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			newMessage       = "the new session asks"
		)
		// The stream ends well after the new request arrives, so the new
		// request's hold, not timing, is what keeps it from the server.
		streamEndsAfter := 3 * admissionGracePeriod
		received := &receivedMessages{}
		var streamEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), func() { streamEnded.Store(true) }), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				if !streamEnded.Load() {
					t.Errorf("the server received %q again before %q's stream ended", refusedMessage, streamingMessage)
				}
				// The error is about the request itself, not the
				// server's being busy, so it ends the request rather
				// than re-queueing it behind the busy refusal.
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, inboundMessage+" is not a request the server serves"))),
				}, nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: admissionGracePeriod / 4},
				{model: "openai/a", userMessages: []string{newMessage}, after: admissionGracePeriod / 4},
			},
			// The stream is still open, the refused request waits, and the
			// new request is held.
			reached: map[string][]string{"openai": {streamingMessage, refusedMessage}},
			// The refused request appears twice: its second send follows
			// the stream's end, and its client receives that send's error.
			// The held new request is sent after that second send, and its
			// client receives an answer too.
			secondCheckAt:        streamEndsAfter + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage, newMessage}},
			answered:             map[string]int{newMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should hold a new request for the model the server is streaming while an older session's request for another model, which the server refused as busy, waits, and once the stream ends send the refused request and then the held new request/the server refuses the refused request's second send as busy again with nothing running on it", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			newMessage       = "the new session asks"
		)
		// The stream ends well after the new request arrives, so the hold,
		// not the stream's end, is what keeps the new request from the
		// server.
		streamEndsAfter := 3 * admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case refusedMessage:
				return admissionServerBusy(), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: admissionGracePeriod / 4},
				{model: "openai/a", userMessages: []string{newMessage}, after: admissionGracePeriod / 4},
			},
			reached:              map[string][]string{"openai": {streamingMessage, refusedMessage}},
			secondCheckAt:        streamEndsAfter + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage, newMessage}},
			answered:             map[string]int{newMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should send the held new request for the model the server is streaming and answer its client before the stream ends/the request the server refused as busy waits, and its client leaves", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			newMessage       = "the new session asks"
		)
		// Times are from the first arrival.
		refusedArrives := admissionGracePeriod / 4
		newArrives := refusedArrives + admissionGracePeriod/4
		// The client leaves after the new request is held.
		clientLeaves := admissionGracePeriod
		streamEndsAfter := 3 * admissionGracePeriod
		// Past the departure and short of the stream's end, so the
		// departure, not the stream's end, is what can have sent the held
		// request.
		checkAt := clientLeaves + admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case refusedMessage:
				return admissionServerBusy(), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: refusedArrives, clientLeavesAfter: clientLeaves - refusedArrives},
				{model: "openai/a", userMessages: []string{newMessage}, after: newArrives - refusedArrives},
			},
			// The stream is still open, the refused request waits, and the
			// new request is held.
			reached:              map[string][]string{"openai": {streamingMessage, refusedMessage}},
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, newMessage}},
			answered:             map[string]int{newMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should send the held new request and answer its client before the stream ends/an older session's request for the same model, which the server refused as busy, waits, and its client leaves", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			newMessage       = "the new session asks"
		)
		refusedArrives := admissionGracePeriod / 4
		newArrives := refusedArrives + admissionGracePeriod/4
		clientLeaves := admissionGracePeriod
		streamEndsAfter := 3 * admissionGracePeriod
		// Past the departure and short of the stream's end, so the
		// departure, not the stream's end, is what can have sent the held
		// request.
		checkAt := clientLeaves + admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case refusedMessage:
				return admissionServerBusy(), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: refusedArrives, clientLeavesAfter: clientLeaves - refusedArrives},
				{model: "openai/b", userMessages: []string{newMessage}, after: newArrives - refusedArrives},
			},
			reached:              map[string][]string{"openai": {streamingMessage, refusedMessage}},
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, newMessage}},
			answered:             map[string]int{newMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should send the newly refused request to the server again once the stream ends, and keep a younger session's request for the first refused model held behind an unanswered request for that model/the first refused request's client left while it waited, and an unanswered request for its model is at the server", func(t *testing.T) test {
		const (
			streamingMessage    = "the streaming session asks"
			firstRefusedMessage = "the first refused session asks"
			olderMessage        = "the older session asks"
			youngerMessage      = "the younger session asks"
			newlyRefusedMessage = "the newly refused session asks"
		)
		// Times are from the first arrival.
		firstRefusedArrives := admissionGracePeriod / 4
		firstRefusedLeaves := firstRefusedArrives + admissionGracePeriod/2
		// The older session's request arrives after the first refused
		// request's client left and its model's queue has held nothing
		// since, so it is sent on arrival; the younger session's request
		// arrives past its grace period without an answer, so their model's
		// own admission rule holds it.
		olderArrives := firstRefusedLeaves + admissionGracePeriod/4
		youngerArrives := olderArrives + admissionGracePeriod + admissionGracePeriod/4
		newlyRefusedArrives := youngerArrives + admissionGracePeriod/4
		// The stream outlives every arrival, so only its end can send the
		// newly refused request again.
		streamEndsAfter := 3 * admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case firstRefusedMessage:
				return admissionServerBusy(), nil
			case newlyRefusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				return admissionJSON(inboundMessage), nil
			}
			// The older and younger sessions' requests are never answered
			// during the test.
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{firstRefusedMessage}, after: firstRefusedArrives, clientLeavesAfter: firstRefusedLeaves - firstRefusedArrives},
				{model: "openai/b", userMessages: []string{olderMessage}, after: olderArrives - firstRefusedArrives},
				{model: "openai/b", userMessages: []string{youngerMessage}, after: youngerArrives - olderArrives},
				{model: "openai/c", userMessages: []string{newlyRefusedMessage}, after: newlyRefusedArrives - youngerArrives},
			},
			reached: map[string][]string{"openai": {streamingMessage, firstRefusedMessage, olderMessage, newlyRefusedMessage}},
			// The newly refused request's second send follows the stream's
			// end, and its answer reaches its client.
			secondCheckAt:        streamEndsAfter + admissionGracePeriod/2,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, firstRefusedMessage, olderMessage, newlyRefusedMessage, newlyRefusedMessage}},
			answered:             map[string]int{newlyRefusedMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should hold a new request for the model the server is streaming while an older session's request for another model, which the server refused as busy, waits, and once the stream ends send the refused request and then the held new request/a request for a model with nothing running that the server answers while the refused request waits", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			newMessage       = "the new session asks"
			answeredMessage  = "the answered session asks"
		)
		// The stream ends well after the answered request arrives, so the
		// new request's hold, not timing, is what keeps it from the server.
		streamEndsAfter := 3 * admissionGracePeriod
		received := &receivedMessages{}
		var streamEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), func() { streamEnded.Store(true) }), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				if !streamEnded.Load() {
					t.Errorf("the server received %q again before %q's stream ended", refusedMessage, streamingMessage)
				}
				return admissionJSON(inboundMessage), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: admissionGracePeriod / 4},
				{model: "openai/a", userMessages: []string{newMessage}, after: admissionGracePeriod / 4},
				{model: "openai/c", userMessages: []string{answeredMessage}, after: admissionGracePeriod / 4},
			},
			// The stream is still open, the refused request waits, the new
			// request is held, and the server has answered the request for
			// the model with nothing running.
			reached: map[string][]string{"openai": {streamingMessage, refusedMessage, answeredMessage}},
			// The refused request appears twice: its second send follows the
			// stream's end.
			secondCheckAt:        streamEndsAfter + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, answeredMessage, refusedMessage, newMessage}},
			answered:             map[string]int{refusedMessage: http.StatusOK, newMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should hold a new request for the model the server is streaming while an older session's request for another model, which the server refused as busy, waits, and once the stream ends send the refused request and then the held new request/a request for a model with nothing running that the server answers while the refused request's second send is unanswered", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			newMessage       = "the new session asks"
			answeredMessage  = "the answered session asks"
		)
		// Times are from the first arrival.
		refusedArrives := admissionGracePeriod / 4
		newArrives := refusedArrives + admissionGracePeriod/4
		streamEndsAfter := 3 * admissionGracePeriod
		// The request for the model with nothing running arrives once the
		// stream's end has sent the refused request to the server again.
		answeredArrives := streamEndsAfter + admissionGracePeriod/4
		// The server's answer to that second send comes after the request
		// for the model with nothing running has been answered.
		refusedSecondSendAnswerDelay := admissionGracePeriod
		received := &receivedMessages{}
		var streamEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), func() { streamEnded.Store(true) }), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				if !streamEnded.Load() {
					t.Errorf("the server received %q again before %q's stream ended", refusedMessage, streamingMessage)
				}
				select {
				case <-r.Context().Done():
					return nil, context.Cause(r.Context())
				case <-time.After(refusedSecondSendAnswerDelay):
				}
				return admissionJSON(inboundMessage), nil
			case answeredMessage:
				if received.count(refusedMessage) < 2 {
					t.Errorf("the server answered %q before receiving %q's second send", answeredMessage, refusedMessage)
				}
				return admissionJSON(inboundMessage), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{streamingMessage}},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: refusedArrives},
				{model: "openai/a", userMessages: []string{newMessage}, after: newArrives - refusedArrives},
				{model: "openai/c", userMessages: []string{answeredMessage}, after: answeredArrives - newArrives},
			},
			// The refused request's second send is still unanswered, so the
			// new request is still held.
			reached: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage, answeredMessage}},
			// The held new request is sent once the server takes the
			// refused request's second send.
			secondCheckAt:        answeredArrives + refusedSecondSendAnswerDelay + admissionGracePeriod/2,
			reachedAtSecondCheck: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage, answeredMessage, newMessage}},
			answered:             map[string]int{refusedMessage: http.StatusOK, answeredMessage: http.StatusOK, newMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should send a request that a server refused as busy to the server again once one of two requests streaming there has ended, and answer the request's client while the other still streams", func(t *testing.T) test {
		const (
			firstStreamingMessage  = "the first streaming session asks"
			secondStreamingMessage = "the second streaming session asks"
			refusedMessage         = "the refused session asks"
		)
		// Times are measured from the first arrival, not from the previous
		// turn.
		secondArrives := admissionGracePeriod / 2
		refusedArrives := secondArrives + admissionGracePeriod/2
		firstEnds := refusedArrives + 2*admissionGracePeriod
		checkAt := firstEnds + admissionGracePeriod/2
		// The second stream must still be running at the check.
		secondEnds := checkAt + 2*admissionGracePeriod
		received := &receivedMessages{}
		var firstEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case firstStreamingMessage:
				return admissionStream(r, time.After(firstEnds), func() { firstEnded.Store(true) }), nil
			case secondStreamingMessage:
				return admissionStream(r, time.After(secondEnds-secondArrives), nil), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				if !firstEnded.Load() {
					t.Errorf("the server received %q again before %q's stream ended", refusedMessage, firstStreamingMessage)
				}
				return admissionJSON(inboundMessage), nil
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{firstStreamingMessage}},
				{model: "openai/c", userMessages: []string{secondStreamingMessage}, after: secondArrives},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: refusedArrives - secondArrives},
			},
			reached:              map[string][]string{"openai": {firstStreamingMessage, secondStreamingMessage, refusedMessage}},
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"openai": {firstStreamingMessage, secondStreamingMessage, refusedMessage, refusedMessage}},
			answered:             map[string]int{refusedMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should send the refused request and then the held new request once both of the server's streams have ended/the server refuses the request as busy again when the first stream ends", func(t *testing.T) test {
		const (
			firstStreamingMessage  = "the first streaming session asks"
			secondStreamingMessage = "the second streaming session asks"
			refusedMessage         = "the refused session asks"
			newMessage             = "the new session asks"
		)
		// Times are measured from the first arrival, not from the previous
		// request.
		secondArrives := admissionGracePeriod / 2
		refusedArrives := admissionGracePeriod
		firstEnds := refusedArrives + admissionGracePeriod + admissionGracePeriod/2
		// The new request arrives after the server's second refusal, so
		// only the refused request's wait can hold it.
		newArrives := firstEnds + admissionGracePeriod/2
		secondEnds := newArrives + 2*admissionGracePeriod
		checkAt := secondEnds + admissionGracePeriod/2
		received := &receivedMessages{}
		var secondEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case firstStreamingMessage:
				return admissionStream(r, time.After(firstEnds), nil), nil
			case secondStreamingMessage:
				return admissionStream(r, time.After(secondEnds-secondArrives), func() { secondEnded.Store(true) }), nil
			case refusedMessage:
				if received.count(inboundMessage) <= 2 {
					return admissionServerBusy(), nil
				}
				if !secondEnded.Load() {
					t.Errorf("the server received %q a third time before %q's stream ended", refusedMessage, secondStreamingMessage)
				}
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{firstStreamingMessage}},
				{model: "openai/c", userMessages: []string{secondStreamingMessage}, after: secondArrives},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: refusedArrives - secondArrives},
				{model: "openai/a", userMessages: []string{newMessage}, after: newArrives - refusedArrives},
			},
			reached:              map[string][]string{"openai": {firstStreamingMessage, secondStreamingMessage, refusedMessage, refusedMessage}},
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"openai": {firstStreamingMessage, secondStreamingMessage, refusedMessage, refusedMessage, refusedMessage, newMessage}},
			answered:             map[string]int{refusedMessage: http.StatusOK, newMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should hold a second request for a model when a request for another of the server's models ends, once the server refused the model's first request as busy and then accepted it", func(t *testing.T) test {
		const (
			firstStreamingMessage  = "the first streaming session asks"
			secondStreamingMessage = "the second streaming session asks"
			refusedMessage         = "the refused session asks"
			waitingMessage         = "the waiting session asks"
		)
		// Times are measured from the first arrival, not from the previous
		// request.
		secondArrives := admissionGracePeriod / 2
		refusedArrives := admissionGracePeriod
		firstEnds := refusedArrives + admissionGracePeriod + admissionGracePeriod/2
		// The re-sent request has been at the server with no answer past its
		// grace period by then, so the model's own admission rule holds the
		// waiting request.
		waitingArrives := firstEnds + admissionGracePeriod + admissionGracePeriod/2
		secondEnds := waitingArrives + admissionGracePeriod/2
		checkAt := secondEnds + admissionGracePeriod/2
		received := &receivedMessages{}
		var firstEnded atomic.Bool
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case firstStreamingMessage:
				return admissionStream(r, time.After(firstEnds), func() { firstEnded.Store(true) }), nil
			case secondStreamingMessage:
				return admissionStream(r, time.After(secondEnds-secondArrives), nil), nil
			case refusedMessage:
				if received.count(inboundMessage) == 1 {
					return admissionServerBusy(), nil
				}
				if !firstEnded.Load() {
					t.Errorf("the server received %q again before %q's stream ended", refusedMessage, firstStreamingMessage)
				}
			case waitingMessage:
			}
			// Both the accepted re-send and the waiting request stay unanswered
			// through the check.
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "openai/a", userMessages: []string{firstStreamingMessage}},
				{model: "openai/c", userMessages: []string{secondStreamingMessage}, after: secondArrives},
				{model: "openai/b", userMessages: []string{refusedMessage}, after: refusedArrives - secondArrives},
				{model: "openai/b", userMessages: []string{waitingMessage}, after: waitingArrives - refusedArrives},
			},
			reached:              map[string][]string{"openai": {firstStreamingMessage, secondStreamingMessage, refusedMessage, refusedMessage}},
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"openai": {firstStreamingMessage, secondStreamingMessage, refusedMessage, refusedMessage}},
		}
	})
	tests.AddFunc("should answer a request the server refused as busy with a rate limit, sending it to the server once/the only request running at the server's address streams under another API key", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
		)
		refusedArrives := admissionGracePeriod / 4
		streamEndsAfter := 3 * admissionGracePeriod
		// Past the stream's end, so a request held for that stream would
		// have reached the server again by then.
		checkAt := streamEndsAfter + admissionGracePeriod/2
		receivedFirst, receivedSecond := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedFirst.add(inboundMessage)
			return admissionStream(r, time.After(streamEndsAfter), nil), nil
		})
		second := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedSecond.add(inboundMessage)
			return admissionServerBusy(), nil
		})
		return test{
			server: newBackendsServer(t, map[string]Backend{
				"first": targetBackend("first", "first", first),
				// The second backend shares the first's address under its
				// own API key.
				"second": targetBackend("second", "first", second),
			}, nil),
			received: map[string]*receivedMessages{"first": receivedFirst, "second": receivedSecond},
			arrivals: []clientTurn{
				{model: "first/a", userMessages: []string{streamingMessage}},
				{model: "second/b", userMessages: []string{refusedMessage}, after: refusedArrives},
			},
			reached:              map[string][]string{"first": {streamingMessage}, "second": {refusedMessage}},
			secondCheckAt:        checkAt,
			reachedAtSecondCheck: map[string][]string{"first": {streamingMessage}, "second": {refusedMessage}},
			answered:             map[string]int{refusedMessage: http.StatusTooManyRequests},
		}
	})
	tests.AddFunc("should send a session's first request to the second target when the first target has been busy for the failover threshold and answers the request with a rate limit and no retry-after", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			holderOpening    = "the holding session asks"
			holderLater      = "the holding session asks again"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		// Times are from the first arrival. The holding session is seen after
		// the refused one, and its later request arrives within the refused
		// request's grace period, so the queue-forming sweep leaves the
		// refused request — the older session's — at the target as the one
		// request it keeps there, and takes the holding one into the line.
		refusedArrives := time.Second
		holderArrives := refusedArrives + time.Second/2
		holderLaterArrives := holderArrives + time.Second/4
		// The first target answers the refused request past the crossing its
		// holding of arrivals reaches, counted from the holding request's
		// grace period — the latest clock the target runs, so the one the
		// refusal must clear.
		refuseAt := holderLaterArrives + admissionGracePeriod + defaultFailoverThreshold + time.Second
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, nil, nil), nil
			case refusedMessage:
				if receivedA.count(inboundMessage) == 1 {
					select {
					case <-r.Context().Done():
						return nil, context.Cause(r.Context())
					case <-time.After(refuseAt - refusedArrives):
					}
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					}, nil
				}
				return admissionJSON(inboundMessage), nil
			case holderOpening:
				return admissionJSON(inboundMessage), nil
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		second := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedB.add(inboundMessage)
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": second}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{streamingMessage}},
				{model: "m", userMessages: []string{refusedMessage}, after: refusedArrives},
				{model: "m", userMessages: []string{holderOpening}, after: holderArrives - refusedArrives},
				{model: "m", userMessages: []string{holderOpening, holderLater}, after: holderLaterArrives - holderArrives},
			},
			reached: map[string][]string{"a": {streamingMessage, refusedMessage, holderOpening, holderLater}},
			// The holding session's later request is sent again once the
			// refused request's send ends, freeing the line's head; the
			// refused request itself lands at the second target, and its
			// client receives that target's answer.
			secondCheckAt: refuseAt + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{
				"a": {streamingMessage, refusedMessage, holderOpening, holderLater, holderLater},
				"b": {refusedMessage},
			},
			answered: map[string]int{refusedMessage: http.StatusOK},
		}
	})
	tests.AddFunc("should send a session's later request to its target again once the target's stream ends, when the target has been busy for the failover threshold and answers it with a rate limit and no retry-after", func(t *testing.T) test {
		const (
			opening          = "the bound session asks"
			later            = "the bound session asks again"
			streamingMessage = "the streaming session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		// Times are relative to the first arrival. The first target stays
		// busy past the failover threshold before refusing the later
		// request; the stream ends after the refusal.
		laterArrives := time.Second
		refuseAt := laterArrives + admissionGracePeriod + defaultFailoverThreshold + time.Second
		streamEnds := refuseAt + 2*time.Second
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			switch inboundMessage {
			case streamingMessage:
				return admissionStream(r, time.After(streamEnds-laterArrives/2), nil), nil
			case later:
				if receivedA.count(inboundMessage) == 1 {
					select {
					case <-r.Context().Done():
						return nil, context.Cause(r.Context())
					case <-time.After(refuseAt - laterArrives):
					}
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
					}, nil
				}
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": silentBackend(t, receivedB)}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{opening}},
				{model: "m", userMessages: []string{streamingMessage}, after: laterArrives / 2},
				{model: "m", userMessages: []string{opening, later}, after: laterArrives / 2},
			},
			reached:              map[string][]string{"a": {opening, streamingMessage, later}},
			secondCheckAt:        streamEnds + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {opening, streamingMessage, later, later}},
			answered:             map[string]int{later: http.StatusOK},
		}
	})
	tests.AddFunc("should keep sending requests to a target up to its earlier concurrency cap after a bare 429 sent a request there back into line", func(t *testing.T) test {
		const (
			streamingMessage = "the streaming session asks"
			refusedMessage   = "the refused session asks"
			firstNewcomer    = "the first new session asks"
			secondNewcomer   = "the second new session asks"
		)
		streamEndsAfter := admissionGracePeriod
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch {
			case inboundMessage == streamingMessage:
				return admissionStream(r, time.After(streamEndsAfter), nil), nil
			case inboundMessage == refusedMessage && received.count(inboundMessage) == 1:
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
				}, nil
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{streamingMessage}},
				{userMessages: []string{refusedMessage}, after: admissionGracePeriod / 4},
				// Sent while the re-sent refused request's grace period is
				// still open. The bare 429 left the cap at its earlier value,
				// so both are admitted.
				{userMessages: []string{firstNewcomer}, after: streamEndsAfter + admissionGracePeriod/4},
				{userMessages: []string{secondNewcomer}, after: admissionGracePeriod / 8},
			},
			reached: map[string][]string{"openai": {streamingMessage, refusedMessage, refusedMessage, firstNewcomer, secondNewcomer}},
		}
	})
	tests.AddFunc("should send a new session's first request waiting at a busy first target to the failing target whose recovery probe falls due first", func(t *testing.T) test {
		const (
			secondDemoter = "the session naming the second target asks"
			thirdDemoter  = "the session naming the third target asks"
			occupier      = "the other session asks"
			newcomer      = "the new session asks"
		)
		refused := `{"error":{"message":"only available hosted in China and requires explicit opt in"}}`
		refusingDemoter := func(received *receivedMessages, demoter string) *http.Client {
			return admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
				received.add(inboundMessage)
				if inboundMessage != demoter {
					return admissionJSON(inboundMessage), nil
				}
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(refused)),
				}, nil
			})
		}
		receivedA, receivedB, receivedC := &receivedMessages{}, &receivedMessages{}, &receivedMessages{}
		// Times are from the first arrival. The first target crosses the
		// failover threshold before either failing target is due its probe.
		crossing := defaultRecoveryInterval - 2*time.Second
		occupierArrives := crossing - admissionGracePeriod - defaultFailoverThreshold
		thirdDemoted := occupierArrives - time.Second
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": refusingDemoter(receivedB, secondDemoter),
				"c": refusingDemoter(receivedC, thirdDemoter),
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
				{backend: "c", model: "mc"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB, "c": receivedC},
			arrivals: []clientTurn{
				// Each names a target's backend and model directly; it shares
				// that target's health key, so its refusal demotes the target.
				{model: "b/mb", userMessages: []string{secondDemoter}},
				{model: "c/mc", userMessages: []string{thirdDemoter}, after: thirdDemoted},
				{model: "m", userMessages: []string{occupier}, after: occupierArrives - thirdDemoted},
				{model: "m", userMessages: []string{newcomer}, after: crossing - occupierArrives - 2*time.Second},
			},
			reached: map[string][]string{"a": {occupier}, "b": {secondDemoter}, "c": {thirdDemoter}},
			// After the second target's probe falls due, before the third's.
			secondCheckAt:        defaultRecoveryInterval + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier}, "b": {secondDemoter, newcomer}, "c": {thirdDemoter}},
			answered:             map[string]int{newcomer: http.StatusOK},
		}
	})
	// The second target's request arrives first, so the second target is
	// already unavailable for new sessions when the first target crosses the
	// failover threshold.
	tests.AddFunc("should send a new session's first request waiting at the first target there once the first target is no longer busy/the second target was busy when the first target crossed the failover threshold, and is still busy", func(t *testing.T) test {
		const (
			secondOccupier = "the second target's session asks"
			firstOccupier  = "the first target's session asks"
			newcomer       = "the new session asks"
		)
		firstOccupierArrives := time.Second / 2
		newcomerArrives := admissionGracePeriod + defaultFailoverThreshold - 2*time.Second
		firstFrees := admissionGracePeriod + defaultFailoverThreshold + 3*time.Second
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			if inboundMessage == firstOccupier {
				time.Sleep(firstFrees - firstOccupierArrives)
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": silentBackend(t, receivedB)}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "b/mb", userMessages: []string{secondOccupier}},
				{model: "m", userMessages: []string{firstOccupier}, after: firstOccupierArrives},
				{model: "m", userMessages: []string{newcomer}, after: newcomerArrives - firstOccupierArrives},
			},
			reached:              map[string][]string{"a": {firstOccupier}, "b": {secondOccupier}},
			secondCheckAt:        firstFrees + 3*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {firstOccupier, newcomer}, "b": {secondOccupier}},
			answered:             map[string]int{newcomer: http.StatusOK},
		}
	})
	// The second target's occupier is answered while the first target's is
	// still running.
	tests.AddFunc("should send a new session's first request waiting at the first target to the second target once the second target is no longer busy/the second target was busy when the first target crossed the failover threshold", func(t *testing.T) test {
		const (
			secondOccupier = "the second target's session asks"
			firstOccupier  = "the first target's session asks"
			newcomer       = "the new session asks"
		)
		// The first target is occupied after the second, so its busy clock's
		// crossing falls that much later than the second's.
		firstOccupierArrives := 3 * time.Second
		newcomerArrives := firstOccupierArrives + admissionGracePeriod + defaultFailoverThreshold - 2*time.Second
		secondFrees := firstOccupierArrives + admissionGracePeriod + defaultFailoverThreshold + time.Second
		firstFrees := secondFrees + 5*time.Second
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			if inboundMessage == firstOccupier {
				// Its answer falls past the second check, so its client's
				// departure is what ends the wait at teardown.
				select {
				case <-r.Context().Done():
					return nil, context.Cause(r.Context())
				case <-time.After(firstFrees - firstOccupierArrives):
				}
			}
			return admissionJSON(inboundMessage), nil
		})
		second := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedB.add(inboundMessage)
			if inboundMessage == secondOccupier {
				time.Sleep(secondFrees)
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": second}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				// Names the second target's backend and model directly; it shares
				// that target's health key, so occupying it holds the target.
				{model: "b/mb", userMessages: []string{secondOccupier}},
				{model: "m", userMessages: []string{firstOccupier}, after: firstOccupierArrives},
				// Arrives after the second target's crossing, before the
				// first target's.
				{model: "m", userMessages: []string{newcomer}, after: newcomerArrives - firstOccupierArrives},
			},
			reached: map[string][]string{"a": {firstOccupier}, "b": {secondOccupier}},
			// After the second target's occupier is answered, before the
			// first target's is.
			secondCheckAt:        secondFrees + 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {firstOccupier}, "b": {secondOccupier, newcomer}},
			answered:             map[string]int{newcomer: http.StatusOK},
		}
	})
	// The second target is failing past the failover threshold at the first
	// target's crossing, and a request naming it directly succeeds there
	// while the first target is still busy.
	tests.AddFunc("should send a new session's first request waiting at the first target to the second target once a request to the second target succeeds/the second target was failing past the failover threshold, with its recovery probe not yet due", func(t *testing.T) test {
		const (
			demoter  = "the session naming the second target asks"
			healer   = "the session naming the second target asks again"
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		occupierArrives := time.Second
		crossing := occupierArrives + admissionGracePeriod + defaultFailoverThreshold
		newcomerArrives := crossing - 2*time.Second
		healerArrives := crossing + 2*time.Second
		firstFrees := defaultRecoveryInterval + 10*time.Second
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			if inboundMessage == occupier {
				select {
				case <-r.Context().Done():
					return nil, context.Cause(r.Context())
				case <-time.After(firstFrees - occupierArrives):
				}
			}
			return admissionJSON(inboundMessage), nil
		})
		second := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedB.add(inboundMessage)
			if inboundMessage != demoter {
				return admissionJSON(inboundMessage), nil
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"only available hosted in China and requires explicit opt in"}}`)),
			}, nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": second}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				// Both name the second target's backend and model directly;
				// they share its health key, so the refusal demotes the
				// target and the success clears it.
				{model: "b/mb", userMessages: []string{demoter}},
				{model: "m", userMessages: []string{occupier}, after: occupierArrives},
				{model: "m", userMessages: []string{newcomer}, after: newcomerArrives - occupierArrives},
				{model: "b/mb", userMessages: []string{healer}, after: healerArrives - newcomerArrives},
			},
			reached: map[string][]string{"a": {occupier}, "b": {demoter, healer, newcomer}},
			// Before the second target's recovery probe is due.
			secondCheckAt:        defaultRecoveryInterval - 2*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier}, "b": {demoter, healer, newcomer}},
			answered:             map[string]int{newcomer: http.StatusOK, healer: http.StatusOK},
		}
	})
	// The new session's first request arrives before the first target's busy
	// clock crosses the failover threshold, so it is waiting there at the
	// crossing; the second target refuses every request it receives, and the
	// request keeping the first target busy is answered past the crossing.
	tests.AddFunc("should send a new session's first request back to the first target once the first target is no longer busy, when the second target it moved to refuses the request", func(t *testing.T) test {
		const (
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			if inboundMessage == occupier {
				time.Sleep(admissionGracePeriod + defaultFailoverThreshold + 3*time.Second)
			}
			return admissionJSON(inboundMessage), nil
		})
		refusing := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedB.add(inboundMessage)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
			}, nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": refusing}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{occupier}},
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold - 2*time.Second},
			},
			reached:              map[string][]string{"a": {occupier}},
			secondCheckAt:        admissionGracePeriod + defaultFailoverThreshold + 5*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier, newcomer}, "b": {newcomer}},
			answered:             map[string]int{newcomer: http.StatusOK},
		}
	})
	tests.AddFunc("should send a new session's first turn to a target a waiting first turn left, once that target is no longer busy", func(t *testing.T) test {
		const (
			occupier = "the other session asks"
			moved    = "the first new session asks"
			returned = "the second new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": silentBackend(t, receivedA),
				"b": silentBackend(t, receivedB),
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				// Its client leaves after the first new session has left for
				// the second target, so the first target stops being busy
				// without answering anything; answering would clear the
				// failure recorded against it.
				{model: "m", userMessages: []string{occupier}, clientLeavesAfter: admissionGracePeriod + defaultFailoverThreshold + 3*time.Second},
				{model: "m", userMessages: []string{moved}, after: admissionGracePeriod + defaultFailoverThreshold - 2*time.Second},
				{model: "m", userMessages: []string{returned}, after: 8 * time.Second},
			},
			reached: map[string][]string{"a": {occupier, returned}, "b": {moved}},
		}
	})
	// A request displaced back into the line carries the arrival of its
	// admission, but the target has held arrivals since that admission's
	// grace period ended: the busy clock runs from there, and a first turn
	// arriving before the threshold has passed from that moment waits.
	tests.AddFunc("should send a new session's first turn to the second target when the first has been busy for the failover threshold since a displaced request's grace period ended", func(t *testing.T) test {
		const (
			older    = "the older session asks"
			younger  = "the younger session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			if inboundMessage == older {
				// Answered at once, fixing the older session's rank ahead of
				// the younger one's.
				return admissionJSON(inboundMessage), nil
			}
			// Every later turn waits at the target with no header: the
			// displacing turn holds the target unconfirmed, so the
			// displaced younger request stays in the line.
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		// Times are from the first arrival.
		youngerArrives := time.Second
		youngerGraceEnds := youngerArrives + admissionGracePeriod
		displaced := youngerGraceEnds + time.Second
		// The threshold has passed since the younger request arrived, but
		// not since its grace period ended.
		newcomerArrives := youngerArrives + defaultFailoverThreshold + admissionGracePeriod/2
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": silentBackend(t, receivedB)}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{older}},
				// Waits at the target, silent with no header.
				{model: "m", userMessages: []string{younger}, after: youngerArrives},
				// Displaces the younger request back into the line, and
				// holds the target.
				{model: "m", userMessages: []string{older, older + " again"}, after: displaced - youngerArrives},
				{model: "m", userMessages: []string{newcomer}, after: newcomerArrives - displaced},
			},
			reached: map[string][]string{"a": {older, younger, older + " again"}},
			// Just past the threshold counted from the end of the younger
			// request's grace period.
			secondCheckAt:        youngerGraceEnds + defaultFailoverThreshold + time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {older, younger, older + " again"}, "b": {newcomer}},
		}
	})
	// The queue has held arrivals without a break since the younger
	// request's grace period ended, so the crossing stays where it was when
	// the request that has waited longest leaves the queue.
	tests.AddFunc("should send a new session's first request waiting at the first target to the second target once the first target has been busy for the failover threshold/the client of the request waiting longest left before then", func(t *testing.T) test {
		const (
			older    = "the older session asks"
			younger  = "the younger session asks"
			newcomer = "the new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			if inboundMessage == older {
				// Answered at once, fixing the older session's rank ahead of
				// the younger one's.
				return admissionJSON(inboundMessage), nil
			}
			// The older session's second request holds the target with no
			// content, so the younger request it displaced stays in the
			// queue.
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		// Times are from the first arrival.
		youngerArrives := time.Second
		youngerGraceEnds := youngerArrives + admissionGracePeriod
		displaced := youngerGraceEnds + time.Second
		newcomerArrives := youngerArrives + defaultFailoverThreshold + admissionGracePeriod/2
		youngerLeaves := newcomerArrives + time.Second/2
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"a": first, "b": silentBackend(t, receivedB)}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{older}},
				// Its client gives up once the new session's request waits
				// behind it, while the target still holds arrivals.
				{model: "m", userMessages: []string{younger}, after: youngerArrives, clientLeavesAfter: youngerLeaves - youngerArrives},
				// Displaces the younger request back into the queue, and
				// holds the target.
				{model: "m", userMessages: []string{older, older + " again"}, after: displaced - youngerArrives},
				{model: "m", userMessages: []string{newcomer}, after: newcomerArrives - displaced},
			},
			reached: map[string][]string{"a": {older, younger, older + " again"}},
			// Just past the threshold counted from the end of the younger
			// request's grace period, and short of it counted from the end
			// of the older session's second request's.
			secondCheckAt:        youngerGraceEnds + defaultFailoverThreshold + time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {older, younger, older + " again"}, "b": {newcomer}},
		}
	})

	// The new session's turn arrives two seconds before the target's busy
	// clock crosses the failover threshold, so it is waiting there at the
	// crossing; the model has no other target, and the request keeping the
	// target busy is answered only past the crossing.
	tests.AddFunc("should send a new session's first turn waiting at the only target once it is no longer busy, when the target has been busy for the failover threshold", func(t *testing.T) test {
		const (
			occupier = "the other session asks"
			newcomer = "the new session asks"
		)
		received := &receivedMessages{}
		only := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage == occupier {
				// Answers three seconds past the failover threshold, so the
				// target is busy at the crossing and no longer busy after.
				time.Sleep(admissionGracePeriod + defaultFailoverThreshold + 3*time.Second)
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"a": only}, map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}}}}),
			received: map[string]*receivedMessages{"a": received},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{occupier}},
				{model: "m", userMessages: []string{newcomer}, after: admissionGracePeriod + defaultFailoverThreshold - 2*time.Second},
			},
			reached:              map[string][]string{"a": {occupier}},
			secondCheckAt:        admissionGracePeriod + defaultFailoverThreshold + 5*time.Second,
			reachedAtSecondCheck: map[string][]string{"a": {occupier, newcomer}},
		}
	})
	tests.AddFunc("should send a new session's first turn to a target busy past the failover threshold once it is no longer busy", func(t *testing.T) test {
		const (
			occupier = "the other session asks"
			routed   = "the first new session asks"
			returned = "the second new session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(_ *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			if inboundMessage == occupier {
				// Sleeps past the failover threshold so the first new
				// session fails over, and answers before the second
				// new session arrives, so the target is no longer busy.
				time.Sleep(admissionGracePeriod + defaultFailoverThreshold + 3*time.Second)
			}
			return admissionJSON(inboundMessage), nil
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": first,
				"b": silentBackend(t, receivedB),
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{occupier}},
				{model: "m", userMessages: []string{routed}, after: admissionGracePeriod + defaultFailoverThreshold + time.Second},
				{model: "m", userMessages: []string{returned}, after: 5 * time.Second},
			},
			reached: map[string][]string{"a": {occupier, returned}, "b": {routed}},
		}
	})
	tests.AddFunc("should hold a session's later turn arriving at the first target when it has been busy past the failover threshold", func(t *testing.T) test {
		const (
			bound    = "the bound session asks"
			occupier = "the other session asks"
		)
		receivedA, receivedB := &receivedMessages{}, &receivedMessages{}
		first := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			receivedA.add(inboundMessage)
			switch inboundMessage {
			case bound:
				return admissionJSON(inboundMessage), nil
			case occupier:
				// A header and then no content, so the request stays
				// unconfirmed and no arrival can displace it.
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(readerFunc(func([]byte) (int, error) {
						<-r.Context().Done()
						return 0, context.Cause(r.Context())
					})),
				}, nil
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{
				"a": first,
				"b": silentBackend(t, receivedB),
			}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "a", model: "ma"},
				{backend: "b", model: "mb"},
			}}}),
			received: map[string]*receivedMessages{"a": receivedA, "b": receivedB},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{bound}},
				{model: "m", userMessages: []string{occupier}, after: time.Second / 2},
				{model: "m", userMessages: []string{bound, bound + " again"}, after: admissionGracePeriod + defaultFailoverThreshold + time.Second},
			},
			reached: map[string][]string{"a": {bound, occupier}},
		}
	})
	tests.AddFunc("should hold a new request at a target behind a request that has waited there longer than the target's health record is kept", func(t *testing.T) test {
		const (
			occupier = "the occupying session asks"
			waiter   = "the waiting session asks"
			late     = "the late session asks"
		)
		occupierAnswerAfter := healthTTL + 2*time.Minute
		// The occupier is sent again at each first-content timeout, so
		// only the other requests are recorded.
		received := &receivedMessages{}
		var answerAt time.Time
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			if inboundMessage != occupier {
				received.add(inboundMessage)
				return admissionJSON(inboundMessage), nil
			}
			if answerAt.IsZero() {
				answerAt = time.Now().Add(occupierAnswerAfter)
			}
			select {
			case <-r.Context().Done():
				return nil, context.Cause(r.Context())
			case <-time.After(time.Until(answerAt)):
				// An open stream holds its place without ending, so the
				// queue sends one request at a time, oldest session first.
				return admissionStream(r, nil, nil), nil
			}
		})
		return test{
			server: newTargetsServer(t, map[string]*http.Client{"openai": backend}, map[string]LogicalModel{"m": {Targets: []Target{
				{backend: "openai", model: "gpt-4"},
			}}}),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{model: "m", userMessages: []string{occupier}, clientLeavesAfter: 2 * healthTTL},
				{model: "m", userMessages: []string{waiter}, after: admissionGracePeriod + time.Second, clientLeavesAfter: 2 * healthTTL},
				// Its walk to the logical model's target sweeps the health
				// records, once the waiting request has waited longer than
				// they are kept.
				{model: "m", userMessages: []string{late}, after: healthTTL + time.Minute},
			},
			reached:              map[string][]string{},
			secondCheckAt:        occupierAnswerAfter + time.Minute,
			reachedAtSecondCheck: map[string][]string{"openai": {waiter, late}},
			answered:             map[string]int{waiter: http.StatusOK, late: http.StatusOK},
		}
	})

	tests.AddFunc("should log how long a request waited for a busy target before it was sent", func(t *testing.T) test {
		const (
			first  = "the first session asks a target slow to answer"
			second = "the second session asks the busy target"
		)
		var (
			// Past the first request's grace period, so the target holds
			// the second request.
			secondArrives        = admissionGracePeriod + time.Second
			secondWaits          = 4 * time.Second
			secondHeaderArrives  = 5 * time.Second
			secondContentArrives = 6 * time.Second
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			if inboundMessage == first {
				// Its content is what lets the second request through.
				return admissionStreamContentAt(r, time.After(secondArrives+secondWaits), nil), nil
			}
			// The second request's own header and content come after
			// delays distinct from its wait, so neither can pass for it.
			time.Sleep(secondHeaderArrives)
			contentFollowsHeader := secondContentArrives - secondHeaderArrives
			return admissionStreamContentAt(r, time.After(contentFollowsHeader), time.After(contentFollowsHeader)), nil
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{first}},
				{userMessages: []string{second}, after: secondArrives},
			},
			reached:              map[string][]string{"openai": {first}},
			secondCheckAt:        secondArrives + secondWaits + secondContentArrives + time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {first, second}},
			answered:             map[string]int{second: http.StatusOK},
			waited:               map[string]time.Duration{second: secondWaits},
		}
	})

	tests.AddFunc("should log the total time a request waited for its target when it was sent there again after an older session's request displaced it", func(t *testing.T) test {
		const (
			olderOpening = "the older session asks"
			olderLater   = "the older session asks again"
			followed     = "the followed session asks"
		)
		var (
			// Past the opening request's grace period, so the target holds
			// the followed request.
			followedArrives = admissionGracePeriod + time.Second
			firstWait       = 5 * time.Second
			secondWait      = 3 * time.Second
			// The followed request has sat at the target past its grace
			// period by then, so the older session's request displaces it.
			displacesAt = followedArrives + firstWait + admissionGracePeriod + time.Second
		)
		received := &receivedMessages{}
		backend := admissionClient(t, nil, func(r *http.Request, inboundMessage string) (*http.Response, error) {
			received.add(inboundMessage)
			switch inboundMessage {
			case olderOpening:
				// Its content is what lets the followed request through the
				// first time.
				return admissionStreamContentAt(r, time.After(followedArrives+firstWait), nil), nil
			case olderLater:
				// Its content is what lets the followed request through the
				// second time.
				return admissionStreamContentAt(r, time.After(secondWait), nil), nil
			case followed:
				if received.count(followed) > 1 {
					return admissionJSON(inboundMessage), nil
				}
			}
			<-r.Context().Done()
			return nil, context.Cause(r.Context())
		})
		return test{
			server:   newTargetsServer(t, map[string]*http.Client{"openai": backend}, nil),
			received: map[string]*receivedMessages{"openai": received},
			arrivals: []clientTurn{
				{userMessages: []string{olderOpening}},
				{userMessages: []string{followed}, after: followedArrives},
				{userMessages: []string{olderOpening, olderLater}, after: displacesAt - followedArrives},
			},
			reached: map[string][]string{"openai": {olderOpening, followed, olderLater}},
			// The followed request appears twice: the older session's
			// request displaces its first send, and its client receives the
			// second send's answer.
			secondCheckAt:        displacesAt + secondWait + time.Second,
			reachedAtSecondCheck: map[string][]string{"openai": {olderOpening, followed, olderLater, followed}},
			answered:             map[string]int{followed: http.StatusOK},
			waited:               map[string]time.Duration{followed: firstWait + secondWait},
		}
	})

	tests.Run(t, func(t *testing.T, tt test) {
		synctest.Test(t, func(t *testing.T) {
			poster := newAdmissionPoster(t, tt.server)

			start := time.Now()
			postTurns(t, poster, tt.arrivals)

			for name, received := range tt.received {
				if d := gocmp.Diff(tt.reached[name], received.all()); d != "" {
					t.Errorf("requests the %q backend received on their arrival (-want +got):\n%s", name, d)
				}
			}
			if tt.reachedAtSecondCheck == nil {
				return
			}
			time.Sleep(time.Until(start.Add(tt.secondCheckAt)))
			synctest.Wait()
			for name, received := range tt.received {
				if d := gocmp.Diff(tt.reachedAtSecondCheck[name], received.all()); d != "" {
					t.Errorf("requests the %q backend received %s after the first arrival (-want +got):\n%s", name, tt.secondCheckAt, d)
				}
			}
			poster.answeredWith(tt.answered)
			for label, want := range tt.waited {
				rec, ok := poster.logRecord(label)
				if !ok {
					t.Errorf("%q was never answered", label)
					continue
				}
				if rec.TimeQueued != want {
					t.Errorf("%q's record reports it waited %s for its target, want %s", label, rec.TimeQueued, want)
				}
			}
		})
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
			t.Errorf("after seeding on the first bare 429: maxInFlight=%d, want 1", maxInFlight)
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

	if _, err := l.acquire(ctx, time.Time{}); err != nil {
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
	for ok, _ := l.tryAcquire(); ok; ok, _ = l.tryAcquire() {
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
				_, _ = l.tryAcquire()
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

// TestChatCompletionsScattersBusyBackoff verifies clients waiting on one busy
// backend are not all told to return at the same instant. Every client at the
// same moment derives its interval from one shared clock, so an unscattered
// backoff would send them back together — and again, and again.
func TestChatCompletionsScattersBusyBackoff(t *testing.T) {
	t.Parallel()

	client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"server busy","code":"unavailable"}}`)),
			Header:     make(http.Header),
		}, nil
	})
	validator := &stubValidator{ValidateFn: func(context.Context, string) (*BackendConfig, error) {
		return &BackendConfig{
			Backends: map[string]Backend{"a": {ProviderType: "openai", Config: providers.Config{BaseURL: &url.URL{Scheme: "http", Host: "a", Path: "/v1"}, APIKey: "sk-a", HTTPClient: client}}},
			Models:   map[string]LogicalModel{"m": {Targets: []Target{{backend: "a", model: "ma"}}}},
		}, nil
	}}
	srv := New(validator, WithLogger(testLogger(t)))

	told := make(map[int]struct{})
	for range 20 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		wait, err := strconv.Atoi(rr.Header().Get("Retry-After"))
		if err != nil {
			t.Fatalf("client was told to return after %q: %v", rr.Header().Get("Retry-After"), err)
		}
		told[wait] = struct{}{}
	}

	// Scattering only shortens the synthesized interval, never lengthens it, and
	// its floor is the demotion threshold itself — any lower and the scattered
	// draw would stop counting as a sustained rate limit.
	for wait := range told {
		lo := int(rateLimitDemotionThreshold / time.Second)
		hi := int(float64(lo) / (1 - defaultJitterFactor))
		if wait < lo || wait > hi {
			t.Errorf("client was told to return after %ds, outside the %d-%ds a scattered interval may span", wait, lo, hi)
		}
	}
	if len(told) < 2 {
		t.Errorf("every client was told to return after %v; they will come back in lockstep", told)
	}
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

	tests.Add("should seed the cap to the observed in-flight count on the first bare rate limit", test{
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
	tests.Add("should set the cap just below the in-flight count when a bare rate limit arrives at saturation", test{
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
	tests.Add("should hold the cap at one when a bare rate limit arrives at a saturated cap of one", test{
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
			if ok, _ := l.tryAcquire(); !ok {
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
			isRetryable: true,
			condition:   notRateLimited,
		},
	})
	tests.Add("should classify a 429 with no Retry-After as a bare rate limit", test{
		buildErr: func() error {
			return yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited"))
		},
		want: limitClassification{
			status:      http.StatusTooManyRequests,
			isRateLimit: true,
			isRetryable: true,
			condition:   bareRateLimit,
		},
	})
	tests.AddFunc("should classify a 429 whose Retry-After reaches the demotion threshold as long enough to demote the target", func(*testing.T) test {
		return test{
			buildErr: func() error {
				return retryAfterError{
					error: yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited")),
					at:    time.Now().Add(rateLimitDemotionThreshold),
				}
			},
			want: limitClassification{
				status:        http.StatusTooManyRequests,
				isRateLimit:   true,
				isRetryable:   true,
				hasRetryAfter: true,
				retryAfter:    rateLimitDemotionThreshold,
				condition:     sustainedRate,
			},
		}
	})
	tests.AddFunc("should classify a 429 whose Retry-After is just under the demotion threshold as too short to demote the target", func(*testing.T) test {
		return test{
			buildErr: func() error {
				return retryAfterError{
					error: yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited")),
					at:    time.Now().Add(rateLimitDemotionThreshold - time.Second),
				}
			},
			want: limitClassification{
				status:        http.StatusTooManyRequests,
				isRateLimit:   true,
				isRetryable:   true,
				hasRetryAfter: true,
				retryAfter:    rateLimitDemotionThreshold - time.Second,
				condition:     transientRate,
			},
		}
	})
	tests.AddFunc("should reject a 429 with a Retry-After beyond the passthrough ceiling", func(*testing.T) test {
		return test{
			buildErr: func() error {
				return retryAfterError{
					error: yerrors.WithHTTPStatus(http.StatusTooManyRequests, errors.New("rate limited")),
					at:    time.Now().Add(maxPassthroughRetryAfter + time.Second),
				}
			},
			want: limitClassification{
				status:        http.StatusTooManyRequests,
				isRateLimit:   true,
				isRetryable:   true,
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

func (p fakeChatProvider) Chat(context.Context, providers.Config, string, io.Reader) (*http.Response, error) {
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

// droppingHandler stands in for a consumer's handler that honors cancellation,
// discarding any record whose context is already done.
type droppingHandler struct{ slog.Handler }

func (h droppingHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx.Err() == nil {
		return h.Handler.Handle(ctx, r)
	}
	return nil
}

// blockingHandler stands in for a consumer's handler that is slow to write: it takes
// the first record, announces that it is holding it, and does not return it until the
// test releases it.
type blockingHandler struct {
	slog.Handler
	holdingRecord chan struct{}
	releaseRecord chan struct{}
	once          sync.Once
}

func (h *blockingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.once.Do(func() {
		close(h.holdingRecord)
		<-h.releaseRecord
	})
	return h.Handler.Handle(ctx, r)
}

// TestChatCompletionsRoutesWhileTheLogSinkBlocks pins DESIGN's "a transition is never
// emitted while the health map is held": a consumer's handler is theirs to make slow,
// and one that blocks must cost only the request that triggered it.
func TestChatCompletionsRoutesWhileTheLogSinkBlocks(t *testing.T) {
	t.Parallel()

	// Real time, not synctest: the failure this pins is a request blocked on s.mu,
	// and a mutex wait does not durably block, so a virtual clock never advances past
	// it — the test would hang where it should fail.
	var logBuf bytes.Buffer
	sink := &blockingHandler{Handler: slog.NewJSONHandler(&logBuf, nil), holdingRecord: make(chan struct{}), releaseRecord: make(chan struct{})}
	clientA := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Retry-After", "50")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)), Header: header}, nil
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
	srv := New(validator, WithLogger(slog.New(sink)))

	serve := func() *httptest.ResponseRecorder {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer user-token")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	demoted := make(chan struct{})
	go func() {
		defer close(demoted)
		serve()
	}()
	// However this test leaves — assertion, fatal, or panic — the held handler is
	// released and its request awaited, so no goroutine outlives the test.
	defer func() {
		close(sink.releaseRecord)
		<-demoted
	}()

	<-sink.holdingRecord
	statusWhileHeld := make(chan int, 1)
	go func() { statusWhileHeld <- serve().Code }()
	select {
	case code := <-statusWhileHeld:
		if code != http.StatusOK {
			t.Errorf("a request did not route while the sink held a record: got %d, want %d", code, http.StatusOK)
		}
	case <-time.After(5 * time.Second):
		t.Error("a request could not route while the sink held a record")
	}
}
