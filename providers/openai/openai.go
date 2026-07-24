// Package openai provides an OpenAI-compatible provider for the understudy system.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitlab.com/flimzy/yerrors"

	"github.com/lindyloopdev/understudy/providers"
)

const (
	chatCompletionsPath = "chat/completions"
	modelsPath          = "models"

	// maxErrorBodyBytes bounds the upstream error message included in returned
	// errors so a runaway backend cannot inflate error strings unboundedly.
	maxErrorBodyBytes = 1 << 10
)

// defaultCallTimeout bounds a non-streaming upstream call (Models). Chat
// streams its response body, so it is bounded by the transport's header
// timeout plus the response-body idle watchdog, not an overall deadline.
const defaultCallTimeout = 30 * time.Second

// connectionError tags err as 502 Bad Gateway, the proxy-boundary status for an
// upstream interaction that failed without a status of its own. An err that is
// the context's cancellation cause is returned unchanged and status-less: a
// cancellation is a lifecycle/client signal, not a gateway failure. A nil err
// yields nil.
func connectionError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Cause(ctx)) {
		return err
	}
	return yerrors.WithHTTPStatus(http.StatusBadGateway, err)
}

func newRequest(ctx context.Context, cfg providers.Config, method, path string, body io.Reader) (*http.Request, error) {
	u := cfg.BaseURL.JoinPath(path).String()
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	return req, nil
}

type openAIError struct {
	Message string              `json:"message"`
	Type    string              `json:"type"`
	Code    string              `json:"code"`
	Details []openAIErrorDetail `json:"details"`
}

type openAIErrorDetail struct {
	Violations []openAIQuotaViolation `json:"violations"`
	RetryDelay string                 `json:"retryDelay"`
}

type openAIQuotaViolation struct {
	QuotaID    string `json:"quotaId"`
	QuotaValue string `json:"quotaValue"`
}

type openAIErrorEnvelope struct {
	Error openAIError `json:"error"`
}

type errorTypeError struct {
	error
	errorType string
}

// ErrorType returns the OpenAI envelope error type carried by this error.
func (e errorTypeError) ErrorType() string { return e.errorType }

func (e errorTypeError) Unwrap() error { return e.error }

type retryAfterError struct {
	error
	retryAfter time.Time
}

// RetryAfter returns the retry time advertised by the upstream.
func (e retryAfterError) RetryAfter() time.Time { return e.retryAfter }

func (e retryAfterError) Unwrap() error { return e.error }

// errorFromResponse turns an upstream non-2xx response into an error carrying
// the client-facing HTTP status. An upstream 5xx becomes 502 Bad Gateway
// because the upstream, not us, is at fault; a 4xx is carried verbatim so
// callers can act on it (429/Retry-After, 400, 404, ...). now is the reference
// time for resolving a per-day quota exhaustion to its reset boundary (the next
// midnight America/Los_Angeles).
func errorFromResponse(resp *http.Response, now time.Time) error {
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var env openAIErrorEnvelope
	_ = json.Unmarshal(bodyBytes, &env)
	msg := env.Error.Message
	if msg == "" {
		msg = string(bodyBytes)
	}
	if len(msg) > maxErrorBodyBytes {
		msg = msg[:maxErrorBodyBytes]
	}
	clientStatus := resp.StatusCode
	if resp.StatusCode >= 500 {
		clientStatus = http.StatusBadGateway
	}
	errType := env.Error.Type
	if errType == "" {
		errType = typeForStatus(resp.StatusCode)
	}
	err := yerrors.WithHTTPStatusf(clientStatus, "upstream returned status %d: %s", resp.StatusCode, msg)
	attrs := []any{
		slog.Int("upstream_status", resp.StatusCode),
		slog.String("upstream_error_type", errType),
	}
	if env.Error.Message != "" {
		attrs = append(attrs, slog.String("upstream_error_message", env.Error.Message))
	}
	if env.Error.Code != "" {
		attrs = append(attrs, slog.String("upstream_error_code", env.Error.Code))
	}
	var quotaIDs, quotaValues []string
	var retryDelay string
	for _, d := range env.Error.Details {
		for _, v := range d.Violations {
			if v.QuotaID != "" {
				quotaIDs = append(quotaIDs, v.QuotaID)
			}
			if v.QuotaValue != "" {
				quotaValues = append(quotaValues, v.QuotaValue)
			}
		}
		if retryDelay == "" && d.RetryDelay != "" {
			retryDelay = d.RetryDelay
		}
	}
	if len(quotaIDs) > 0 {
		attrs = append(attrs, slog.String("upstream_quota_id", strings.Join(quotaIDs, ",")))
	}
	if len(quotaValues) > 0 {
		attrs = append(attrs, slog.String("upstream_quota_value", strings.Join(quotaValues, ",")))
	}
	if retryDelay != "" {
		attrs = append(attrs, slog.String("upstream_quota_retry_delay", retryDelay))
	}
	err = yerrors.With(err, attrs...)
	err = withPerDayQuotaRetryAfter(err, env.Error, now)
	err = withRetryAfter(err, resp.Header.Get("Retry-After"))
	err = withGeminiQuotaRetryAfter(err, msg)
	err = errorTypeError{error: err, errorType: errType}
	return err
}

// withPerDayQuotaRetryAfter attaches a RetryAfter() of the next midnight in
// America/Los_Angeles after now when a quota violation names a per-day quota,
// overriding the misleading short prose delay Google attaches to it. err is
// returned unchanged when no per-day violation is present or the zone cannot
// be loaded.
func withPerDayQuotaRetryAfter(err error, apiErr openAIError, now time.Time) error {
	perDay := false
	for _, d := range apiErr.Details {
		for _, v := range d.Violations {
			if strings.Contains(v.QuotaID, "PerDay") {
				perDay = true
			}
		}
	}
	if !perDay {
		return err
	}
	local := now.In(pacificLocation)
	next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, pacificLocation)
	return retryAfterError{error: err, retryAfter: next}
}

// pacificLocation is the IANA zone whose civil midnight bounds the Gemini
// free-tier per-day quota reset; it is loaded once at startup. A missing tz
// database is an unrecoverable environment fault, so loading panics rather than
// silently degrading the per-day retry time.
var pacificLocation = func() *time.Location {
	l, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return l
}()

// geminiRetryDelayRE extracts the retry delay Gemini reports as prose in a
// RESOURCE_EXHAUSTED quota message ("Please retry in 22.509013813s."). The compat
// 429 carries no Retry-After header; it does also carry a structured
// RetryInfo.retryDelay (parsed and logged elsewhere), but that field has appeared
// and disappeared across endpoint changes, so prose remains the stable source for
// RetryAfter. See notes/2026-07-12-gemini-compat-passes-structured-ratelimit-details.md.
var geminiRetryDelayRE = regexp.MustCompile(`retry in ([0-9.]+)s`)

// withGeminiQuotaRetryAfter attaches a RetryAfter() parsed from a Gemini quota
// message's prose delay, but only when err does not already carry one and msg
// signals a Google quota exhaustion.
func withGeminiQuotaRetryAfter(err error, msg string) error {
	if _, ok := yerrors.AsType[interface{ RetryAfter() time.Time }](err); ok {
		return err
	}
	if !strings.Contains(msg, "RESOURCE_EXHAUSTED") {
		return err
	}
	m := geminiRetryDelayRE.FindStringSubmatch(msg)
	if m == nil {
		return err
	}
	secs, perr := strconv.ParseFloat(m[1], 64)
	if perr != nil {
		return err
	}
	return retryAfterError{error: err, retryAfter: time.Now().Add(time.Duration(secs * float64(time.Second)))}
}

// typeForStatus returns the OpenAI envelope error type implied by the upstream
// HTTP status when the response body does not include an explicit type.
func typeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}

// withRetryAfter wraps err so it implements RetryAfter() time.Time, parsing v
// (a Retry-After header value) as either delta-seconds or an HTTP-date. For an
// empty or unparseable value, err is returned unchanged. A nil err yields nil.
func withRetryAfter(err error, v string) error {
	if err == nil {
		return nil
	}
	if secs, perr := strconv.Atoi(v); perr == nil {
		return retryAfterError{error: err, retryAfter: time.Now().Add(time.Duration(secs) * time.Second)}
	}
	if d, derr := http.ParseTime(v); derr == nil {
		return retryAfterError{error: err, retryAfter: d}
	}
	return err
}

// Chat POSTs body to <BaseURL>/chat/completions with Bearer auth.
// Caller is responsible for closing the returned response body.
func Chat(ctx context.Context, cfg providers.Config, body io.Reader) (*http.Response, error) {
	ctx, trace := newUpstreamTrace(ctx)
	req, err := newRequest(ctx, cfg, http.MethodPost, chatCompletionsPath, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.Client().Do(req)
	if err != nil {
		return nil, connectionError(ctx, fmt.Errorf("%w: upstream %s", err, trace.summarize()))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errorFromResponse(resp, time.Now())
	}
	return resp, nil
}

// upstreamTrace records the phases an upstream HTTP call reached, so a call
// that fails with no upstream response — the proxy's "upstream_status: None"
// blind spot — still says where it stopped (never connected, wrote the request
// but got no first byte, etc.). See summarize.
type upstreamTrace struct {
	start        time.Time
	gotConn      time.Time
	wroteRequest time.Time
	firstByte    time.Time
}

// newUpstreamTrace returns a context carrying a ClientTrace that records into a
// fresh upstreamTrace, plus the trace to read back when the call ends.
func newUpstreamTrace(ctx context.Context) (context.Context, *upstreamTrace) {
	t := &upstreamTrace{start: time.Now()}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { t.gotConn = time.Now() },
		WroteRequest:         func(httptrace.WroteRequestInfo) { t.wroteRequest = time.Now() },
		GotFirstResponseByte: func() { t.firstByte = time.Now() },
	}), t
}

// summarize names the last phase the call reached. "wrote_request;no_first_byte"
// is the signature of an upstream that accepted the request and went silent —
// exactly the fact "upstream_status: None" hides.
func (t *upstreamTrace) summarize() string {
	switch {
	case !t.firstByte.IsZero():
		return "first_byte"
	case !t.wroteRequest.IsZero():
		return "wrote_request;no_first_byte"
	case !t.gotConn.IsZero():
		return "got_conn;no_request_sent"
	default:
		return "no_conn"
	}
}

// Models GETs <BaseURL>/models with Bearer auth and returns the parsed model
// list, bounding the call with the default timeout.
func Models(ctx context.Context, cfg providers.Config) ([]providers.Model, error) {
	return models(ctx, cfg, defaultCallTimeout)
}

// models is Models bounded by an explicit timeout (the test seam).
func models(ctx context.Context, cfg providers.Config, timeout time.Duration) ([]providers.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := newRequest(ctx, cfg, http.MethodGet, modelsPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cfg.Client().Do(req)
	if err != nil {
		return nil, connectionError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errorFromResponse(resp, time.Now())
	}
	defer resp.Body.Close()
	var result modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, yerrors.WithHTTPStatus(http.StatusBadGateway, err)
	}
	return result.Data, nil
}

type modelsResponse struct {
	Data []providers.Model `json:"data"`
}
