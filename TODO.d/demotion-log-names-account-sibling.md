# `backend down` can name a backend that never failed

**Tag:** understudy / observability / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (per-target health
and the failover walk that emits the transition),
[DESIGN.md §Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization)
(health keys on the canonical `(url + key + model)`, which is what makes two
config names collide on one entry).

`noteBackendDown` logs `t.backend` for the target `pickTarget` is currently
skipping, but health is keyed by `healthKey` — the canonical account + model,
not the config's backend name. Two backend entries pointing at the same base URL
and credential share one health entry, so a failure on one is announced under
whichever sibling the walk reaches first. The operator reads `backend down X`
for an X that was never called.

The coalescing itself is correct and deliberate (it is what stops the global cap
fragmenting per-tenant); only the line's wording is wrong — it presents an
account-level fact in backend-name terms.

Name the target that actually failed, or name the account rather than an
arbitrary alias. Same for the `backend up` line in `clearFailure`, which is
paired with it and has the same aliasing.
