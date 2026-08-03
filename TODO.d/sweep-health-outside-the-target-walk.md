# Bound the health map against a caller minting keys

**Tag:** understudy / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — per-target health
and the failover walk that reads it.
[DESIGN.md §Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization)
— health keying on `(url + key + model)`, of which the model half is whatever the
caller named.

A request naming `<backend>/<model>` directly puts a caller-supplied string in the
health key, so the set of entries a client can mint is whatever it cares to type.

- Decide whether a TTL alone is the bound. It answers the idle case but not a
  caller minting fresh keys faster than `healthTTL` retires them; a size cap
  answers that and needs an eviction order to go with it. The sweep is O(n) and
  now runs on every health write, so a map a caller has inflated makes each
  subsequent write costlier under the lock — the cap bounds latency, not just
  memory.
- Give the map an observable size before specifying the cap. Nothing reports how
  many entries `s.health` holds — no metric, no control-plane read — so neither
  the bound nor the sweep that feeds it can be stated as a behavior anyone can
  check. A cap phrased as "evict the least-recently-touched entry once the map
  exceeds a bound" cannot be tested against a map no consumer can see, and the
  reason the sweep on the refusal path is untestable is the same absence.
- State the policy in DESIGN.md, including that the sweep reclaims the whole map
  rather than the key being written — the property the bound rests on. The
  eviction window is a constant in the code with no design section governing it,
  unlike the tenant registry's, which §Daemon idle eviction settles.
