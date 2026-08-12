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

// deferredLog queues a log call to be emitted once the caller's lock is released.
type deferredLog func(msg string, args ...any)

// targetHealth tracks a target's failure streak. An absent entry is a healthy target:
// only a failure creates one, and clearing it is what recovery means.
type targetHealth struct {
	// failingSince is the moment the streak is measured from: when it began, or a
	// failover threshold before a target demoted at once, so the walk routes around
	// that one on the very next request and the terminal ladder ages it from there.
	failingSince time.Time
	// streakBegan is when the target actually first failed, which is what a record
	// reports and no backdate touches.
	streakBegan time.Time
	// lastProbe is when the target was last attempted.
	lastProbe time.Time
	// readmitAt is a known re-admission time, from an advertised Retry-After or a
	// synthesized stall bench, or zero when only probe pacing holds the target back.
	readmitAt time.Time
	// downLogged is whether this streak's "backend down" has been reported, so the
	// walk and every demotion after the first stay silent.
	downLogged bool
	// lastError is what the target answered the last time it failed, so a later reader
	// can name the cause and its status without hunting the request that saw it.
	lastError error
	// lastTouch is when the entry was last written, the age the eviction sweep
	// measures.
	lastTouch time.Time
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

// neverRetryableError lets the response path recognize a failure no delay can
// clear, after clientFacing has flattened the upstream status to 502 and taken the
// evidence with it.
type neverRetryableError struct{ error }

func (e neverRetryableError) Unwrap() error { return e.error }

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

// pick is what choosing a target taught the walk. Its skipped list is reported
// rather than recorded, so the walk can order its own account: it knows what it
// abandoned before this pick, and pickTarget does not.
type pick struct {
	target  Target
	ok      bool
	skipped []Attempt // candidates understudy could not call
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
// re-admission time (readmitAt, from an advertised Retry-After or a synthesized
// stall bench) is instead
// routed around until that time, then re-admitted as a half-open probe (its
// health preserved until the probe's outcome) — never half-open-probed early while
// any other candidate remains. If every target is past the threshold and not due
// for a probe, it returns the last one it could call so a request always has
// somewhere to go.
//
// TODO(TODO.d/honor-an-advertised-backoff-with-nothing-left.md): that fallback
// returns a target benched until a moment that has not arrived, against the rule
// above.
//
// It reports false when every candidate named a backend understudy cannot use, and
// returns each rejection for the caller to record — only the walk knows the order it
// reached them in. A walk that has already failed somewhere answers with that
// failure; one that never attempted anything is the model with nothing to serve it,
// which only the caller can name.
func (s *server) pickTarget(ctx context.Context, targets []Target, backends map[string]Backend) pick {
	// Queued under the lock, emitted after it: the logger is the consumer's, and a
	// handler that blocks on I/O would otherwise hold every other request's walk
	// behind it. Registered before the unlock below, so LIFO runs it after.
	var queued []func()
	defer func() {
		for _, emit := range queued {
			emit()
		}
	}()
	logLater := func(msg string, args ...any) {
		queued = append(queued, func() { s.logTransition(ctx, msg, args...) })
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.evictStaleHealth()
	var skipped []Attempt
	for _, t := range targets {
		if _, err := s.resolveBackend(backends, t.backend); err != nil {
			skipped = append(skipped, Attempt{Backend: t.backend, ModelUpstream: t.model, Err: err})
			continue
		}
		id := healthKey(t, backends)
		h, failing := s.health[id]
		if !failing || now.Sub(h.failingSince) <= s.failoverThreshold {
			return pick{target: t, ok: true, skipped: skipped}
		}
		if due := s.nextReattempt(h); now.Before(due) {
			notDue := fmt.Errorf("routed around: not due until %s, last answered: %w",
				due.Format(time.RFC3339), h.lastError)
			skipped = append(skipped, Attempt{Backend: t.backend, ModelUpstream: t.model, Err: notDue})
			s.noteBackendDown(logLater, id, t, h)
			continue
		}
		h.readmitAt = time.Time{}
		h.lastProbe = now
		h.lastTouch = now
		s.health[id] = h
		return pick{target: t, ok: true, skipped: skipped}
	}
	// Nothing was healthy, but a target health kept back is still worth attempting.
	last, ok := s.lastCallableTarget(targets, backends)
	return pick{target: last, ok: ok, skipped: skipped}
}

// lastCallableTarget returns the last target understudy could call at all, and
// whether the list held one. A backend understudy cannot call subtracts itself
// rather than deciding for the list, so what remains is what the request has.
func (s *server) lastCallableTarget(targets []Target, backends map[string]Backend) (Target, bool) {
	for _, t := range slices.Backward(targets) {
		if _, err := s.resolveBackend(backends, t.backend); err == nil {
			return t, true
		}
	}
	return Target{}, false
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

// msgBackendDown announces a target leaving rotation. Its pair is "backend up".
const msgBackendDown = "backend down"

// Why a target is out, in the words an operator reads. Each names something visible
// from outside understudy: what the upstream sent, what it failed to send, or that
// understudy is pacing its own retries.
const (
	reasonUpstreamRetryAfter = "upstream retry-after"
	reasonNoResponseHeader   = "no response header"
	reasonProbeNotYetDue     = "probe not yet due"
)

// downCause is why a target is out paired with when it is due back. The two travel
// together so a record cannot name an upstream's terms while reporting the schedule
// understudy set for itself, or the reverse.
type downCause struct {
	reason   string
	schedule slog.Attr
}

// benchedUntil is a target held to a re-admission moment something recorded, named by
// whatever recorded it.
func benchedUntil(reason string, at time.Time) downCause {
	return downCause{reason: reason, schedule: slog.Time("readmit_at", at)}
}

// pacedTo is a target no one benched, held back only until understudy's own next
// probe is due.
func pacedTo(at time.Time) downCause {
	return downCause{reason: reasonProbeNotYetDue, schedule: slog.Time("next_probe", at)}
}

// backendDownRecord is what every "backend down" says: which target, why it is out,
// when it started failing, and when it is due back — the moment an upstream named, or
// the one understudy's own pacing sets, never both. Built here so the walk and the
// demotion paths cannot drift apart. Caller holds s.mu only if h came from the map.
func backendDownRecord(t Target, h targetHealth, cause downCause) []any {
	return []any{
		slog.String("backend", t.backend),
		slog.String("model", t.model),
		slog.String("reason", cause.reason),
		// The record's own timestamp is not this: a demotion may be reported by a
		// walk that routes around t some time after it started failing.
		h.failedSince(),
		slog.String("upstream_error", errText(h.lastError)),
		cause.schedule,
	}
}

// demote writes t's demotion and reports the health written, along with whether this
// call owes a "backend down" — the first of the streak. bench is how long t is held
// out, or nil when nothing holds it but understudy's own probe pacing; a nil bench
// leaves an existing re-admission moment alone rather than clearing it, since an
// upstream that named one has not withdrawn it by failing another way. Unlike
// writeHealth, an existing entry is always visited, because a streak that was never
// reported still owes a record however it is demoted again.
func (s *server) demote(t Target, backends map[string]Backend, bench *time.Duration, answered error) (targetHealth, bool) {
	var owed bool
	var written targetHealth
	s.writeHealth(t, backends,
		func(now time.Time) targetHealth {
			var readmitAt time.Time
			if bench != nil {
				readmitAt = now.Add(*bench)
			}
			h := s.demotedHealth(now, readmitAt)
			owed, h.downLogged = true, true
			h.lastError = answered
			written = h
			return h
		},
		func(now time.Time, h targetHealth) targetHealth {
			if bench != nil {
				h.readmitAt = now.Add(*bench)
			}
			owed = !h.downLogged
			h.downLogged = true
			h.lastError = answered
			written = h
			return h
		})
	return written, owed
}

// demoteFor is demote with a re-admission moment: t is held out for d, rather than
// until understudy's own probe pacing lets it back.
func (s *server) demoteFor(t Target, d time.Duration, backends map[string]Backend, answered error) (targetHealth, bool) {
	return s.demote(t, backends, &d, answered)
}

// errText renders an error for a record, or "" when there is none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// failedSince is the attr naming when a target started failing, built here so every
// "backend down" record maps the same name to the same field: failingSince is what
// the streak is measured from, backdated for a target demoted at once, and never
// what an operator is told.
func (h targetHealth) failedSince() slog.Attr {
	return slog.Time("failing_since", h.streakBegan)
}

// nextReattempt is when t is due to be called again: the re-admission moment if one
// was recorded, and otherwise a full recovery interval past its last attempt. The
// walk routes around a failing target until this moment and the "backend down"
// record reports it, so both read the moment from here. Which schedule set it is
// still read from readmitAt at each site. Caller holds s.mu.
func (s *server) nextReattempt(h targetHealth) time.Time {
	if h.readmitAt.IsZero() {
		return h.lastProbe.Add(s.recoveryInterval)
	}
	return h.readmitAt
}

// noteBackendDown logs t's "backend down" transition once per failure streak, saying
// why the walk routed around it. It logs through logLater rather than directly,
// because the caller holds s.mu.
func (s *server) noteBackendDown(logLater deferredLog, id string, t Target, h targetHealth) {
	if h.downLogged {
		return
	}
	h.downLogged = true
	h.lastTouch = time.Now()
	s.health[id] = h
	// Always paced: every path that benches a target reports its own demotion and
	// marks it, so a streak still owing a record is one this walk accrued.
	logLater(msgBackendDown, backendDownRecord(t, h, pacedTo(s.nextReattempt(h)))...)
}

// evictStaleHealth drops every entry untouched for healthTTL and returns the
// sweep time, so a caller's own write agrees with the eviction. Caller holds s.mu.
func (s *server) evictStaleHealth() time.Time {
	now := time.Now()
	for id, h := range s.health {
		if now.Sub(h.lastTouch) >= healthTTL {
			delete(s.health, id)
		}
	}
	return now
}

// writeHealth stores t's health under s.mu. It sweeps first — the only thing that
// reclaims an entry named directly and so never walked past — then stamps the
// entry with the sweep's own clock, so a write cannot be aged out by the sweep
// that preceded it. beginStreak builds the entry for a key the map does not hold —
// a target with no entry is not failing, so a new one opens a streak; updateStreak,
// when non-nil, revises a streak already under way.
func (s *server) writeHealth(t Target, backends map[string]Backend, beginStreak func(now time.Time) targetHealth, updateStreak func(now time.Time, h targetHealth) targetHealth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.evictStaleHealth()
	id := healthKey(t, backends)
	h, ok := s.health[id]
	switch {
	case !ok:
		h = beginStreak(now)
	case updateStreak != nil:
		h = updateStreak(now, h)
	}
	h.lastTouch = now
	s.health[id] = h
}

// recordFailure marks t as failing, preserving the start of an existing streak
// so the threshold measures from the first failure, not the most recent, while
// lastError follows the most recent — the streak's age and its cause answer
// different questions. A new
// streak seeds lastProbe at the demotion moment (failingSince plus the failover
// threshold), so the first half-open probe waits a full recovery interval after
// the target is actually routed around, not from its first failure.
func (s *server) recordFailure(t Target, backends map[string]Backend, answered error) {
	s.writeHealth(t, backends,
		func(now time.Time) targetHealth {
			return targetHealth{failingSince: now, streakBegan: now, lastProbe: now.Add(s.failoverThreshold), lastError: answered}
		},
		func(_ time.Time, h targetHealth) targetHealth {
			h.lastError = answered
			return h
		})
}

// demotedHealth builds the health of a target demoted at once: the streak is
// backdated past the failover threshold so pickTarget routes around it on the
// very next request, and lastProbe is seeded at the demotion moment so the first
// half-open re-probe still waits a full recovery interval. readmitAt is the known
// re-admission time — an advertised Retry-After, or the bench understudy synthesizes
// for an upstream that answered nothing — or zero for an unbounded demotion.
func (s *server) demotedHealth(now, readmitAt time.Time) targetHealth {
	return targetHealth{failingSince: now.Add(-s.failoverThreshold), streakBegan: now, lastProbe: now, readmitAt: readmitAt, lastTouch: now}
}

// recordImmediateFailure demotes t at once, so pickTarget routes around it on
// the very next request rather than tolerating it for a failover threshold.
func (s *server) recordImmediateFailure(ctx context.Context, t Target, backends map[string]Backend, answered error) {
	h, owed := s.demote(t, backends, nil, answered)
	if owed {
		s.logTransition(ctx, msgBackendDown, backendDownRecord(t, h, pacedTo(s.nextReattempt(h)))...)
	}
}

// recordStalled demotes t for a pre-header stall and benches it for a backoff
// understudy synthesized, the upstream having named none.
// It logs the transition itself: a stall is a demotion understudy decides, so the
// cause is known here and nowhere later.
func (s *server) recordStalled(ctx context.Context, t Target, backends map[string]Backend, answered error) {
	if h, owed := s.demoteFor(t, synthesizedStallBackoff, backends, answered); owed {
		s.logTransition(ctx, msgBackendDown, backendDownRecord(t, h, benchedUntil(reasonNoResponseHeader, h.readmitAt))...)
	}
}

// recordRateLimited demotes t at once like recordImmediateFailure, and records the
// moment the upstream named. An upstream that answers `Retry-After` has said more
// about when it will serve again than understudy's own pacing can infer, so that
// moment supersedes the recovery interval — which would otherwise call the target
// back while it is still saying no. A bench understudy synthesized for an upstream
// that said nothing is recordStalled's, not this one's. It logs the transition when
// the streak has not already reported one.
func (s *server) recordRateLimited(ctx context.Context, t Target, retryAfter time.Duration, backends map[string]Backend, answered error) {
	if h, owed := s.demoteFor(t, retryAfter, backends, answered); owed {
		s.logTransition(ctx, msgBackendDown, backendDownRecord(t, h, benchedUntil(reasonUpstreamRetryAfter, h.readmitAt))...)
	}
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

// failedAttempt is an attempt that failed: what the request would answer with if
// nothing better comes, and what to record if something does.
type failedAttempt struct {
	// answer is client-facing, classified in the iteration that failed: what
	// cancelled an attempt is only knowable while that attempt's context is alive.
	answer error
	// raw is what the verdict reads, since clientFacing rewrites a 501 past
	// recognizing.
	raw           error
	target        Target
	backend       string
	upstreamModel string
}

// status is what the attempt answered with. An attempt cut off before its header
// answered nothing, so it reports none: Attempt.UpstreamStatus is 0 for exactly that,
// and yerrors would otherwise invent a 500 for a status-less error.
func (f failedAttempt) status() int {
	if errors.Is(f.raw, errHeaderStall) {
		return 0
	}
	return yerrors.HTTPStatus(f.raw)
}

// record puts f on the request's Excluded: a target the request did not serve from.
func (f failedAttempt) record(ctx context.Context) {
	addLogCalled(ctx, f.backend, f.upstreamModel, f.status(), f.raw)
}

// terminalFailure marks err terminal — the response path rejects it as
// non-retryable rather than relaying it — when remaining holds nothing understudy
// could call, whether because it is empty or because every candidate in it names a
// backend understudy cannot use, and t has been failing past the terminal
// threshold. Both are required: a long failure on a target the list can still route
// around is a demoted target, not an exhausted list.
func (s *server) terminalFailure(t Target, remaining []Target, backends map[string]Backend, err error) error {
	if t.backend == "" || s.failingFor(t, backends) < s.terminalThreshold {
		return err
	}
	// A candidate understudy cannot call is not somewhere left to go, so it cannot
	// hold the ladder's last rung open — the same reading the walk uses to decide
	// whether to fail over at all.
	if _, ok := s.lastCallableTarget(remaining, backends); ok {
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
// "backend up" transition iff the streak was previously logged "backend
// down", keeping the up/down log pair symmetric.
func (s *server) clearFailure(ctx context.Context, t Target, backends map[string]Backend) {
	// Logged after dropHealth returns, so the consumer's handler does not block
	// every other request's walk behind s.mu.
	if s.dropHealth(t, backends) {
		s.logTransition(ctx, "backend up",
			slog.String("backend", t.backend),
			slog.String("model", t.model),
		)
	}
}

// logTransition emits a target's health transition. The transition is decided and
// committed under s.mu before this runs, so the record cannot depend on the request
// that noticed it still being around: a client that leaves in that window would
// otherwise take a committed state change out of the log with it. A consumer's
// values travel; only cancellation is dropped.
func (s *server) logTransition(ctx context.Context, msg string, args ...any) {
	s.logger.InfoContext(context.WithoutCancel(ctx), msg, args...)
}

// dropHealth forgets t's health and reports whether its failure had been logged, so
// the "backend up" that pairs with it is logged outside the lock.
func (s *server) dropHealth(t Target, backends map[string]Backend) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := healthKey(t, backends)
	h, ok := s.health[id]
	delete(s.health, id)
	return ok && h.downLogged
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
	if status := yerrors.HTTPStatus(err); status >= 500 {
		if status == http.StatusNotImplemented {
			err = neverRetryableError{err}
		}
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
	// hasRetryAfter reports that the upstream advertised a Retry-After still
	// outstanding; one that has elapsed counts as no advertisement.
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
		// An elapsed advertisement leaves nothing to relay, so the failure falls to
		// the synthesized path rather than handing every reader a negative delay.
		if remaining := time.Until(ra.RetryAfter()); remaining > 0 {
			sig.hasRetryAfter = true
			sig.retryAfter = remaining
		}
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
	// A failure no retry can help offers no delay, in either form one takes: not a
	// relayed header, not a reject's retry_after_ms. Clearing it here rather than at
	// each exit keeps the two from drifting apart.
	if _, never := errors.AsType[neverRetryableError](err); never {
		sig.hasRetryAfter = false
		sig.shouldReject = false
		sig.retryAfter = 0
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
func addLogSkipped(ctx context.Context, a Attempt) {
	if h := logCtxFrom(ctx); h != nil {
		h.Excluded = append(h.Excluded, a)
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
// [writeJSONError]; the upstream_* values are understudy's own, written by the
// rejects that end a request rather than relay it.
const (
	errTypeAuth                = "authentication_error"
	errTypeServer              = "server_error"
	errTypeInvalidRequest      = "invalid_request_error"
	errTypeUpstreamRateLimited = "upstream_rate_limited"
	errTypeUpstreamUnavailable = "upstream_unavailable"
	errTypeUpstreamRefused     = "upstream_refused"
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
	status := yerrors.HTTPStatus(err)
	msg := err.Error()
	if status >= 500 || status == http.StatusUnauthorized || status == http.StatusForbidden {
		msg = http.StatusText(status)
	}
	writeErrorEnvelope(ctx, w, err, status, msg, errType, 0)
}

// errorBody is the shape every failure answer takes on the wire, so the format has
// one declaration rather than a literal at each writer. retryAfter is understudy's
// own decision — sometimes what an upstream advertised, sometimes a value it
// synthesized — so it arrives as a duration and is rendered in milliseconds here,
// where the wire encoding belongs.
type errorBody struct {
	Error        apiError `json:"error"`
	RetryAfterMS int64    `json:"retry_after_ms,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// writeErrorEnvelope writes the error body every failure answer shares. The real
// error reaches the log whatever the client is shown. A zero retryAfter carries no
// delay at all; a rounded one is how far off the client is told to come back.
func writeErrorEnvelope(ctx context.Context, w http.ResponseWriter, err error, status int, message, errType string, retryAfter time.Duration) {
	setLogError(ctx, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// TODO(TODO.d/record-a-client-that-left-before-the-answer.md): a client that
	// disconnects between the header and the body leaves no trace here, where the
	// streaming path logs the same event.
	_ = json.NewEncoder(w).Encode(errorBody{
		Error:        apiError{Message: message, Type: errType},
		RetryAfterMS: retryAfter.Round(time.Second).Milliseconds(),
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
			if isAccessRefused(err) {
				writeRefusal(r.Context(), w, err)
				return
			}
			// Any retryable failure carries its advertised backoff, not just a rate
			// limit — a 503 that named its own return is worth relaying. A request the
			// upstream faulted on (400, 404) is not retryable, so a delay it advertised
			// means nothing.
			if sig.hasRetryAfter && (sig.isRateLimit || isFatalUpstream(err)) {
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

// writeRefusal writes the 400 reject for a request no configured target will
// serve. It does not route through [writeJSONError]: that obfuscates on status
// (>=500, 401, 403), so a 400 escapes it and would carry the upstream's own words.
// No delay rides with it either, because no delay clears a refusal. What a client
// may be told is §Understudy's to say, not this function's.
func writeRefusal(ctx context.Context, w http.ResponseWriter, err error) {
	writeErrorEnvelope(ctx, w, err, http.StatusBadRequest,
		"no configured target could serve this request", errTypeUpstreamRefused, 0)
}

// writeRetryAfterReject writes a 400 reject envelope carrying errType and
// message plus a top-level retry_after_ms. remaining is rounded to the nearest
// second to absorb the few ms of processing lag between when the provider
// parsed Retry-After and when we emit the response.
func writeRetryAfterReject(ctx context.Context, w http.ResponseWriter, err error, remaining time.Duration, errType, message string) {
	writeErrorEnvelope(ctx, w, err, http.StatusBadRequest, message, errType, remaining)
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

// noTargetsError is what a client is told about a model nothing can serve, whether it
// declared no targets or every target it declared named a backend understudy cannot
// call. The two are one condition to a caller, so they are one sentence.
func noTargetsError(model string) error {
	return notFound(fmt.Errorf("logical model %q has no targets", model))
}

// errWalkExhausted reports, from the pick, that the walk has nowhere left to go. It
// never reaches a client: the loop recognizes it and answers with the failure that
// got there.
var errWalkExhausted = errors.New("walk exhausted")

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
			addLogSkipped(r.Context(), Attempt{Backend: name, Err: err})
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
	// throttled is the candidate the walk failed over from under a timed backoff; the
	// answer is judged against whichever target it came from.
	// TODO(TODO.d/weigh-every-candidates-contribution.md)
	var throttled *failedAttempt
	var lastFailure *failedAttempt
	var remaining []Target

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
					return "", resolveError{noTargetsError(model)}
				}
				logicalTargets = lm.Targets
				// A within-request failover has just demoted the prior target, but
				// pickTarget still tolerates it at the demotion instant (before
				// virtual time advances past the failover threshold), so pick from
				// the targets not yet tried this request.
				p := s.pickTarget(r.Context(), untriedTargets(lm.Targets, tried, backend.Backends), backend.Backends)
				if p.ok && lastFailure != nil {
					// Somewhere else to go, so the request did move past the last
					// candidate — recorded before this pick's skips, which is the
					// order the walk saw them in.
					lastFailure.record(r.Context())
				}
				for _, a := range p.skipped {
					addLogSkipped(r.Context(), a)
				}
				if !p.ok {
					// A walk that got here through a failure answers for that failure,
					// which only the loop can do; one that never attempted anything is
					// the model with nothing to serve it.
					if lastFailure != nil {
						return "", errWalkExhausted
					}
					return "", resolveError{noTargetsError(model)}
				}
				chosen = p.target
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
			if errors.Is(err, errWalkExhausted) {
				break
			}
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
			s.recordStalled(r.Context(), chosen, backend.Backends, err)
			releaseHeld()
			stalled := yerrors.WithHTTPStatus(http.StatusGatewayTimeout, errHeaderStall)
			if logicalTargets != nil {
				tried = append(tried, healthKey(chosen, backend.Backends))
				lastFailure = &failedAttempt{
					answer:        stalled,
					raw:           err,
					target:        chosen,
					backend:       parsedBackendName,
					upstreamModel: upstreamModel,
				}
				continue
			}
			return stalled
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
				s.clearFailure(r.Context(), chosen, backend.Backends)
			case demote && sig.hasRetryAfter:
				s.recordRateLimited(r.Context(), chosen, sig.retryAfter, backend.Backends, err)
			case demote || isAccessRefused(err):
				s.recordImmediateFailure(r.Context(), chosen, backend.Backends, err)
			// A recurring transient 429 accrues the streak so a brief-throttle storm eventually redirects; the Retry-After is honored for the client wait in the response path.
			case sig.condition == transientRate || isFatalUpstream(err):
				s.recordFailure(chosen, backend.Backends, err)
			}
		}
		if err != nil {
			failed := failedAttempt{
				answer:        clientFacing(ctx, err),
				raw:           err,
				target:        chosen,
				backend:       parsedBackendName,
				upstreamModel: upstreamModel,
			}
			// A sustainedRate 429 or refused access has just demoted chosen
			// above; if another target has not yet been tried this request, replay it
			// there rather than surface the refusal to the client.
			if logicalTargets != nil && (sig.condition == sustainedRate || isAccessRefused(err)) {
				// The verdict is the soonest return on offer, so a later candidate
				// displaces the incumbent when it comes back first. The incumbent's
				// delay is re-derived rather than remembered, so both are what remains
				// as of now.
				if sig.condition == sustainedRate && sig.hasRetryAfter &&
					(throttled == nil || sig.retryAfter < classifyLimit(throttled.raw).retryAfter) {
					throttled = &failed
				}
				tried = append(tried, healthKey(chosen, backend.Backends))
				lastFailure = &failed
				releaseHeld()
				cancel(nil)
				continue
			}
			releaseHeld()
			cancel(nil)
			lastFailure, remaining = &failed, untriedTargets(logicalTargets, append(slices.Clone(tried), healthKey(chosen, backend.Backends)), backend.Backends)
			break
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

	// The walk is over, by either road: it ran out of candidates, or its last failure
	// was one no other candidate could answer for. What it answers with is that
	// failure, unless an earlier candidate offered a return it can still make.
	f := *lastFailure
	setLogUpstreamStatus(r.Context(), f.status())
	answer, answering := f.answer, f.target
	if throttled != nil &&
		(isAccessRefused(f.raw) || yerrors.HTTPStatus(f.raw) == http.StatusNotImplemented) {
		// An earlier candidate answers, so this one is a target the request did not
		// serve from — and the only record of why.
		f.record(r.Context())
		answer, answering = throttled.answer, throttled.target
	}
	return s.terminalFailure(answering, remaining, backend.Backends, answer)
}
