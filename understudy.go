// Package understudy provides an HTTP server that proxies OpenAI-compatible
// API requests, enforcing token validation on each request.
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
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.com/flimzy/yerrors"

	"github.com/lindyloopdev/understudy/internal/jsonstream"
	"github.com/lindyloopdev/understudy/providers"
	"github.com/lindyloopdev/understudy/providers/openai"
)

// ProviderOpenAI is the [Backend.ProviderType] value for the OpenAI
// provider type. New provider types add their own constant alongside this one.
const ProviderOpenAI = "openai"

// openaiProvider implements [providers.Handler] over the openai package.
type openaiProvider struct{}

func (openaiProvider) Chat(ctx context.Context, cfg providers.Config, body io.Reader) (*http.Response, error) {
	return openai.Chat(ctx, cfg, body)
}

func (openaiProvider) Models(ctx context.Context, cfg providers.Config) ([]providers.Model, error) {
	return openai.Models(ctx, cfg)
}

// Backend is one uniquely-named upstream the proxy can route to. ProviderType
// selects the registered [providers.Handler]; Config carries the connection details.
type Backend struct {
	ProviderType string
	Config       providers.Config
}

// BackendConfig is the per-token routing result: the set of backends a token
// resolves to, keyed by each backend's unique operator-chosen name.
type BackendConfig struct {
	Backends map[string]Backend

	// Models maps a logical model name to its ordered failover targets.
	Models map[string]LogicalModel
}

// LogicalModel is a named model whose requests are routed across an ordered
// list of concrete targets, failing over from one to the next on failure.
type LogicalModel struct {
	Targets []Target
}

// TokenValidator validates a bearer token extracted from an incoming request.
type TokenValidator interface {
	Validate(ctx context.Context, token string) (*BackendConfig, error)
}

type backendKey struct{}

func backendFromContext(ctx context.Context) *BackendConfig {
	v, _ := ctx.Value(backendKey{}).(*BackendConfig)
	return v
}

type tokenKey struct{}

func tokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tokenKey{}).(string)
	return v
}

// maxPrefixScan bounds the number of bytes the proxy scans for the top-level
// "model" field before the tripwire fires. Requests with a "model" key beyond
// this offset still proceed normally, but an ERROR is logged so operators can
// investigate unexpectedly large prefixes.
const maxPrefixScan = 64 << 10

type countingReader struct {
	io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// defaultFailoverThreshold is how long a target may keep failing before
// pickTarget routes around it to the next target in a logical model.
const defaultFailoverThreshold = 15 * time.Second

// defaultRecoveryInterval is how long pickTarget waits between half-open
// re-probes of a demoted target, so a recovered target rejoins the failover
// walk instead of being stranded on the fallback forever.
const defaultRecoveryInterval = 30 * time.Second

// defaultHeaderStallGate bounds how long understudy waits for an upstream's first
// response header before treating the attempt as a pre-header stall — backpressure
// from a busy target — rather than a slow success. Shorter than the transport's
// 60s ResponseHeaderTimeout so a stalled request is routed around well before the
// transport would give up. Provisional; see
// TODO.d/understudy-adaptive-coordinated-backoff.md.
const defaultHeaderStallGate = 20 * time.Second

// synthesizedStallBackoff is the bounded Retry-After understudy synthesizes for a
// pre-header stall, which carries none: it benches the stalled target until the
// interval elapses. Provisional; see
// TODO.d/understudy-adaptive-coordinated-backoff.md.
const synthesizedStallBackoff = 30 * time.Second

// errHeaderStall marks an upstream attempt cancelled by the header-stall gate.
var errHeaderStall = errors.New("upstream produced no response header before the stall gate")

// targetHealth tracks a target's failure streak: failingSince is when the streak
// began; lastProbe is when the target was last attempted; readmitAt is a known
// re-admission time from an advertised Retry-After, or zero if none; downLogged
// is whether the "backend down" transition has been logged; lastTouch is when
// the entry was last written, the age the eviction sweep measures.
type targetHealth struct {
	failingSince time.Time
	lastProbe    time.Time
	readmitAt    time.Time
	downLogged   bool
	lastTouch    time.Time
}

// healthTTL is how long a health entry may sit untouched before the
// sweep drops it. An entry is otherwise removed only by a success through its
// own canonical (url + key + model), so a rotated credential or a withdrawn
// backend strands one forever — no live target can produce the key again. Age is
// the only proxy understudy has for that, since the backend set arrives per
// request. It is set far above the recovery-probe schedule so a demotion that is
// still being probed is never mistaken for a stranded one.
const healthTTL = 24 * time.Hour

// defaultMaxConcurrentPerUpstream is the per-account limiter's cold-start
// allowance. It is a starting point, not a ceiling: grow() raises the cap toward
// the upstream's real capacity on success (shrink()/throttle() pull it back on a
// 429), with the process-wide FD budget as the hard backstop.
const defaultMaxConcurrentPerUpstream = 20

// maxRequestBodyBytes caps a buffered chat-completions request body — set well
// above any legitimate body, so it guards only against memory-exhausting
// abusive payloads.
const maxRequestBodyBytes = 32 << 20

// FD-budget sizing for the process-wide concurrency limiter. Each in-flight
// upstream request costs roughly fdsPerSlot file descriptors (the upstream dial,
// with headroom); fdSlotReserve holds back descriptors for the listener, log
// files, and idle keep-alive connections. The budget is (soft RLIMIT_NOFILE −
// reserve) / fdsPerSlot, so the process is bounded by the resource that actually
// binds — descriptors, shared across all upstreams — rather than a per-account
// guess.
const (
	fdSlotReserve uint64 = 64
	fdsPerSlot    uint64 = 2
	// defaultFDSoftLimitFallback sizes the budget when RLIMIT_NOFILE cannot be
	// read (a non-Linux embedder or a sandbox), rather than failing construction.
	defaultFDSoftLimitFallback uint64 = 1024
)

// TODO(TODO.d/understudy-process-budget-shed.md): grow the backoff with sustained
// saturation instead of a fixed value.
const processBudgetRetryAfter = 5 * time.Second

// fdSlotBudget converts an FD soft limit into a process-wide concurrent-request
// budget, floored at 1 so an unusually small limit still admits work.
func fdSlotBudget(soft uint64) int {
	if soft <= fdSlotReserve {
		return 1
	}
	slots := (soft - fdSlotReserve) / fdsPerSlot
	if slots < 1 {
		return 1
	}
	// Unreachable in practice — a real RLIMIT_NOFILE never approaches 2^63 — but
	// guards the int conversion so a garbage-large soft limit can't wrap negative.
	if slots > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(slots)
}

type server struct {
	logger *slog.Logger
	h      http.Handler

	// providers maps a [Backend.ProviderType] to the handler that proxies
	// requests for that provider type.
	providers map[string]providers.Handler

	failoverThreshold time.Duration
	// terminalThreshold is how long a target may keep failing, with nowhere left
	// to fail over to, before understudy stops relaying the retryable failure and
	// hard-fails to the non-retryable reject.
	terminalThreshold time.Duration
	recoveryInterval  time.Duration
	headerStallGate   time.Duration

	maxConcurrentPerUpstream int

	// fdLimitReader reports the FD soft limit the process-wide budget is sized
	// from; an override lets tests drive both the read and the unavailable path.
	fdLimitReader func() (uint64, bool)
	// processLimiter bounds in-flight requests across all upstreams to the
	// FD-derived budget — the safety backstop for the per-account limiters.
	// chatCompletions sheds (503) when it is exhausted rather than queueing.
	processLimiter *upstreamLimiter

	interceptor ResponseInterceptor

	mu sync.Mutex
	// health records each target's current failure streak; a target absent from
	// the map is healthy.
	health map[string]targetHealth
	// upstreamLimiters holds a per-upstream concurrency limiter keyed by upstream
	// identity, bounding concurrent in-flight requests to each account.
	upstreamLimiters map[string]*upstreamLimiter
}

// upstreamLimiter bounds concurrent in-flight requests to one upstream. Unlike a
// buffered channel it can shrink its cap at runtime (AIMD multiplicative
// decrease on a rate limit); a broadcast channel wakes all waiters on release.
type upstreamLimiter struct {
	mu       sync.Mutex
	limit    int
	inflight int
	// knownGood is the cap the last saturated rejection measured, zero before any
	// such rejection has arrived. Halving is reserved for a rejection at or below it.
	knownGood int
	// successes accrue toward the next additive step, once the cap has climbed
	// back to the known-good boundary; a full round of them raises it by one.
	successes int
	ready     chan struct{}
}

func newUpstreamLimiter(limit int) *upstreamLimiter {
	return &upstreamLimiter{limit: limit, ready: make(chan struct{})}
}

// acquire takes a slot, blocking until one is free or ctx is done, and reports
// whether it had to wait — the demand signal that gates growth of the cap. A
// free slot is taken immediately without consulting ctx, so a request with an
// already-cancelled context but an available slot still runs and surfaces the
// handler's own cause.
func (l *upstreamLimiter) acquire(ctx context.Context) (bool, error) {
	waited := false
	for {
		l.mu.Lock()
		if l.inflight < l.limit {
			l.inflight++
			l.mu.Unlock()
			return waited, nil
		}
		ready := l.ready
		l.mu.Unlock()
		select {
		case <-ready:
			waited = true
		case <-ctx.Done():
			return waited, context.Cause(ctx)
		}
	}
}

// tryAcquire takes a slot if one is free and reports whether it did, never
// blocking — the non-blocking counterpart to acquire, for callers that shed
// rather than wait when the limiter is full.
func (l *upstreamLimiter) tryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight < l.limit {
		l.inflight++
		return true
	}
	return false
}

func (l *upstreamLimiter) release() {
	l.mu.Lock()
	l.inflight--
	l.wake()
	l.mu.Unlock()
}

// inFlight reports the current number of in-flight slots held.
func (l *upstreamLimiter) inFlight() int {
	l.mu.Lock()
	n := l.inflight
	l.mu.Unlock()
	return n
}

// shrink halves the cap, floored at 1 (multiplicative decrease on a rate limit).
func (l *upstreamLimiter) shrink() {
	l.mu.Lock()
	if l.limit > 1 {
		l.limit /= 2
		if l.limit < 1 {
			l.limit = 1
		}
	}
	l.mu.Unlock()
}

// throttle reacts to a signal-less rate limit. A rejection arriving at
// saturation is a capacity measurement — the account's limit sits just below the
// count in flight at that moment — so the cap lands one slot under it and that
// value is remembered as known-good. Halving is reserved for a rejection at or
// below the known-good boundary, where the measurement itself is in doubt.
// Arriving under the cap measures nothing about capacity, but the count in
// flight is still an upper bound the cap is pulled down to.
func (l *upstreamLimiter) throttle() {
	l.mu.Lock()
	switch {
	case l.knownGood > 0 && l.inflight <= l.knownGood:
		l.mu.Unlock()
		l.shrink()
		return
	case l.inflight < l.limit:
		l.limit = l.inflight
	case l.inflight > 1:
		l.measure()
	}
	l.mu.Unlock()
}

// measure sets the cap one slot below the in-flight count and records it as the
// known-good boundary; caller holds l.mu.
func (l *upstreamLimiter) measure() {
	l.limit = l.inflight - 1
	l.knownGood = l.limit
}

// grow raises the cap on a success that waited for a slot; no per-account
// ceiling — the process-wide FD budget is the hard backstop. Below the
// known-good boundary the estimate is far from the edge, so one slot per success
// is right: while saturated that doubles the cap each round trip. At or above
// the boundary the edge is near and overshoot is paid for in real rejections, so
// growth drops to one slot per round of successes.
func (l *upstreamLimiter) grow() {
	l.mu.Lock()
	if l.knownGood > 0 && l.limit >= l.knownGood {
		l.successes++
		if l.successes < l.limit {
			l.mu.Unlock()
			return
		}
		l.successes = 0
	}
	l.limit++
	l.wake()
	l.mu.Unlock()
}

// wake wakes all current waiters; caller holds l.mu.
func (l *upstreamLimiter) wake() {
	close(l.ready)
	l.ready = make(chan struct{})
}

// RequestMetadata carries understudy's metadata about a relayed request: the
// backend and model that served it, and the bearer token the request carried.
type RequestMetadata struct {
	Backend string
	Model   string
	Token   string
}

// ResponseInterceptor may mutate an upstream *http.Response in place before
// understudy relays it to the client. served identifies the target that served
// the request.
//
// If it replaces resp.Body, the replacement's Close must close through to the
// original body. understudy releases the upstream connection by closing
// resp.Body after relay, so a replacement that does not close through leaks the
// connection.
type ResponseInterceptor func(ctx context.Context, served RequestMetadata, resp *http.Response) error

// Option configures a server built by New.
type Option func(*server)

// WithResponseInterceptor registers fn to be invoked on each upstream response
// before it is relayed to the client.
func WithResponseInterceptor(fn ResponseInterceptor) Option {
	return func(s *server) {
		s.interceptor = fn
	}
}

// WithLogger routes the server's operational logs to logger instead of the
// process-default [slog.Default].
func WithLogger(logger *slog.Logger) Option {
	return func(s *server) {
		s.logger = logger
	}
}

// withFDSoftLimit makes the FD-limit read report soft, so tests can size the
// process budget deterministically without reading the host RLIMIT_NOFILE.
func withFDSoftLimit(soft uint64) Option {
	return func(s *server) {
		s.fdLimitReader = func() (uint64, bool) { return soft, true }
	}
}

// withoutFDSoftLimit makes the FD-limit read report unavailable, so tests can
// drive the fallback path without a host on which RLIMIT_NOFILE is unreadable.
func withoutFDSoftLimit() Option {
	return func(s *server) {
		s.fdLimitReader = func() (uint64, bool) { return 0, false }
	}
}

// WithProvider registers h to serve backends whose [Backend.ProviderType] is
// name. A nil h panics, as does a second registration of the same name. A
// single registration may override the OpenAI default.
func WithProvider(name string, h providers.Handler) Option {
	if h == nil {
		panic(fmt.Sprintf("understudy: WithProvider(%q): nil handler", name))
	}
	return func(s *server) {
		if _, dup := s.providers[name]; dup {
			panic(fmt.Sprintf("understudy: WithProvider(%q): duplicate registration", name))
		}
		s.providers[name] = h
	}
}

// defaultProviders is the provider set understudy serves when an embedder
// registers none of its own. Seeded after options so a [WithProvider] override
// of a default name is not itself seen as a duplicate registration.
func defaultProviders() map[string]providers.Handler {
	return map[string]providers.Handler{ProviderOpenAI: openaiProvider{}}
}

// New returns a new HTTP handler that uses v to validate bearer tokens.
func New(v TokenValidator, opts ...Option) http.Handler {
	s := newServer(v, opts...)
	for name, h := range defaultProviders() {
		if _, ok := s.providers[name]; !ok {
			s.providers[name] = h
		}
	}
	return s
}

// newServer builds a fully-wired *server, which satisfies http.Handler via
// ServeHTTP.
func newServer(v TokenValidator, opts ...Option) *server {
	s := &server{
		logger:                   slog.Default(), //nolint:forbidigo // deliberate fallback when no WithLogger option is given; the rule targets implicit logging elsewhere
		providers:                make(map[string]providers.Handler),
		failoverThreshold:        defaultFailoverThreshold,
		terminalThreshold:        maxPassthroughRetryAfter,
		recoveryInterval:         defaultRecoveryInterval,
		headerStallGate:          defaultHeaderStallGate,
		maxConcurrentPerUpstream: defaultMaxConcurrentPerUpstream,
		fdLimitReader:            readFDSoftLimit,
		health:                   make(map[string]targetHealth),
		upstreamLimiters:         make(map[string]*upstreamLimiter),
	}
	for _, opt := range opts {
		opt(s)
	}
	soft, ok := s.fdLimitReader()
	if !ok {
		soft = defaultFDSoftLimitFallback
	}
	s.processLimiter = newUpstreamLimiter(fdSlotBudget(soft))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", errToResponse(recoverPanic(s.chatCompletions)))
	mux.HandleFunc("GET /v1/models", errToResponse(recoverPanic(s.models)))
	s.h = withStatusRecorder(authMiddleware(v, mux))
	return s
}

// canonicalUpstreamKey returns the canonical per-upstream limiter key for the
// given base URL and API key. A trailing slash on the base-URL path is folded so
// the same account written with or without it shares one limiter.
func canonicalUpstreamKey(baseURL *url.URL, apiKey string) string {
	canonical := *baseURL
	canonical.Path = strings.TrimSuffix(canonical.Path, "/")
	return canonical.String() + "\x00" + apiKey
}

// healthKey is the availability key for t: the canonical upstream account (base
// URL + key) plus the model. Keying on the account rather than the operator's
// backend name means two names for one account+model share health — so a
// per-account failure demotes both — while keeping the model in the key so a
// per-model failure never demotes sibling models on the same account. A target
// naming an unresolvable backend falls back to the backend name.
func healthKey(t Target, backends map[string]Backend) string {
	b, ok := backends[t.backend]
	if !ok || b.Config.BaseURL == nil {
		return t.backend + "/" + t.model
	}
	return canonicalUpstreamKey(b.Config.BaseURL, b.Config.APIKey) + "\x00" + t.model
}

// upstreamLimiter lazily creates and returns the concurrency limiter for the
// given canonical upstream key, starting at maxConcurrentPerUpstream (the
// cold-start allowance it then floats up from).
func (s *server) upstreamLimiter(key string) *upstreamLimiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	lim, ok := s.upstreamLimiters[key]
	if !ok {
		lim = newUpstreamLimiter(s.maxConcurrentPerUpstream)
		s.upstreamLimiters[key] = lim
	}
	return lim
}

// ServeHTTP dispatches to the server's built middleware chain.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.h.ServeHTTP(w, r)
}

// pickTarget returns the first target that is usable at all and whose failure
// streak is within the failover threshold (or that has none). A target whose
// backend resolveBackend rejects is skipped before its health is consulted, and
// never demoted: only a configuration change can make it usable, so a health
// entry for it would be one no recovery probe could clear.
//
// Among the targets that remain, a target past the threshold is skipped
// until a recovery interval has elapsed since its last probe, at which point it
// is offered as a single half-open probe (stamping lastProbe so concurrent
// requests within the cooldown still skip it). A target demoted with a known
// re-admission time (readmitAt, from an advertised Retry-After) is instead
// routed around until that time, then re-admitted as a half-open probe (its
// health preserved until the probe's outcome) — never half-open-probed early. If every target is past the threshold and not
// due for a probe, or every target was skipped as unusable, it returns the last so
// a request always has somewhere to go — which means the returned target is itself
// unusable when nothing was usable.
func (s *server) pickTarget(ctx context.Context, targets []Target, backends map[string]Backend) Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.evictStaleHealth(now)
	for _, t := range targets {
		if _, err := s.resolveBackend(backends, t.backend); err != nil {
			addLogSkipped(ctx, t.backend, t.model, err)
			continue
		}
		id := healthKey(t, backends)
		h, failing := s.health[id]
		if !failing || now.Sub(h.failingSince) <= s.failoverThreshold {
			return t
		}
		if !h.readmitAt.IsZero() {
			if now.Before(h.readmitAt) {
				s.noteBackendDown(id, t, h)
				continue
			}
			h.readmitAt = time.Time{}
			h.lastProbe = now
			h.lastTouch = now
			s.health[id] = h
			return t
		}
		if now.Sub(h.lastProbe) >= s.recoveryInterval {
			h.lastProbe = now
			h.lastTouch = now
			s.health[id] = h
			return t
		}
		s.noteBackendDown(id, t, h)
	}
	// TODO(TODO.d/degrade-past-a-misconfigured-backend.md): the last target is an
	// arbitrary one to hand back when every candidate was excluded — the caller
	// reads whichever backend sorts last rather than the first that failed.
	return targets[len(targets)-1]
}

// untriedTargets returns the targets whose availability key is absent from
// tried, preserving order. It returns an empty slice once every target has been
// tried. Because the key is the canonical account+model, targets on one account
// share a key: trying one marks its same-account siblings tried too.
func untriedTargets(targets []Target, tried []string, backends map[string]Backend) []Target {
	if len(tried) == 0 {
		return targets
	}
	remaining := make([]Target, 0, len(targets))
	for _, t := range targets {
		if !slices.Contains(tried, healthKey(t, backends)) {
			remaining = append(remaining, t)
		}
	}
	return remaining
}

// noteBackendDown logs the "backend down" transition for t once per failure
// streak, recording on h that it has been logged. Caller holds s.mu.
func (s *server) noteBackendDown(id string, t Target, h targetHealth) {
	if h.downLogged {
		return
	}
	s.logger.InfoContext(context.Background(), "backend down",
		slog.String("backend", t.backend),
		slog.String("model", t.model),
	)
	h.downLogged = true
	h.lastTouch = time.Now()
	s.health[id] = h
}

// evictStaleHealth drops every entry untouched for healthTTL — the ones no live
// target can reach any more, since a reachable demotion is re-stamped by the
// probes and failures that keep measuring it. Caller holds s.mu.
func (s *server) evictStaleHealth(now time.Time) {
	for id, h := range s.health {
		if now.Sub(h.lastTouch) >= healthTTL {
			delete(s.health, id)
		}
	}
}

// recordFailure marks t as failing, preserving the start of an existing streak
// so the threshold measures from the first failure, not the most recent. A new
// streak seeds lastProbe at the demotion moment (failingSince plus the failover
// threshold), so the first half-open probe waits a full recovery interval after
// the target is actually routed around, not from its first failure.
func (s *server) recordFailure(t Target, backends map[string]Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := healthKey(t, backends)
	if _, ok := s.health[id]; !ok {
		now := time.Now()
		s.health[id] = targetHealth{failingSince: now, lastProbe: now.Add(s.failoverThreshold), lastTouch: now}
	}
}

// demotedHealth builds the health of a target demoted at once: the streak is
// backdated past the failover threshold so pickTarget routes around it on the
// very next request, and lastProbe is seeded at the demotion moment so the first
// half-open re-probe still waits a full recovery interval. readmitAt is the known
// re-admission time from an advertised Retry-After, or zero for an unbounded one.
func (s *server) demotedHealth(now, readmitAt time.Time) targetHealth {
	return targetHealth{failingSince: now.Add(-s.failoverThreshold), lastProbe: now, readmitAt: readmitAt, lastTouch: now}
}

// recordImmediateFailure demotes t at once, so pickTarget routes around it on
// the very next request rather than tolerating it for a failover threshold.
func (s *server) recordImmediateFailure(t Target, backends map[string]Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := healthKey(t, backends)
	if _, ok := s.health[id]; !ok {
		s.health[id] = s.demotedHealth(time.Now(), time.Time{})
	}
}

// recordRateLimited demotes t at once like recordImmediateFailure, but records a
// known re-admission time (now plus retryAfter) from an advertised Retry-After.
// pickTarget keeps t benched until that time rather than half-open-probing it at
// the recovery interval, so a target under a timed backoff is not re-admitted
// before the backoff it advertised has elapsed.
func (s *server) recordRateLimited(t Target, retryAfter time.Duration, backends map[string]Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := healthKey(t, backends)
	now := time.Now()
	h, ok := s.health[id]
	if !ok {
		h = s.demotedHealth(now, now.Add(retryAfter))
	} else {
		h.readmitAt = now.Add(retryAfter)
		h.lastTouch = now
	}
	s.health[id] = h
}

// failingFor reports how long t's current failure streak has run, or zero when
// t is healthy.
func (s *server) failingFor(t Target, backends map[string]Backend) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.health[healthKey(t, backends)]
	if !ok {
		return 0
	}
	return time.Since(h.failingSince)
}

// terminalFailure marks err terminal — the response path rejects it as
// non-retryable rather than relaying it — when remaining is empty and t's
// streak has crossed the terminal threshold. Both are required: a long streak
// on a target the list can still route around is a demoted target, not an
// exhausted list.
func (s *server) terminalFailure(t Target, remaining []Target, backends map[string]Backend, err error) error {
	if t.backend == "" || len(remaining) > 0 || s.failingFor(t, backends) < s.terminalThreshold {
		return err
	}
	return terminalError{err}
}

// terminalError marks an upstream failure understudy has stopped relaying. It
// carries the verdict only — the backoff the reject advertises is the response
// path's to decide.
type terminalError struct{ error }

// Unwrap returns the underlying error so that errors.Is/As and
// yerrors.HTTPStatus can still traverse the chain.
func (e terminalError) Unwrap() error { return e.error }

// clearFailure marks t healthy, ending any failure streak. It emits the
// "backend up" transition iff the streak was previously announced "backend
// down", keeping the up/down log pair symmetric.
func (s *server) clearFailure(t Target, backends map[string]Backend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := healthKey(t, backends)
	if h, ok := s.health[id]; ok && h.downLogged {
		s.logger.InfoContext(context.Background(), "backend up",
			slog.String("backend", t.backend),
			slog.String("model", t.model),
		)
	}
	delete(s.health, id)
}

// clientFacing maps an error returned by a provider call into the status the
// client is shown: an upstream 5xx becomes 502 Bad Gateway, because the
// upstream, not understudy, is at fault, while a 4xx passes verbatim so a client
// can act on it (429/Retry-After, 400, 404, ...). A cancellation cause is the
// caller's own error travelling back out — a consumer's shutdown 503, say — so
// it is surfaced untouched.
func clientFacing(ctx context.Context, err error) error {
	if errors.Is(err, context.Cause(ctx)) {
		return err
	}
	if yerrors.HTTPStatus(err) >= 500 {
		return yerrors.WithHTTPStatus(http.StatusBadGateway, err)
	}
	return err
}

// isFatalUpstream reports whether err is an upstream failure that should count
// against a target's health: an upstream 5xx, or a connection failure that never
// reached one, which the provider raises as 502.
func isFatalUpstream(err error) bool {
	return yerrors.HTTPStatus(err) >= 500
}

// isAccessRefused reports whether the upstream refused the account the target
// (401: identity rejected, 402: out of funds, 403: not permitted). Each is a
// standing property of the account, not of the request, so retrying the same
// target only refuses again.
func isAccessRefused(err error) bool {
	switch yerrors.HTTPStatus(err) {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return true
	}
	return false
}

// rateLimitDemotionThreshold is the Retry-After delay at or above which a 429 is
// treated as the target being unhealthy rather than a brief throttle: such a
// target is demoted immediately so requests fail over to a fallback instead of
// stalling on it for the length of the advertised backoff.
const rateLimitDemotionThreshold = 30 * time.Second

// synthesizedRateLimitRetryAfter is the backoff understudy advertises to the
// client for a 429 the upstream left unbounded (no Retry-After), so the client
// waits instead of retrying immediately.
const synthesizedRateLimitRetryAfter = 60 * time.Second

// limitCondition classifies a backpressure error by the nature of the limit the
// upstream signalled, so callers derive the response and concurrency handling
// from one condition rather than re-reading the Retry-After facts.
type limitCondition int

const (
	// notRateLimited is anything that is not a 429 (e.g. a 5xx rendered as 502):
	// no rate-limit handling applies.
	notRateLimited limitCondition = iota
	// transientRate is a 429 with a short Retry-After (< rateLimitDemotionThreshold):
	// a brief throttle the client waits out; neither demote nor shrink.
	transientRate
	// sustainedRate is a 429 with a Retry-After at/beyond rateLimitDemotionThreshold:
	// a timed backoff long enough to treat the target as unhealthy and demote it,
	// but not a concurrency limit — so it does not shrink the cap.
	sustainedRate
	// signalless is a 429 with no Retry-After — the ambiguous, unsignalled case
	// (z.ai-shaped): it may be concurrency or an exhausted quota. It throttles the
	// cap (see throttle); whether it also demotes turns on the in-flight count (see
	// chatCompletions).
	signalless
)

// limitClassification is understudy's classification of an upstream backpressure error
// (a rate-limit 429, or a 5xx) into the facts the response path
// and the concurrency limiter act on. classifyLimit is the single place this
// classification lives.
type limitClassification struct {
	// status is the error's HTTP status (via responseStatus): the upstream's own
	// where the error has not yet passed clientFacing, the client's where it has.
	// Only isRateLimit reads it, and a 429 is the same either side of the map.
	status int
	// isRateLimit reports status == 429 Too Many Requests.
	isRateLimit bool
	// hasRetryAfter reports that the upstream advertised a Retry-After.
	hasRetryAfter bool
	// retryAfter is the remaining Retry-After delay (valid when hasRetryAfter).
	retryAfter time.Duration
	// shouldReject reports a failure understudy fails fast rather than forwards:
	// a Retry-After beyond maxPassthroughRetryAfter, or a failure the walk gave up
	// on (a terminalError), which carries no Retry-After condition at all.
	shouldReject bool
	// condition is the nature of the limit; the shrink path reads it to distinguish
	// a concurrency limit (signalless) from a timed backoff.
	condition limitCondition
}

// classifyLimit inspects err's status and Retry-After once, so callers act on a
// single signal instead of re-deriving the classification.
func classifyLimit(err error) limitClassification {
	if err == nil {
		return limitClassification{}
	}
	sig := limitClassification{status: responseStatus(err)}
	sig.isRateLimit = sig.status == http.StatusTooManyRequests
	if ra, ok := errors.AsType[interface {
		error
		RetryAfter() time.Time
	}](err); ok {
		sig.hasRetryAfter = true
		sig.retryAfter = time.Until(ra.RetryAfter())
	}
	sig.shouldReject = sig.hasRetryAfter && sig.retryAfter > maxPassthroughRetryAfter
	// A failure the walk gave up on rejects on its own terms: the streak, not an
	// advertised Retry-After, is what crossed the threshold.
	if _, ok := errors.AsType[terminalError](err); ok {
		sig.shouldReject = true
		// Only a failure carrying no upstream backoff needs a synthesized one; the
		// upstream's own delay is the honest value wherever it sent one.
		if !sig.hasRetryAfter {
			sig.retryAfter = maxPassthroughRetryAfter
		}
	}
	switch {
	case !sig.isRateLimit:
		sig.condition = notRateLimited
	case !sig.hasRetryAfter:
		sig.condition = signalless
	case sig.retryAfter >= rateLimitDemotionThreshold:
		sig.condition = sustainedRate
	default:
		sig.condition = transientRate
	}
	return sig
}

// statusRecorder captures whether (and with what status) a response has been
// committed, so errToResponse can detect an already-written response and refrain
// from rendering its error envelope over it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	// A body-only write commits a 200; recording it lets errToResponse see the
	// response is already committed.
	r.status = cmp.Or(r.status, http.StatusOK)
	return r.ResponseWriter.Write(p)
}

// LogRecord carries the per-request telemetry only understudy can supply: which
// backend and model served, the upstream status, and the real error behind an
// obfuscated body. Generic HTTP facts (response status, byte counts) are
// deliberately excluded — they are not understudy's to record. Install with
// WithLogCtx and read back with LogRecordFromContext.
type LogRecord struct {
	// Err is the request's error, or nil.
	Err error
	// BackendName is the backend that served the request, or "".
	BackendName string
	// ModelRequested and ModelUpstream are the requested and upstream model names.
	ModelRequested string
	ModelUpstream  string
	// UpstreamStatus is the upstream response status, or 0.
	UpstreamStatus int
	// Excluded holds what the request considered and did not serve from: targets a
	// failover abandoned, targets excluded as unusable before any call, and the
	// backends a listing could not use. A listing whose catalog fetch fails is not
	// here — that reaches understudy's own logger alone. A chat request records
	// them in the order it walked its candidates, so an exclusion and a failover
	// interleave as they happened; a listing ranges a map and has no order to
	// report. It is empty for a request that
	// served from its first target. A demotion is attributable through it: the
	// target it demoted is here when the request moved on, and in the fields above
	// when there was nowhere left to go. A skipped backend appears on every request
	// that routes around it, since only a configuration change can clear it.
	Excluded []Attempt
}

// Attempt describes one target or backend a request considered and did not use,
// so a failover leaves a record of them rather than only the one that determined
// the client's outcome. Called separates the two ways that happens: understudy
// called it and abandoned the result, or could not use it and never called.
type Attempt struct {
	// Backend is the backend considered.
	Backend string
	// Called reports whether understudy issued a request to it. False means the
	// backend was unusable as configured and Err says why; nothing was sent.
	Called bool
	// ModelUpstream is the upstream model name the attempt requested. Empty when
	// nothing was called, and for a listing, which names no model.
	ModelUpstream string
	// UpstreamStatus is the status the attempt answered with, or 0 when it never
	// produced a response — a pre-header stall, a transport failure, or a backend
	// that was never called.
	UpstreamStatus int
	// Err is the error that ended the attempt, carrying the upstream's own
	// message where it sent one, or why the backend could not be used at all.
	Err error
}

type logCtxKey struct{}

// WithLogCtx derives a context carrying a fresh LogRecord understudy's handlers
// populate, returning the derived context. A mount installs it before serving and
// reads it back with LogRecordFromContext on the way out.
func WithLogCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, logCtxKey{}, &LogRecord{})
}

// LogRecordFromContext returns a value copy of the request's log record and true,
// or the zero LogRecord and false when ctx carries no record (a unit test, or a
// mount that declined to install one). The bool distinguishes an absent record
// from a present-but-empty one. Returning by value keeps the live record
// understudy-private.
func LogRecordFromContext(ctx context.Context) (LogRecord, bool) {
	if h := logCtxFrom(ctx); h != nil {
		return *h, true
	}
	return LogRecord{}, false
}

func logCtxFrom(ctx context.Context) *LogRecord {
	h, _ := ctx.Value(logCtxKey{}).(*LogRecord)
	return h
}

func setLogError(ctx context.Context, err error) {
	if h := logCtxFrom(ctx); h != nil {
		h.Err = err
	}
}

func setLogBackendName(ctx context.Context, name string) {
	if h := logCtxFrom(ctx); h != nil {
		h.BackendName = name
	}
}

func setLogModels(ctx context.Context, requested, upstream string) {
	if h := logCtxFrom(ctx); h != nil {
		h.ModelRequested = requested
		h.ModelUpstream = upstream
	}
}

// addLogSkipped records a backend understudy could not use and never called.
func addLogSkipped(ctx context.Context, backend, upstreamModel string, err error) {
	if h := logCtxFrom(ctx); h != nil {
		h.Excluded = append(h.Excluded, Attempt{Backend: backend, ModelUpstream: upstreamModel, Err: err})
	}
}

// addLogCalled records a target understudy called and abandoned the result of.
func addLogCalled(ctx context.Context, backend, upstreamModel string, status int, err error) {
	if h := logCtxFrom(ctx); h != nil {
		h.Excluded = append(h.Excluded, Attempt{Backend: backend, Called: true, ModelUpstream: upstreamModel, UpstreamStatus: status, Err: err})
	}
}

func setLogUpstreamStatus(ctx context.Context, status int) {
	if h := logCtxFrom(ctx); h != nil {
		h.UpstreamStatus = status
	}
}

// withStatusRecorder wraps w in a statusRecorder so errToResponse can detect a
// response a handler already committed and refrain from rendering its error
// envelope over it.
func withStatusRecorder(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&statusRecorder{ResponseWriter: w}, r)
	})
}

// authMiddleware rejects requests whose Authorization header is present but not
// parsable as "Bearer <token>" before the request reaches any handler. It
// validates the token and stashes the resulting [BackendConfig] in the request
// context for downstream handlers.
func authMiddleware(v TokenValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		var token string
		if auth != "" {
			fields := strings.Fields(auth)
			if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
				writeJSONError(r.Context(), w, yerrors.WithHTTPStatus(http.StatusUnauthorized, errors.New("invalid authorization header")), errTypeAuth)
				return
			}
			token = fields[1]
		}
		backend, err := v.Validate(r.Context(), token)
		if err != nil {
			if errors.Is(err, ErrInvalidToken) {
				writeJSONError(r.Context(), w, yerrors.WithHTTPStatus(http.StatusUnauthorized, err), errTypeAuth)
				return
			}
			writeJSONError(r.Context(), w, err, errTypeServer)
			return
		}
		ctx := context.WithValue(r.Context(), backendKey{}, backend)
		ctx = context.WithValue(ctx, tokenKey{}, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

// ErrInvalidToken is returned by a [TokenValidator] when the inbound bearer
// token is rejected. The handler maps it to 401 Unauthorized; any other
// error from Validate is treated as an internal failure and returns 500.
const ErrInvalidToken sentinelErr = "invalid token"

// errStreamIdle is returned by [idleReader.Read] when the upstream reader
// has not produced any data within the idle timeout.
const errStreamIdle sentinelErr = "upstream stream stalled"

// streamIdleTimeout bounds the gap between bytes when proxying a streamed
// chat-completions response; a stall longer than this aborts the stream. It is
// deliberately generous: a healthy upstream can go quiet for a long time before
// the first token (large-prompt prefill, a queued backend, a model still
// "thinking"), and at the byte level that silence is indistinguishable from a
// wedged stream, so we err toward not killing a slow-but-live response.
const streamIdleTimeout = 5 * time.Minute

// idleReader wraps an io.Reader and cancels ctx with errStreamIdle if the
// underlying Read call does not return within the idle window.
type idleReader struct {
	r      io.Reader
	idle   time.Duration
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func (w *idleReader) Read(p []byte) (int, error) {
	t := time.AfterFunc(w.idle, func() { w.cancel(errStreamIdle) })
	n, err := w.r.Read(p)
	t.Stop()
	return n, cause(w.ctx, err)
}

// cause augments err with ctx's cancellation cause when err is ctx's own error
// and the cause is distinct, so a read cancelled by a context carries why.
func cause(ctx context.Context, err error) error {
	if errors.Is(err, ctx.Err()) {
		if c := context.Cause(ctx); !errors.Is(err, c) {
			return fmt.Errorf("%w: %w", c, err)
		}
	}
	return err
}

// errNoBackendConfigured is returned when no backend in the resolved
// [BackendConfig] is usable — because it declares none, or because
// [server.resolveBackend] rejects every one it declares. It carries HTTP 500 so
// the error seam renders it as Internal Server Error.
var errNoBackendConfigured = yerrors.WithHTTPStatus(http.StatusInternalServerError, errors.New("no backend configured"))

// Error envelope `type` values. The first three are OpenAI-spec, written by
// [writeJSONError]; the upstream_* values are understudy-specific, written by
// [writeRetryAfterReject].
const (
	errTypeAuth                = "authentication_error"
	errTypeServer              = "server_error"
	errTypeInvalidRequest      = "invalid_request_error"
	errTypeUpstreamRateLimited = "upstream_rate_limited"
	errTypeUpstreamUnavailable = "upstream_unavailable"
)

// maxPassthroughRetryAfter is the longest Retry-After delay understudy
// forwards unchanged; a longer delay is rejected as a non-retryable 400.
// opencode honors long Retry-After values essentially unboundedly, so
// understudy fails fast rather than let them reach the client.
const maxPassthroughRetryAfter = 2 * time.Minute

// typedError wraps an error and attaches an OpenAI envelope error type string
// so that errorType can surface it via errors.AsType.
type typedError struct {
	error
	errType string
}

// ErrorType returns the OpenAI envelope type carried by this error.
func (e typedError) ErrorType() string { return e.errType }

// Unwrap returns the underlying error so that errors.Is/As and yerrors.HTTPStatus
// can still traverse the chain.
func (e typedError) Unwrap() error { return e.error }

// resolveError marks an error produced while resolving a logical model to a
// concrete target (as opposed to a malformed request body). It carries the
// resolution failure's own HTTP status through to the response.
type resolveError struct{ error }

// Unwrap returns the inner error so that yerrors.HTTPStatus, errors.As, and
// errors.Is traversal still work through the chain.
func (e resolveError) Unwrap() error { return e.error }

func badRequest(err error) error {
	return typedError{
		error:   yerrors.WithHTTPStatus(http.StatusBadRequest, err),
		errType: errTypeInvalidRequest,
	}
}

// notFound carries the "invalid_request_error" type, not a "not found" type,
// because OpenAI treats a request for a nonexistent model as an invalid request
// (404, code model_not_found). https://platform.openai.com/docs/guides/error-codes
func notFound(err error) error {
	return typedError{
		error:   yerrors.WithHTTPStatus(http.StatusNotFound, err),
		errType: errTypeInvalidRequest,
	}
}

// statusClientClosedRequest (499, a non-standard nginx code) marks a request
// the client aborted before completion. net/http has no constant for it.
const statusClientClosedRequest = 499

var sensitiveResponseHeaders = []string{"Authorization", "Set-Cookie"}

// writeJSONError writes a JSON error response. Bodies for 5xx and for 401/403
// are obfuscated to the generic HTTP status text: 5xx to avoid leaking
// internal error detail, and 401/403 to avoid revealing which auth check
// failed. The real error is always logged.
func writeJSONError(ctx context.Context, w http.ResponseWriter, err error, errType string) {
	setLogError(ctx, err)
	status := yerrors.HTTPStatus(err)
	msg := err.Error()
	if status >= 500 || status == http.StatusUnauthorized || status == http.StatusForbidden {
		msg = http.StatusText(status)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
		},
	})
}

// apiHandler is a route handler that returns its error instead of writing it.
// A nil return means the handler already wrote the response.
type apiHandler func(w http.ResponseWriter, r *http.Request) error

// recoverPanic converts a panic raised while serving into a 500-carrying error
// so [errToResponse] renders it as a server_error envelope (or, if the response
// is already committed mid-stream, logs it) rather than unwinding into net/http.
// The error carries a captured stack (via yerrors) back to the request log; the
// client sees only the generic 500 text writeJSONError substitutes for 5xx.
func recoverPanic(h apiHandler) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = yerrors.WithHTTPStatusf(http.StatusInternalServerError, "panic serving request: %v", v)
			}
		}()
		return h(w, r)
	}
}

// errorType returns the OpenAI envelope "type" for err, walking the error chain
// for a carried type via ErrorType() string; falls back to errTypeServer.
func errorType(err error) string {
	if et, ok := errors.AsType[interface {
		error
		ErrorType() string
	}](err); ok {
		return et.ErrorType()
	}
	return errTypeServer
}

// errToResponse adapts an [apiHandler] to an [http.HandlerFunc], rendering a
// returned error as the OpenAI-spec JSON error envelope. The status is
// determined by responseStatus; the envelope type is read from the error chain
// via errorType (auth failures are handled in authMiddleware before any handler runs).
func errToResponse(h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			if rec, ok := w.(*statusRecorder); ok && rec.status != 0 {
				setLogError(r.Context(), err)
				return
			}
			sig := classifyLimit(err)
			if sig.shouldReject {
				switch sig.status {
				case http.StatusTooManyRequests:
					writeRetryAfterReject(r.Context(), w, err, sig.retryAfter, errTypeUpstreamRateLimited, "upstream rate limited")
					return
				case http.StatusBadGateway:
					writeRetryAfterReject(r.Context(), w, err, sig.retryAfter, errTypeUpstreamUnavailable, "upstream unavailable")
					return
				}
			}
			// Forward the upstream backoff so the client waits before retrying.
			if sig.isRateLimit && sig.hasRetryAfter {
				w.Header().Set("Retry-After", strconv.Itoa(int(sig.retryAfter.Round(time.Second)/time.Second)))
			}
			// A rate limit the upstream left unbounded still needs a client backoff.
			if sig.isRateLimit && w.Header().Get("Retry-After") == "" {
				w.Header().Set("Retry-After", strconv.Itoa(int(synthesizedRateLimitRetryAfter/time.Second)))
			}
			writeJSONError(r.Context(), w, yerrors.WithHTTPStatus(responseStatus(err), err), errorType(err))
		}
	}
}

// writeRetryAfterReject writes a 400 reject envelope carrying errType and
// message plus a top-level retry_after_ms. remaining is rounded to the nearest
// second to absorb the few ms of processing lag between when the provider
// parsed Retry-After and when we emit the response.
func writeRetryAfterReject(ctx context.Context, w http.ResponseWriter, err error, remaining time.Duration, errType, message string) {
	setLogError(ctx, err)
	retryAfterMS := remaining.Round(time.Second).Milliseconds()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":          map[string]any{"message": message, "type": errType},
		"retry_after_ms": retryAfterMS,
	})
}

// responseStatus classifies an error into an HTTP status. A status-less
// upstream-call timeout (context.DeadlineExceeded) maps to
// 504 Gateway Timeout; a status-less client cancellation (context.Canceled) maps
// to 499 Client Closed Request; any other error keeps its own status via
// yerrors.HTTPStatus, which defaults a status-less error to 500.
func responseStatus(err error) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return statusClientClosedRequest
	default:
		return yerrors.HTTPStatus(err)
	}
}

// selection is a routed backend: its connection config and the handler that
// serves its provider type.
type selection struct {
	cfg     providers.Config
	handler providers.Handler
}

// errNoSuchBackend is the reason resolveBackend gives when the config declares no
// backend under the name asked for, as opposed to declaring one understudy cannot
// use. Callers that phrase the two differently match on it.
var errNoSuchBackend = errors.New("no such backend")

// resolveBackend reports whether the backend named name is one the proxy can route
// to, and why not when it cannot: a nil error means routable, a non-nil error is
// the reason a caller skips it. It is the one place that question is answered, for
// every selection site, so a routable verdict promises a declared backend with a
// handler and a base URL.
func (s *server) resolveBackend(backends map[string]Backend, name string) (selection, error) {
	backend, declared := backends[name]
	if !declared {
		return selection{}, errNoSuchBackend
	}
	h, ok := s.providers[backend.ProviderType]
	if !ok {
		return selection{}, fmt.Errorf("provider type %q has no registered handler", backend.ProviderType)
	}
	if backend.Config.BaseURL == nil {
		return selection{}, errors.New("must provide base_url")
	}
	return selection{cfg: backend.Config, handler: h}, nil
}

func (s *server) models(w http.ResponseWriter, r *http.Request) error {
	backend := backendFromContext(r.Context())

	var all []providers.Model
	matched := false
	for name := range backend.Backends {
		sel, err := s.resolveBackend(backend.Backends, name)
		if err != nil {
			addLogSkipped(r.Context(), name, "", err)
			continue
		}
		matched = true
		data, err := sel.handler.Models(r.Context(), sel.cfg)
		if err != nil {
			// The listing answers what understudy can serve, so a backend that cannot
			// answer contributes nothing rather than failing the request. The reason is
			// the operator's fact and reaches the log alone.
			s.logger.ErrorContext(r.Context(), "backend catalog unavailable",
				slog.String("backend", name),
				slog.Any("error", err),
			)
			continue
		}
		for i := range data {
			data[i].ID = name + "/" + data[i].ID
		}
		all = append(all, data...)
	}
	if !matched {
		return errNoBackendConfigured
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   all,
	})
	return nil
}

// rewriteModel rewrites the top-level "model" value of a JSON request body via
// the injected replace transform, streaming byte-faithfully. A body that is not
// a JSON object, an object with no top-level "model" key, or a "model" whose
// value is not a string, passes through unchanged (replace is not called),
// leaving any missing-model policy to the caller.
func rewriteModel(r io.Reader, replace func(model string) (string, error)) (io.Reader, error) {
	return jsonstream.RewriteField(r, "model", func(raw []byte) ([]byte, error) {
		if len(raw) == 0 || raw[0] != '"' {
			// Not a string value: leave it untouched, skipping replace.
			return raw, nil
		}
		var model string
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, err
		}
		newModel, err := replace(model)
		if err != nil {
			return nil, err
		}
		return json.Marshal(newModel)
	})
}

// disableThinking returns a reader over r's JSON object with exactly one
// top-level "thinking" key whose value is {"type":"disabled"}: an existing
// "thinking" value is replaced, otherwise the key is inserted. Like
// rewriteModel, only the bytes up to the splice point are buffered; the
// remainder of r streams through byte-faithfully. r must be a JSON object; a
// body that is not one passes through unchanged.
func disableThinking(r io.Reader) (io.Reader, error) {
	const (
		disabled   = `{"type":"disabled"}`
		disabledKV = `"thinking":` + disabled
	)
	var prefixBuf bytes.Buffer
	tee := io.TeeReader(r, &prefixBuf)
	dec := json.NewDecoder(tee)

	firstTok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if firstTok != json.Delim('{') {
		return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), nil
	}

	empty := true
	for {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if keyTok == json.Delim('}') {
			// The object ended without a "thinking" key: insert it right after
			// the opening '{', with a trailing comma when other keys follow.
			prefix := prefixBuf.Bytes()
			open := int64(bytes.IndexByte(prefix, '{')) + 1
			insert := []byte(disabledKV)
			if !empty {
				insert = []byte(disabledKV + ",")
			}
			spliced := slices.Concat(prefix[:open], insert, prefix[open:])
			return io.MultiReader(bytes.NewReader(spliced), r), nil
		}
		empty = false
		key, ok := keyTok.(string)
		if !ok {
			return io.MultiReader(bytes.NewReader(prefixBuf.Bytes()), r), nil
		}
		if key != "thinking" {
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return nil, err
			}
			continue
		}

		beforeValue := dec.InputOffset()
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return nil, err
		}
		afterValue := dec.InputOffset()

		// The value literal follows the ':' separator; skip the ':' and any
		// whitespace to find its start, then splice only the literal.
		prefix := prefixBuf.Bytes()
		valueStart := beforeValue + int64(bytes.IndexByte(prefix[beforeValue:], ':')) + 1
		for valueStart < afterValue && isJSONSpace(prefix[valueStart]) {
			valueStart++
		}
		spliced := slices.Concat(prefix[:valueStart], []byte(disabled), prefix[afterValue:])
		return io.MultiReader(bytes.NewReader(spliced), r), nil
	}
}

// isJSONSpace reports whether b is JSON insignificant whitespace.
func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// callWithHeaderGate runs sel.handler bounded by a header-phase gate. It returns
// the handler's response as soon as the first response header arrives; if none
// arrives within gate it cancels the attempt with errHeaderStall, waits for the
// handler to unwind, and returns errHeaderStall. Streaming of the response body
// past the header is deliberately left unbounded here — that is the mid-stream
// idle watchdog's concern, not the gate's.
func callWithHeaderGate(ctx context.Context, cancel context.CancelCauseFunc, gate time.Duration, sel selection, body io.Reader) (*http.Response, error) {
	type handlerResult struct {
		resp *http.Response
		err  error
	}
	done := make(chan handlerResult, 1)
	go func() {
		r, e := sel.handler.Chat(ctx, sel.cfg, body)
		done <- handlerResult{r, e}
	}()
	select {
	case res := <-done:
		return res.resp, res.err
	case <-time.After(gate):
		cancel(errHeaderStall)
		// Reap the cancelled handler so its upstream connection tears down before the
		// caller frees the concurrency slot; close any response it still returned, so
		// a handler that won the race does not leak its body.
		if res := <-done; res.resp != nil && res.resp.Body != nil {
			_ = res.resp.Body.Close()
		}
		return nil, errHeaderStall
	}
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) error {
	backend := backendFromContext(r.Context())

	// A panic mid-request (in the response interceptor or the streamed body) would
	// otherwise unwind past the release and leak the slot, starving the upstream.
	// One pointer suffices because the retry loop holds at most one slot at a time;
	// re-panicking leaves rendering to recoverPanic.
	var heldSlot *upstreamLimiter
	releaseHeld := func() {
		if heldSlot != nil {
			heldSlot.release()
			heldSlot = nil
		}
	}
	defer func() {
		if v := recover(); v != nil {
			releaseHeld()
			panic(v)
		}
	}()

	// Buffer the whole request body so a failover can replay it to the next
	// target: rewriteModel consumes the reader locating/rewriting the model, so
	// each attempt needs a fresh reader over the same bytes. This is the correct
	// but eager form; the streaming replacement (tee the first attempt, drain the
	// remainder on failover behind a sole-reader wrapper so r.Body is never shared
	// with the outbound transport) is the planned optimization —
	// see TODO.d/understudy-streaming-body-replay.md.
	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			return yerrors.WithHTTPStatusf(http.StatusRequestEntityTooLarge, "request body exceeds %d-byte limit", maxRequestBodyBytes)
		}
		return yerrors.WithHTTPStatusf(statusClientClosedRequest, "reading request body: %v", err)
	}

	// Shed rather than queue: waiters at a hard FD ceiling would consume the very
	// goroutines and buffered bodies the budget exists to protect, so backpressure
	// goes to the client instead. Acquired once for the whole request, not per
	// failover attempt, because a request holds at most one upstream connection.
	if !s.processLimiter.tryAcquire() {
		w.Header().Set("Retry-After", strconv.Itoa(int(processBudgetRetryAfter/time.Second)))
		return yerrors.WithHTTPStatusf(http.StatusServiceUnavailable, "process FD budget exhausted (in-flight %d)", s.processLimiter.inFlight())
	}
	defer s.processLimiter.release()

	var requestedModel, upstreamModel string
	var parsedBackendName string
	var chosen Target
	var logicalTargets []Target
	var tried []string

	for {
		requestedModel, upstreamModel, parsedBackendName = "", "", ""
		chosen, logicalTargets = Target{}, nil

		cr := &countingReader{ReadCloser: io.NopCloser(bytes.NewReader(bodyBytes))}
		body, err := rewriteModel(cr, func(model string) (string, error) {
			requestedModel = model
			// Nothing is resolved against a name the request never gave; the
			// model-is-required check below answers it once the rewrite returns.
			if model == "" {
				return model, nil
			}
			if lm, ok := backend.Models[model]; ok {
				if len(lm.Targets) == 0 {
					return "", resolveError{notFound(fmt.Errorf("logical model %q has no targets", model))}
				}
				logicalTargets = lm.Targets
				// A within-request failover has just demoted the prior target, but
				// pickTarget still tolerates it at the demotion instant (before
				// virtual time advances past the failover threshold), so pick from
				// the targets not yet tried this request.
				chosen = s.pickTarget(r.Context(), untriedTargets(lm.Targets, tried, backend.Backends), backend.Backends)
				parsedBackendName = chosen.backend
				upstreamModel = chosen.model
				return chosen.model, nil
			}
			ref, err := ParseTarget(model)
			if _, notRef := errors.AsType[notAReferenceError](err); notRef {
				if len(backend.Backends) == 0 {
					// Nothing is configured to have declared the model, so the absent
					// configuration is the more useful answer than calling the model
					// unknown.
					return "", resolveError{notFound(fmt.Errorf("no backend configured to serve model %q", model))}
				}
				return "", resolveError{notFound(fmt.Errorf("unknown logical model %q", requestedModel))}
			}
			if err != nil {
				return "", resolveError{badRequest(err)}
			}
			if err := ref.validate(); err != nil {
				return "", resolveError{badRequest(fmt.Errorf("model %q: %w", model, err))}
			}
			chosen = ref
			parsedBackendName = ref.backend
			upstreamModel = ref.model
			return ref.model, nil
		})
		if err != nil {
			if re, ok := errors.AsType[resolveError](err); ok {
				return re.error
			}
			return badRequest(fmt.Errorf("malformed request body: %w", err))
		}
		if requestedModel == "" {
			return badRequest(errors.New("model is required"))
		}

		// A configured-but-unusable backend is not an unknown one; the caller gets
		// the real reason rather than a falsehood about what was declared.
		sel, err := s.resolveBackend(backend.Backends, parsedBackendName)
		if errors.Is(err, errNoSuchBackend) {
			return notFound(fmt.Errorf("model references unknown backend %q", parsedBackendName))
		}
		if err != nil {
			return notFound(fmt.Errorf("model references unusable backend %q: %w", parsedBackendName, err))
		}
		name := parsedBackendName
		setLogBackendName(r.Context(), name)

		if requestedModel != "" {
			setLogModels(r.Context(), requestedModel, upstreamModel)
		}

		// Snapshot the byte count before forwarding: the downstream handler reads
		// further through cr, so any check after sel.handler would include the
		// forwarded body bytes and always exceed the threshold.
		scanned := cr.n
		if scanned > maxPrefixScan {
			s.logger.ErrorContext(r.Context(), "model field beyond prefix scan threshold",
				slog.Int64("bytes", scanned),
				slog.Int64("threshold", maxPrefixScan),
			)
		}

		ctx, cancel := context.WithCancelCause(r.Context())

		if chosen.disablesThinking() {
			body, err = disableThinking(body)
			if err != nil {
				cancel(nil)
				return badRequest(fmt.Errorf("malformed request body: %w", err))
			}
		}

		// Hold an upstream slot for the whole request — through the response-body
		// stream, not just the handler call — so an (N+1)th request blocks here
		// before reaching the upstream once N are in flight.
		lim := s.upstreamLimiter(canonicalUpstreamKey(sel.cfg.BaseURL, sel.cfg.APIKey))
		waited, err := lim.acquire(ctx)
		if err != nil {
			cancel(nil)
			return fmt.Errorf("waiting for an upstream slot (in-flight %d): %w", lim.inFlight(), err)
		}
		heldSlot = lim

		resp, err := callWithHeaderGate(ctx, cancel, s.headerStallGate, sel, body)
		if errors.Is(err, errHeaderStall) {
			// A pre-header stall: backpressure from a busy target. Demote it under a
			// synthesized backoff, then replay the request onto the next untried
			// target rather than surfacing the stall; only when none remains does the
			// client see the 504.
			s.recordRateLimited(chosen, synthesizedStallBackoff, backend.Backends)
			releaseHeld()
			if logicalTargets != nil {
				tried = append(tried, healthKey(chosen, backend.Backends))
				if len(untriedTargets(logicalTargets, tried, backend.Backends)) > 0 {
					addLogCalled(r.Context(), parsedBackendName, upstreamModel, 0, err)
					continue
				}
			}
			return yerrors.WithHTTPStatus(http.StatusGatewayTimeout, errHeaderStall)
		}
		sig := classifyLimit(err)
		// The limiter is keyed per upstream account, independent of any logical-model
		// target, so shrink on a rate-limit 429 even for a request that has no chosen
		// target to demote.
		switch {
		case err == nil:
			// Demand-gated: a success that never waited for a slot is no evidence
			// about the account's capacity, so it does not raise the cap.
			if waited {
				lim.grow()
			}
		case sig.condition == signalless:
			lim.throttle()
		}
		// A signal-less 429 is ambiguous — a concurrency limit or an exhausted quota —
		// and we see only this process's in-flight, not the account's global load.
		// With others in flight here, concurrency is plausible: shrink (a cheap,
		// reversible throttle) and stay in rotation rather than fail over. Arriving
		// locally alone makes concurrency less likely — not ruled out, since other
		// lindy instances share the account's limit (until a shared understudy sees
		// the aggregate) — so lean toward demoting.
		demote := sig.condition == sustainedRate ||
			(sig.condition == signalless && lim.inFlight() <= 1)
		if chosen.backend != "" {
			switch {
			case err == nil:
				s.clearFailure(chosen, backend.Backends)
			case demote && sig.hasRetryAfter:
				s.recordRateLimited(chosen, sig.retryAfter, backend.Backends)
			case demote || isAccessRefused(err):
				s.recordImmediateFailure(chosen, backend.Backends)
			// A recurring transient 429 accrues the streak so a brief-throttle storm eventually redirects; the Retry-After is honored for the client wait in the response path.
			case sig.condition == transientRate || isFatalUpstream(err):
				s.recordFailure(chosen, backend.Backends)
			}
		}
		if err != nil {
			// A sustainedRate 429 or refused access has just demoted chosen
			// above; if another target has not yet been tried this request, replay it
			// there rather than surface the refusal to the client.
			if logicalTargets != nil && (sig.condition == sustainedRate || isAccessRefused(err)) {
				tried = append(tried, healthKey(chosen, backend.Backends))
				if len(untriedTargets(logicalTargets, tried, backend.Backends)) > 0 {
					addLogCalled(r.Context(), parsedBackendName, upstreamModel, yerrors.HTTPStatus(err), err)
					releaseHeld()
					cancel(nil)
					continue
				}
			}
			setLogUpstreamStatus(r.Context(), yerrors.HTTPStatus(err))
			releaseHeld()
			cancel(nil)
			remaining := untriedTargets(logicalTargets, append(slices.Clone(tried), healthKey(chosen, backend.Backends)), backend.Backends)
			return s.terminalFailure(chosen, remaining, backend.Backends, clientFacing(ctx, err))
		}
		setLogUpstreamStatus(r.Context(), resp.StatusCode)

		for _, h := range sensitiveResponseHeaders {
			resp.Header.Del(h)
		}

		if s.interceptor != nil {
			if err := s.interceptor(r.Context(), RequestMetadata{Backend: parsedBackendName, Model: upstreamModel, Token: tokenFromContext(r.Context())}, resp); err != nil {
				_ = resp.Body.Close()
				releaseHeld()
				cancel(nil)
				return err
			}
			resp.Header.Del("Content-Length")
		}

		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, err = io.Copy(w, &idleReader{r: resp.Body, idle: streamIdleTimeout, ctx: ctx, cancel: cancel})
		_ = resp.Body.Close()
		releaseHeld()
		cancel(nil)
		return err
	}
}
