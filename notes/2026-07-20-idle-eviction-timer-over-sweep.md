# Idle eviction: per-tenant timer chosen over the sweep

Point-in-time decision (2026-07-20), reversing the sweep direction taken in
[2026-07-13](2026-07-13-registry-idle-eviction-timer-race.md).

## The reversal

The 2026-07-13 note rejected per-token timers on a reset-after-fire race and built a
`lastUsed` timestamp swept by a daemon-lifecycle goroutine instead. That reasoning was
re-examined and the timer was adopted after all: each tenant holds a `time.AfterFunc`,
armed at Register, `Reset` on every Validate, `Stop` on Teardown — all under the
registry mutex.

## Why the race objection didn't hold

The rejected race: `Reset` cannot recall a callback that has already fired, so a request
landing in the tiny gap after the deadline can evict a tenant it just used. Traced to its
actual outcome, the only observable effect is a **correct 401** on that tenant's *next*
request — the token genuinely no longer exists, so reporting it unregistered is truthful.
No crash, no corruption: `Reset` on a fired timer just reschedules, and a second
`Teardown` is an idempotent map-delete.

Two facts make it a non-issue in practice:

- The race only fires when a run's inter-request gap is ≈ the full idle window. The window
  is set *above* opencode's ~10-min idle watchdog, so a live run never idles that long —
  it would be torn down by the watchdog first.
- The same "evict → next request 401s" outcome exists for the sweep too (a request
  arriving as the sweeping decision holds the lock). Neither run re-registers on 401, so
  the failure mode is shared; the timer isn't uniquely exposed.

## Why the timer won

- **Eviction is intrinsic to the Registry.** `NewRegistry` → every `Register` arms a
  timer → the registry reclaims idle tenants on its own. The sweep made eviction
  *extrinsic*: a `Registry` only got swept if a caller remembered to start the
  daemon-lifecycle goroutine, so a registry constructed anywhere else silently never
  reclaimed. Keeping the whole tenant lifecycle inside one type is the SSOT win.
- **Cost is negligible.** `AfterFunc` registers a timer-heap entry, not a parked
  goroutine; the callback goroutine is transient, spawned only at firing. The steady-state
  delta versus one sweeper goroutine is ≈ zero.

The sweep's one genuine edge — a single decision site under the lock, trivially reasoned
about at the boundary — was judged not worth making eviction extrinsic to the Registry.
