package openai

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gitlab.com/flimzy/testy/v2"
	"gitlab.com/flimzy/yerrors"

	"github.com/lindyloopdev/understudy/providers"
)

type capturedRequest struct {
	Method      string
	Path        string
	Auth        string
	ContentType string
	Body        string
}

// newCapturingServer stands up an httptest server that records the inbound
// request into the returned *capturedRequest and responds with the given
// status, headers, and body.
func newCapturingServer(t *testing.T, status int, headers http.Header, body string) (string, *capturedRequest) {
	t.Helper()
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		got = capturedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Auth:        r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"),
			Body:        string(reqBody),
		}
		for k, vs := range headers {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestChatRequest verifies what Chat sends upstream.
func TestChatRequest(t *testing.T) {
	t.Parallel()

	standardBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`

	srvURL, got := newCapturingServer(t, http.StatusOK, nil, "")

	resp, err := Chat(t.Context(), providers.Config{BaseURL: mustParseURL(t, srvURL+"/v4"), APIKey: "sk-test"}, strings.NewReader(standardBody))
	if err != nil {
		t.Fatalf("Chat returned unexpected error: %v", err)
	}
	_ = resp.Body.Close()

	want := capturedRequest{
		Method:      http.MethodPost,
		Path:        "/v4/chat/completions",
		Auth:        "Bearer sk-test",
		ContentType: "application/json",
		Body:        standardBody,
	}
	if d := gocmp.Diff(want, *got); d != "" {
		t.Errorf("unexpected request to backend (-want +got):\n%s", d)
	}
}

// TestModelsRequest verifies what Models sends upstream: given a base_url
// carrying a "/v4" prefix, the backend receives the request at exactly
// "/v4/models" with no provider-injected version segment.
func TestModelsRequest(t *testing.T) {
	t.Parallel()

	srvURL, got := newCapturingServer(t, http.StatusOK, http.Header{"Content-Type": {"application/json"}}, `{"data":[]}`)

	_, err := Models(t.Context(), providers.Config{BaseURL: mustParseURL(t, srvURL+"/v4"), APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("Models returned unexpected error: %v", err)
	}

	if got.Method != http.MethodGet {
		t.Errorf("method: got %q, want %q", got.Method, http.MethodGet)
	}
	if got.Path != "/v4/models" {
		t.Errorf("path: got %q, want %q", got.Path, "/v4/models")
	}
}

// TestModelsBoundsCallWithinTimeout verifies the timeout-bounded core bounds an
// upstream call to the given timeout even when the caller's context carries no
// deadline: the backend hangs until its request context is cancelled, so models
// can only return if it imposed the bound.
func TestModelsBoundsCallWithinTimeout(t *testing.T) {
	t.Parallel()

	client := testy.HTTPClient(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, context.Cause(r.Context())
	})
	cfg := providers.Config{BaseURL: mustParseURL(t, "http://nonexistent.invalid"), APIKey: "sk-test", HTTPClient: client}

	errCh := make(chan error, 1)
	go func() {
		_, err := models(t.Context(), cfg, 10*time.Millisecond)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error: got %v, want context.DeadlineExceeded", err)
		}
		if got := yerrors.HTTPStatus(err); got != http.StatusInternalServerError {
			t.Errorf("status: got %d, want %d (status-less; classified 504 at the seam)", got, http.StatusInternalServerError)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("models did not return; it failed to bound the call with the timeout")
	}
}

// TestChatResponse verifies what Chat surfaces to the caller given the
// backend's behavior. Cases vary backend status, headers, reachability, ctx,
// and custom client.
func TestChatResponse(t *testing.T) {
	t.Parallel()

	type test struct {
		cfg             providers.Config
		ctx             context.Context
		wantStatus      int
		wantHeaders     http.Header
		wantErr         string
		wantErrExcludes string
		wantServerBusy  bool
		wantRetryAfter  time.Time
		wantErrorType   string
		wantAttrs       map[string]any
	}

	tests := testy.NewTable[test]()

	tests.AddFunc("should propagate backend's 200 status", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusOK, nil, "")
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantStatus: http.StatusOK,
		}
	})
	tests.AddFunc("should return error carrying upstream message on non-2xx", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusUnauthorized, http.Header{"Content-Type": {"application/json"}}, `{"error":{"message":"invalid api key"}}`)
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "invalid api key",
			wantStatus:    http.StatusUnauthorized,
			wantErrorType: "server_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusUnauthorized),
				"upstream_error_type":    "server_error",
				"upstream_error_message": "invalid api key",
			},
		}
	})
	tests.AddFunc("should fall back to raw body when error is not the JSON envelope", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusBadGateway, http.Header{"Content-Type": {"text/html"}}, "<html>502 Bad Gateway</html>")
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "502 Bad Gateway",
			wantStatus:    http.StatusBadGateway,
			wantErrorType: "server_error",
			wantAttrs: map[string]any{
				"upstream_status":     int64(http.StatusBadGateway),
				"upstream_error_type": "server_error",
			},
		}
	})
	tests.AddFunc("should carry the upstream Retry-After header on the error", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusTooManyRequests,
			http.Header{"Content-Type": {"application/json"}, "Retry-After": {"Wed, 21 Oct 2026 07:28:00 GMT"}},
			`{"error":{"message":"rate limited"}}`)
		return test{
			cfg:            providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:        "rate limited",
			wantStatus:     http.StatusTooManyRequests,
			wantRetryAfter: time.Date(2026, time.October, 21, 7, 28, 0, 0, time.UTC),
			wantErrorType:  "rate_limit_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusTooManyRequests),
				"upstream_error_type":    "rate_limit_error",
				"upstream_error_message": "rate limited",
			},
		}
	})
	// This case is the single Chat-level plumbing check for Retry-After; the
	// time/seconds/invalid parsing matrix is covered in TestWithRetryAfter.
	tests.AddFunc("should propagate response headers from backend", func(t *testing.T) test {
		hdrs := http.Header{
			"Content-Type":        {"text/event-stream"},
			"Retry-After":         {"30"},
			"X-Custom-Rate-Limit": {"42"},
		}
		srvURL, _ := newCapturingServer(t, http.StatusOK, hdrs, "")
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantStatus: http.StatusOK,
			wantHeaders: http.Header{
				"Content-Type":        {"text/event-stream"},
				"Retry-After":         {"30"},
				"X-Custom-Rate-Limit": {"42"},
			},
		}
	})
	tests.Add("should return error when backend is unreachable", test{
		cfg:        providers.Config{BaseURL: mustParseURL(t, "http://127.0.0.1:1"), APIKey: "sk-test"},
		wantErr:    "connect",
		wantStatus: http.StatusBadGateway,
	})
	tests.AddFunc("should return ctx.Err() when context is cancelled", func(t *testing.T) test {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		srvURL, _ := newCapturingServer(t, http.StatusOK, nil, "")
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			ctx:        ctx,
			wantErr:    "context canceled",
			wantStatus: http.StatusInternalServerError,
		}
	})
	tests.AddFunc("should leave a custom-caused cancellation status-less", func(t *testing.T) test {
		errCustom := errors.New("custom shutdown")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errCustom)
		srvURL, _ := newCapturingServer(t, http.StatusOK, nil, "")
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			ctx:        ctx,
			wantErr:    "custom shutdown",
			wantStatus: http.StatusInternalServerError,
		}
	})
	tests.AddFunc("should use Config.Client when provided", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     http.Header{},
			}, nil
		})
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, "http://nonexistent.invalid"), APIKey: "sk-test", HTTPClient: client},
			wantStatus: http.StatusOK,
		}
	})

	tests.AddFunc("should carry an upstream 5xx as the status the upstream sent", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusInternalServerError, http.Header{"Content-Type": {"application/json"}}, `{"error":{"message":"kaboom"}}`)
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "upstream returned status 500",
			wantStatus:    http.StatusInternalServerError,
			wantErrorType: "server_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusInternalServerError),
				"upstream_error_type":    "server_error",
				"upstream_error_message": "kaboom",
			},
		}
	})
	tests.AddFunc("should type an upstream 400 as invalid_request_error", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusBadRequest, http.Header{"Content-Type": {"application/json"}}, `{"error":{"message":"bad input"}}`)
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "bad input",
			wantStatus:    http.StatusBadRequest,
			wantErrorType: "invalid_request_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusBadRequest),
				"upstream_error_type":    "invalid_request_error",
				"upstream_error_message": "bad input",
			},
		}
	})
	tests.AddFunc("should type an upstream 404 as invalid_request_error", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusNotFound, http.Header{"Content-Type": {"application/json"}}, `{"error":{"message":"model not found"}}`)
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "model not found",
			wantStatus:    http.StatusNotFound,
			wantErrorType: "invalid_request_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusNotFound),
				"upstream_error_type":    "invalid_request_error",
				"upstream_error_message": "model not found",
			},
		}
	})

	tests.AddFunc("should carry the upstream error type, message, and code as structured attrs", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusUnauthorized, http.Header{"Content-Type": {"application/json"}},
			`{"error":{"message":"invalid api key","type":"authentication_error","code":"invalid_api_key"}}`)
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "invalid api key",
			wantStatus:    http.StatusUnauthorized,
			wantErrorType: "authentication_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusUnauthorized),
				"upstream_error_type":    "authentication_error",
				"upstream_error_message": "invalid api key",
				"upstream_error_code":    "invalid_api_key",
			},
		}
	})

	tests.AddFunc("should carry the Gemini quota id, value, and retry delay as structured attrs", func(t *testing.T) test {
		body := `{"error":{"code":429,"message":"quota exceeded","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests","quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier","quotaValue":"20"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"42s"}]}}`
		srvURL, _ := newCapturingServer(t, http.StatusTooManyRequests, http.Header{"Content-Type": {"application/json"}}, body)
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "quota exceeded",
			wantStatus:    http.StatusTooManyRequests,
			wantErrorType: "rate_limit_error",
			wantAttrs: map[string]any{
				"upstream_status":            int64(http.StatusTooManyRequests),
				"upstream_error_type":        "rate_limit_error",
				"upstream_error_message":     "quota exceeded",
				"upstream_quota_id":          "GenerateRequestsPerMinutePerProjectPerModel-FreeTier",
				"upstream_quota_value":       "20",
				"upstream_quota_retry_delay": "42s",
			},
		}
	})

	tests.AddFunc("should comma-join the ids and values of multiple quota violations", func(t *testing.T) test {
		body := `{"error":{"code":429,"message":"quota exceeded","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier","quotaValue":"5"},{"quotaId":"GenerateContentInputTokensPerModelPerMinute-FreeTier","quotaValue":"250000"}]}]}}`
		srvURL, _ := newCapturingServer(t, http.StatusTooManyRequests, http.Header{"Content-Type": {"application/json"}}, body)
		return test{
			cfg:           providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:       "quota exceeded",
			wantStatus:    http.StatusTooManyRequests,
			wantErrorType: "rate_limit_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusTooManyRequests),
				"upstream_error_type":    "rate_limit_error",
				"upstream_error_message": "quota exceeded",
				"upstream_quota_id":      "GenerateRequestsPerMinutePerProjectPerModel-FreeTier,GenerateContentInputTokensPerModelPerMinute-FreeTier",
				"upstream_quota_value":   "5,250000",
			},
		}
	})

	tests.AddFunc("should carry the parsed error attrs of an array-wrapped upstream error envelope", func(t *testing.T) test {
		body := `[{"error":{"code":"429","message":"quota exceeded","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier","quotaValue":"20"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"42s"}]}}]`
		srvURL, _ := newCapturingServer(t, http.StatusTooManyRequests, http.Header{"Content-Type": {"application/json"}}, body)
		return test{
			cfg:             providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:         "quota exceeded",
			wantErrExcludes: "type.googleapis.com",
			wantStatus:      http.StatusTooManyRequests,
			wantErrorType:   "rate_limit_error",
			wantAttrs: map[string]any{
				"upstream_status":            int64(http.StatusTooManyRequests),
				"upstream_error_type":        "rate_limit_error",
				"upstream_error_message":     "quota exceeded",
				"upstream_error_code":        "429",
				"upstream_quota_id":          "GenerateRequestsPerMinutePerProjectPerModel-FreeTier",
				"upstream_quota_value":       "20",
				"upstream_quota_retry_delay": "42s",
			},
		}
	})

	// TODO: add "should leave a 503 carrying another code a generic failure" — a
	// 503 coded e.g. "overloaded" must not match ErrServerBusy, so it keeps the
	// ordinary 5xx path and still demotes.
	tests.AddFunc("should report an upstream 503 coded unavailable as a busy server", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusServiceUnavailable, http.Header{"Content-Type": {"application/json"}},
			`{"error":{"message":"server busy","code":"unavailable"}}`)
		return test{
			cfg:            providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:        "server busy",
			wantStatus:     http.StatusServiceUnavailable,
			wantServerBusy: true,
			wantErrorType:  "server_error",
			wantAttrs: map[string]any{
				"upstream_status":        int64(http.StatusServiceUnavailable),
				"upstream_error_type":    "server_error",
				"upstream_error_message": "server busy",
				"upstream_error_code":    "unavailable",
			},
		}
	})

	tests.AddFunc("should cap the error message at 1 KiB", func(t *testing.T) test {
		body := strings.Repeat("x", 1024) + "OVERFLOW_PAST_CAP"
		srvURL, _ := newCapturingServer(t, http.StatusBadGateway, http.Header{"Content-Type": {"text/plain"}}, body)
		return test{
			cfg:             providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:         "x",
			wantErrExcludes: "OVERFLOW_PAST_CAP",
			wantStatus:      http.StatusBadGateway,
			wantErrorType:   "server_error",
			wantAttrs: map[string]any{
				"upstream_status":     int64(http.StatusBadGateway),
				"upstream_error_type": "server_error",
			},
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		ctx := tt.ctx
		if ctx == nil {
			ctx = t.Context()
		}
		resp, err := Chat(ctx, tt.cfg, strings.NewReader(`{}`))
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Errorf("unexpected error: got %v, want /%s/", err, tt.wantErr)
		}
		if got := errors.Is(err, providers.ErrServerBusy); got != tt.wantServerBusy {
			t.Errorf("server busy: got %v, want %v", got, tt.wantServerBusy)
		}
		gotStatus := yerrors.HTTPStatus(err)
		if err == nil {
			gotStatus = resp.StatusCode
		}
		if want := cmp.Or(tt.wantStatus, http.StatusOK); gotStatus != want {
			t.Errorf("status: got %d, want %d", gotStatus, want)
		}
		var gotRetryAfter time.Time
		if ra, ok := yerrors.AsType[interface{ RetryAfter() time.Time }](err); ok {
			gotRetryAfter = ra.RetryAfter()
		}
		if !gotRetryAfter.Equal(tt.wantRetryAfter) {
			t.Errorf("RetryAfter: got %v, want %v", gotRetryAfter, tt.wantRetryAfter)
		}
		var gotErrorType string
		if et, ok := yerrors.AsType[interface{ ErrorType() string }](err); ok {
			gotErrorType = et.ErrorType()
		}
		if gotErrorType != tt.wantErrorType {
			t.Errorf("ErrorType: got %q, want %q", gotErrorType, tt.wantErrorType)
		}
		gotAttrs := make(map[string]any)
		for _, a := range yerrors.Attrs(err) {
			gotAttrs[a.Key] = a.Value.Any()
		}
		if d := gocmp.Diff(tt.wantAttrs, gotAttrs, cmpopts.EquateEmpty()); d != "" {
			t.Errorf("unexpected error attrs (-want +got):\n%s", d)
		}
		if err != nil {
			if tt.wantErrExcludes != "" && strings.Contains(err.Error(), tt.wantErrExcludes) {
				t.Errorf("error message must not contain %q, but got: %v", tt.wantErrExcludes, err)
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			return
		}
		_ = resp.Body.Close()

		for k, want := range tt.wantHeaders {
			if got := resp.Header.Values(k); !gocmp.Equal(got, want) {
				t.Errorf("header %s: got %v, want %v", k, got, want)
			}
		}
	})
}

// TestChatSurfacesCancellationCause verifies Chat relays the caller's
// cancellation cause: the returned error wraps the exact cause (recoverable via
// errors.Is), and a cause carrying its own HTTP status is honored.
func TestChatSurfacesCancellationCause(t *testing.T) {
	t.Parallel()

	type test struct {
		cause      error
		wantStatus int
	}

	tests := testy.NewTable[test]()

	tests.Add("surfaces a bare cause status-less", test{
		cause:      errors.New("custom shutdown"),
		wantStatus: http.StatusInternalServerError,
	})
	tests.Add("honors a cause's own HTTP status", test{
		cause:      yerrors.WithHTTPStatus(http.StatusServiceUnavailable, errors.New("custom shutdown")),
		wantStatus: http.StatusServiceUnavailable,
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(tt.cause)
		srvURL, _ := newCapturingServer(t, http.StatusOK, nil, "")

		_, err := Chat(ctx, providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"}, strings.NewReader(`{}`))
		if !errors.Is(err, tt.cause) {
			t.Errorf("returned error does not wrap the cancellation cause: got %v, want errors.Is(_, %v)", err, tt.cause)
		}
		if got := testy.StatusCode(err); got != tt.wantStatus {
			t.Errorf("status: got %d, want %d", got, tt.wantStatus)
		}
	})
}

// TestChatSurfacesGeminiQuotaRetryDelay verifies that a Gemini quota 429
// arriving through the OpenAI-compat endpoint surfaces its prose retry delay as
// the error's RetryAfter(), even though the response carries no Retry-After
// header and no structured retryDelay field.
func TestChatSurfacesGeminiQuotaRetryDelay(t *testing.T) {
	t.Parallel()

	body := `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details. * Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 5, model: gemini-2.5-flash\nPlease retry in 22.509013813s.\n{\"status\": \"RESOURCE_EXHAUSTED\"}","type":"rate_limit_error"}}`
	srvURL, _ := newCapturingServer(t, http.StatusTooManyRequests, http.Header{"Content-Type": {"application/json"}}, body)

	want := time.Now().Add(22509 * time.Millisecond)

	_, err := Chat(t.Context(), providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"}, strings.NewReader("{}"))

	var gotRetryAfter time.Time
	if ra, ok := yerrors.AsType[interface{ RetryAfter() time.Time }](err); ok {
		gotRetryAfter = ra.RetryAfter()
	}
	if d := gotRetryAfter.Sub(want); d < 0 || d > time.Second {
		t.Errorf("RetryAfter: got %v, want within 1s at or after %v", gotRetryAfter, want)
	}
}

// TestErrorFromResponsePerDayQuotaWaitsForDailyReset verifies that a Gemini
// free-tier 429 whose QuotaFailure violation names a per-day quota
// ("GenerateRequestsPerDayPerProjectPerModel-FreeTier") sets RetryAfter() to
// the next Pacific midnight — the daily reset — rather than trusting the
// misleading short prose delay ("Please retry in 42s") Google attaches to it.
// The reset is computed at a fixed −8 (PST) offset with no tz database, so in
// summer it lands an hour late (1am PDT = 08:00 UTC) — a deliberate bias, since
// an early estimate would retry before the reset, see it still blocked, and
// skip forward a full day.
func TestErrorFromResponsePerDayQuotaWaitsForDailyReset(t *testing.T) {
	t.Parallel()

	// A summer instant (DST in effect): 15:00 Pacific = 22:00 UTC.
	now := time.Date(2026, time.July, 11, 22, 0, 0, 0, time.UTC)
	// Next −8 midnight: July 12 00:00 at −8 = 08:00 UTC (= 1am PDT, an hour late).
	wantMidnight := time.Date(2026, time.July, 12, 8, 0, 0, 0, time.UTC)

	body := `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details. \n* Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20, model: gemini-2.5-flash\nPlease retry in 42s.\n{\"status\": \"RESOURCE_EXHAUSTED\"}","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests","quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier","quotaValue":"20"}]}]}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	got := errorFromResponse(resp, now)

	ra, ok := yerrors.AsType[interface{ RetryAfter() time.Time }](got)
	if !ok {
		t.Fatalf("error carries no RetryAfter(): %v", got)
	}
	if !ra.RetryAfter().Equal(wantMidnight) {
		t.Errorf("RetryAfter: got %v, want %v (next Pacific midnight at fixed −8, not now+42s)", ra.RetryAfter(), wantMidnight)
	}
}

// TestErrorFromResponsePerMinuteQuotaHonorsProseDelay verifies that the per-day
// treatment does not over-match: a 429 whose QuotaFailure violation names a
// per-minute quota keeps its short prose retry delay (a near-future instant),
// rather than being pushed out to the next daily reset.
func TestErrorFromResponsePerMinuteQuotaHonorsProseDelay(t *testing.T) {
	t.Parallel()

	body := `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details. \n* Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20, model: gemini-2.5-flash\nPlease retry in 42s.\n{\"status\": \"RESOURCE_EXHAUSTED\"}","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests","quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier","quotaValue":"20"}]}]}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	got := errorFromResponse(resp, time.Now())

	ra, ok := yerrors.AsType[interface{ RetryAfter() time.Time }](got)
	if !ok {
		t.Fatalf("error carries no RetryAfter(): %v", got)
	}
	if d := time.Until(ra.RetryAfter()); d <= 0 || d > 2*time.Minute {
		t.Errorf("RetryAfter: got %v (in %v), want the short prose delay (~42s), not the daily reset", ra.RetryAfter(), d)
	}
}

func TestWithRetryAfter(t *testing.T) {
	t.Parallel()

	base := yerrors.New("upstream returned status 429")

	type test struct {
		err   error
		value string
		want  time.Time // expected RetryAfter(), matched within 1s; zero means none
	}

	tests := testy.NewTable[test]()

	tests.Add("HTTP-date value sets RetryAfter to that instant", test{
		err:   base,
		value: "Wed, 21 Oct 2026 07:28:00 GMT",
		want:  time.Date(2026, time.October, 21, 7, 28, 0, 0, time.UTC),
	})
	tests.Add("delta-seconds value sets RetryAfter to now plus those seconds", test{
		err:   base,
		value: "30",
		want:  time.Now().Add(30 * time.Second),
	})
	tests.Add("invalid value attaches no RetryAfter", test{
		err:   base,
		value: "soon",
	})
	tests.Add("nil base error stays nil", test{
		err:   nil,
		value: "30",
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		got := withRetryAfter(tt.err, tt.value)

		var gotRetryAfter time.Time
		if ra, ok := yerrors.AsType[interface{ RetryAfter() time.Time }](got); ok {
			gotRetryAfter = ra.RetryAfter()
		}
		if d := gotRetryAfter.Sub(tt.want); d < 0 || d > time.Second {
			t.Errorf("RetryAfter: got %v, want within 1s at or after %v", gotRetryAfter, tt.want)
		}
	})
}

func TestConnectionError(t *testing.T) {
	t.Parallel()

	type test struct {
		ctx        context.Context
		err        error
		wantErr    string
		wantStatus int // yerrors.HTTPStatus of the result; 0 means 200 (nil result)
	}

	tests := testy.NewTable[test]()

	tests.Add("a nil error yields nil", test{})
	tests.Add("a genuine error is tagged 502", test{
		err:        errors.New("boom"),
		wantErr:    "boom",
		wantStatus: http.StatusBadGateway,
	})
	tests.AddFunc("the context's cancellation cause is left status-less", func(t *testing.T) test {
		cause := errors.New("shutdown")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(cause)
		return test{
			ctx:        ctx,
			err:        cause,
			wantErr:    "shutdown",
			wantStatus: http.StatusInternalServerError, // status-less: HTTPStatus defaults to 500
		}
	})
	tests.AddFunc("a genuine error during a cancelled context is still tagged 502", func(t *testing.T) test {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errors.New("shutdown"))
		return test{
			ctx:        ctx,
			err:        errors.New("real failure"), // does not wrap the cancellation cause
			wantErr:    "real failure",
			wantStatus: http.StatusBadGateway,
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		ctx := tt.ctx
		if ctx == nil {
			ctx = t.Context()
		}
		got := connectionError(ctx, tt.err)
		if !testy.ErrorMatchesRE(tt.wantErr, got) {
			t.Errorf("unexpected error: got %v, want /%s/", got, tt.wantErr)
		}
		if want := cmp.Or(tt.wantStatus, http.StatusOK); yerrors.HTTPStatus(got) != want {
			t.Errorf("status: got %d, want %d", yerrors.HTTPStatus(got), want)
		}
	})
}

func TestListModels(t *testing.T) {
	t.Parallel()

	type test struct {
		cfg        providers.Config
		ctx        context.Context
		wantModels []providers.Model
		wantErr    string
		wantStatus int
	}

	jsonHeaders := http.Header{"Content-Type": {"application/json"}}

	tests := testy.NewTable[test]()

	tests.AddFunc("should return parsed models on 200", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusOK, jsonHeaders,
			`{"data":[{"id":"gpt-4","object":"model","created":1687882411,"owned_by":"openai"},{"id":"gpt-4o","object":"model","created":1715367049,"owned_by":"openai"}]}`)
		return test{
			cfg: providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantModels: []providers.Model{
				{ID: "gpt-4", Created: 1687882411, OwnedBy: "openai"},
				{ID: "gpt-4o", Created: 1715367049, OwnedBy: "openai"},
			},
		}
	})
	tests.AddFunc("should return error when backend returns non-200", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusUnauthorized, jsonHeaders, `{"error":"invalid api key"}`)
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:    "401",
			wantStatus: http.StatusUnauthorized,
		}
	})
	tests.AddFunc("should carry the upstream message on non-200", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusNotFound, jsonHeaders, `{"error":{"message":"model not found"}}`)
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:    "model not found",
			wantStatus: http.StatusNotFound,
		}
	})
	tests.AddFunc("should return error when response body is not valid JSON", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusOK, jsonHeaders, `not-json-at-all{`)
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantErr:    "invalid character",
			wantStatus: http.StatusBadGateway,
		}
	})
	tests.Add("should return error when backend is unreachable", test{
		cfg:        providers.Config{BaseURL: mustParseURL(t, "http://127.0.0.1:1"), APIKey: "sk-test"},
		wantErr:    "connect",
		wantStatus: http.StatusBadGateway,
	})
	tests.AddFunc("should return ctx.Err() when context is cancelled", func(t *testing.T) test {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		srvURL, _ := newCapturingServer(t, http.StatusOK, jsonHeaders, `{"data":[]}`)
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			ctx:        ctx,
			wantErr:    "context canceled",
			wantStatus: http.StatusInternalServerError,
		}
	})
	tests.AddFunc("should return empty slice when data array is empty", func(t *testing.T) test {
		srvURL, _ := newCapturingServer(t, http.StatusOK, jsonHeaders, `{"data":[]}`)
		return test{
			cfg:        providers.Config{BaseURL: mustParseURL(t, srvURL), APIKey: "sk-test"},
			wantModels: []providers.Model{},
		}
	})
	tests.AddFunc("should use Config.Client when provided", func(t *testing.T) test {
		client := testy.HTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"sentinel-model","created":1,"owned_by":"sentinel-owner"}]}`)),
				Header:     http.Header{"Content-Type": {"application/json"}},
			}, nil
		})
		return test{
			cfg: providers.Config{BaseURL: mustParseURL(t, "http://nonexistent.invalid"), APIKey: "sk-test", HTTPClient: client},
			wantModels: []providers.Model{
				{ID: "sentinel-model", Created: 1, OwnedBy: "sentinel-owner"},
			},
		}
	})

	tests.Parallel()
	tests.Run(t, func(t *testing.T, tt test) {
		ctx := tt.ctx
		if ctx == nil {
			ctx = t.Context()
		}
		gotModels, err := Models(ctx, tt.cfg)
		if !testy.ErrorMatchesRE(tt.wantErr, err) {
			t.Errorf("unexpected error: got %v, want /%s/", err, tt.wantErr)
		}
		gotStatus := yerrors.HTTPStatus(err)
		if want := cmp.Or(tt.wantStatus, http.StatusOK); gotStatus != want {
			t.Errorf("status: got %d, want %d", gotStatus, want)
		}
		if err != nil {
			return
		}

		if d := gocmp.Diff(tt.wantModels, gotModels); d != "" {
			t.Errorf("unexpected models (-want +got):\n%s", d)
		}
	})
}

func TestUpstreamTraceSummarize(t *testing.T) {
	t.Parallel()

	// The opencode-go-via-proxy failure shape: the request was written to the
	// upstream but no first response byte ever arrived. summarize must name
	// both facts so the "upstream_status: None" blind spot becomes legible in
	// the request-log error field.
	tr := &upstreamTrace{
		gotConn:      time.Unix(0, 1),
		wroteRequest: time.Unix(0, 2),
		// firstByte zero: the upstream never answered.
	}
	got := tr.summarize()
	if !strings.Contains(got, "wrote") || !strings.Contains(got, "no_first_byte") {
		t.Errorf("summarize() = %q; want it to state the request was written but no first byte arrived", got)
	}
}
