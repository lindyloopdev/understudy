# Degrade past a misconfigured backend instead of failing every request

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Least
degradation: a target understudy cannot use is one target's problem", and the
availability-failover walk it generalizes.

A backend with a nil base URL fails **every** request against the config, not
just the ones that would have touched it. The auth middleware
(`understudy.go:1009-1015`) loops over all backends before any handler runs and
500s on the first malformed one, deliberately — "regardless of map iteration
order", to keep the error deterministic. That buys determinism at the cost of
availability: given a logical model with targets `[x/a, y/b, z/a]` and a
misconfigured `y`, a request that resolves to `x/a` dies at the trust boundary
having never needed `y`.

Past the middleware the same request would still not degrade. The within-request
walk (`untriedTargets` / the `continue`s in `chatCompletions`) is gated on
`logicalTargets != nil` and driven by *runtime* classifications — a header stall,
`sustainedRate`, a refused credential. A config defect is not in that vocabulary,
so a target that fails because it is malformed reaches `terminalFailure` on the
first attempt rather than advancing to `z/a`.

Least degradation: a target understudy cannot use is one target's problem.

- Stop rejecting the whole request for a backend the request never resolves to.
  Determinism is still available without the blanket pre-flight — validate the
  backend a target actually names, at the point it is chosen.
- Treat a malformed target as an availability failure: demote it and advance to
  the next untried target, the same walk a refused credential takes.
- Surface it terminally only at exhaustion — a 500 when the misconfigured target
  was the last candidate, exactly as `terminalFailure` already handles a walk
  with nowhere left to go.
- Report the skipped target through `LogRecord.FailedOver` (`addLogFailedOver`),
  which already exists to make a demotion attributable. No new log line — and
  not a WARN.

Distinct from [reject-unconfigured-logical-model.md](reject-unconfigured-logical-model.md):
that one is a name with no targets at all, this one is a target list whose
members are unequally usable.
