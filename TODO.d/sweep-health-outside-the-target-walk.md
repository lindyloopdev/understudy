# Sweep health entries a target walk never reaches

**Tag:** understudy / ha / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — per-target health
and the failover walk that reads it.
[DESIGN.md §Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization)
— health keying on `(url + key + model)`, of which the model half is whatever the
caller named.

`evictStaleHealth` runs from one place, `pickTarget`, which only a logical model
reaches. A request naming `<backend>/<model>` directly records health without ever
walking a candidate list, so an understudy driven only by references — or by a
caller cycling model names against a declared backend — grows `s.health` with
entries nothing sweeps. The key's model half is caller-supplied, so the ceiling is
whatever a client cares to type.

- Sweep from the write paths (`recordFailure`, `recordImmediateFailure`,
  `recordRateLimited`) rather than from the walk, so an entry is reclaimed by the
  same event that minted it.
- Decide whether a TTL alone is the bound. It answers the idle case but not a
  caller minting fresh keys faster than `healthTTL` retires them; a size cap
  answers that and needs an eviction order to go with it.
- State the policy in DESIGN.md. The health map's eviction window is a constant in
  the code with no design section governing it — unlike the tenant registry's,
  which §Daemon idle eviction settles.

The test belongs beside "should start a fresh streak once a demotion has gone
untouched past the eviction window" in `TestChatCompletionsFailoverRouting`: the
same two steps, both naming `a/ma` directly. Measured today, that pair answers a
terminal 400 `upstream_unavailable` on a day-old streak where the logical-model
case gets a fresh 502 `server_error`.
