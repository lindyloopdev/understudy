# State the health map's bound in DESIGN.md

**Tag:** understudy / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — per-target health
and the failover walk that reads it.
[DESIGN.md §Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization)
— health keying on `(url + key + model)`, of which the model half is whatever the
caller named.

A request naming `<backend>/<model>` directly puts a caller-supplied string in the
health key, so the set of entries a client can mint is whatever it cares to type.
The TTL sweep is the bound: idle entries retire, and a caller minting faster than
`healthTTL` retires them is a hostile caller understudy does not otherwise defend
against.

- Say so in DESIGN.md, including that the sweep reclaims the whole map rather than
  the key being written — the property the bound rests on. The eviction window is a
  constant in the code with no design section governing it, unlike the tenant
  registry's, which §Daemon idle eviction settles.

## Out of scope

- **A size cap on the map.** It would bound write latency as well as memory — the
  sweep is O(n) under the lock on every write — but it needs an eviction order and a
  size a consumer can read before it can be stated as a behavior anyone can check,
  and no consumer can see the map today.
