# Tell a removed model from a recovering one

**Tag:** understudy / availability / ha

**Design:** [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) — the
probe is a synthetic chat completion because it "must exercise the chat path,
the only one that tests the same credential, balance, and quota bucket a real
request does"; `/v1/models` was rejected *as the probe*. That rejection does not
forbid the catalog as a **classifier** beside a failing probe: the chat
completion still does the health test, and the catalog only answers what kind
of failure it found.
[DESIGN.md §Understudy](../DESIGN.md#understudy) — the endings table gives "a
model with nothing usable the same words as one declaring no targets"; today
nothing produces that ending, because nothing can name a removed model as such.

## Work

- Observed 2026-10-21: zen go dropped `deepseek-v4-flash` from its catalog
  between one evening and the next morning. Because the target was healthy,
  nothing was demoted, so no probe ran — the first client request paid for the
  discovery, and with [[fail-over-on-an-envelope-less-400]] landed, the walk
  will route around it from then on. What the walk and the probe cannot yet
  say is that the condition is *permanent*: a removed model escalates through
  the recovery schedule forever (free, but pointless — ≈100 probes a day at
  the cap, each answering only "still failing"), and the operator is never
  told this is not an outage that time heals.
- When a probe (or a walk exclusion) fails a target, and the failure is a
  `400` — the class [[fail-over-on-an-envelope-less-400]] classifies as
  target-fault — consult the upstream's `GET /models` catalog once: is the
  model still listed?
  - Listed → the failure is something else; keep the recovery schedule.
  - Absent → a standing fact, like a refusal: stop escalating, mark the
    (url + key + model) health terminal for the model, and give the walk's
    answer the "model with nothing usable" ending instead of a bare relayed
    `400`. Re-check only when the catalog itself shows the model return (a
    cheap periodic list, demand-triggered as [[demand-triggered-recovery-probe]]
    already is — not a new standing timer).
- The catalog check needs the same credential but shares neither the billing
  check nor the quota — which is exactly why it classifies rather than
  probes: existence is the one question it answers honestly.
- The residual gap — drift on a *healthy* target costs its first client
  request — is the cost §Recovery probing already accepted when it rejected
  activity-gated polling; this item does not reopen that. A single-member pin
  (lindy's `review-deepseek-thinking` targets only zen go's deepseek) still
  fails its beat; the gain there is an honest immediate error instead of a
  bare `400`.
- Bounded by [[serve-a-stale-catalog-for-an-unreachable-backend]]'s rule that
  a cached catalog never becomes a routing decision: here the catalog feeds
  health classification, not what chat is willing to attempt — but keep the
  two registries from bleeding into each other.
- Tests: a probe 400 followed by an absent catalog entry marks the model
  terminal and ends the schedule; a listed model keeps escalating; the walk's
  answer for a terminal model carries the no-usable-model ending; a catalog
  read failure degrades to today's behavior (keep probing).
