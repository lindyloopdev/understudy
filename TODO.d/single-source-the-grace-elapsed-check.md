# Single-source the grace-elapsed check

**Tag:** understudy / admission / cleanliness

**Design:** [DESIGN.md §Session-Ordered Admission](../DESIGN.md#session-ordered-admission)
— the grace period itself, unaffected; this is purely about where its
elapsed-check is computed.

`o.arrived.Add(admissionGracePeriod).After(now)` (or its negation) is computed
inline at four sites in `admission.go`: the outranking loop's displacement
check, growth's hold-on-unconfirmed check, the wait-formation timer's arming
condition, and `formByWaiting`'s own displacement check. If the grace rule ever
changes — measured from a different timestamp, made per-target — all four need
updating together, and nothing makes that dependency visible.

Three of the four (the outranking loop, the timer-arming condition, and
`formByWaiting`) also share a second predicate on top of grace —
`o.headered || o.preempted` — since all three are asking "is this request
exempt from being taken." The fourth (growth's hold check) is not the same
rule: it deliberately omits `headered` entirely, since header state must not
gate growth's decision to admit (DESIGN.md's "Connections accepted are not
capacity"). A shared helper must not bundle `headered` into it, or reusing it
at growth's site would silently reintroduce that already-fixed bug.

## Work

- Extract only the grace-elapsed arithmetic into one method, e.g.
  `func (o *pending) pastGrace(now time.Time) bool`, and use it at all four
  sites. Leave `headered`/`preempted`/`confirmed` as separate conditions at
  each call site — do not fold them into the same helper, since they differ
  between the two predicates.
- Strict refactor: no behavior change, existing suite stays green, no new
  test.
