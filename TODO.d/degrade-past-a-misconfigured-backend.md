# Answer when nothing is usable, and report the skip everywhere

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Operator and caller
learn different things" (the skip reason reaches the operator through the request's
`LogRecord`), "The reason travels with the skip" (the terminal error carries it when
the skipping exhausted the candidates), "A list emptied by misconfiguration answers
404, not 500" (which ending each exhaustion gets, and that the rule is about usable
targets however the list emptied), and "Two endpoints, two answers" (emptiness is a
valid answer for the listing whatever its cause).

- **Surface the first failure, not the last target.** `pickTarget` returns
  `targets[len(targets)-1]` when every target is unusable, so the reason the caller
  reads is whichever backend sorts last rather than what first went wrong. Every
  reason is already on `FailedOver`; only the one promoted into the answer is
  arbitrary. No case pins which one, so nothing has to change alongside.

- **`/v1/models` reports no skip at all.** Its own `resolveBackend` rejection is a
  bare `continue`, so a backend that vanishes from the listing leaves no trace for
  the consumer, while the chat path records one on the request's `LogRecord`. The
  listing has no `LogRecord` convention today, so settle what carries the fact there
  before building it.

- **`/v1/models` with nothing configured still answers 500** (`errNoBackendConfigured`
  on `!matched`), which reads against "emptiness is a valid answer whatever its
  cause". Settle whether the listing may fail at all, or whether zero usable
  backends is simply an empty catalog. Test surface: "should return 500 when no
  backend configured". It is the sole remaining use of `errNoBackendConfigured`, so
  retiring it retires the error too.

- **Give a consumer something to match on.** `errNoSuchBackend` is unexported, so a
  consumer reading `FailedOver` can only tell a skip from a failed attempt by
  reading the message. Decide whether the skip reasons become exported sentinels —
  and whether that is a contract worth freezing — or whether the message is all a
  consumer should rely on.

## Out of scope

- **Failover for a target unusable at runtime.** A refused credential or an
  unreachable host still demotes and is walked past; only *static* unusability is
  a skip. See [[fail-over-in-place-from-a-demoted-target]].
- **An unset credential under `auth = "auto"`.** The document is correct and the
  world is not supplying a key, which seeds health state rather than skipping —
  see [[auth-requirement-and-key-env-source]] and [[resolve-validate-split]].
- **Exposing the registered provider set** so a consumer can pre-check
  routability itself. Deliberately deferred until one needs it; a consumer can
  already enforce any rule it *can* see from its own `TokenValidator`.
