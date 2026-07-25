# Fail over on a refused or unfunded credential (401, 402)

**Tag:** understudy / fallback / ha / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (the availability
ladder and per-target health this joins),
[DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(a refused credential is distinct from an absent one, which `auth` handles).

`isFatalUpstream` matches only `502`, and the walk advances on
`transientRate || isFatalUpstream`, so an upstream `401` or `402` surfaces to the
client instead of moving to the next target. Both are availability facts —
the target cannot serve, a healthy sibling can:

    APIError: upstream returned status 402: Insufficient Balance   (deepseek)

A cost-ordered chain puts the cheap paid backend first, so exhausting its balance
is the ordinary consequence of correct ordering, not a misconfiguration. The
request should keep working.

- Advance the target walk on `401` and `402`, and demote the target.

That is the whole change. No new logging: `noteBackendDown`/`clearFailure` already
emit one `backend down` / `backend up` pair per transition, and `downLogged`
survives a failed re-probe, so a persistently refused credential does not repeat.
No recovery mechanism either: the half-open probe re-offers the target every
`recoveryInterval`, so a topped-up balance or a rotated key is picked up
automatically without a restart or reload.

**Open — probe interval for this failure class.** `recoveryInterval` is 30s, tuned
for faults that heal on their own. A refused credential heals only by out-of-band
operator action, so probing every 30s spends a request per interval on a target
that cannot recover in that window. Settle empirically whether these want a longer
interval; a constant, not a mechanism, so it need not block the change above.

**Ordering: must not land before the `auth = "auto"` drop**
([[auth-requirement-and-key-env-source]]). On its own, an unedited
`examples/free-tiers.toml` would call every unconfigured backend, and the wall of
auth failures is what the drop exists to prevent. The two cover different failures
and neither subsumes the other — the drop catches an **absent** credential at
startup, this catches a **refused or unfunded** one at runtime, which no
config-time check can see.

The envelope `type` an upstream 401 carries to the client stays open in
[[understudy-error-envelope-type]]; this item settles only the availability path.
Getting the `backend down` line to an operator who is not watching the log is the
delivery gap in [[understudy-scope]] (§Provenance reporting), not this item.
