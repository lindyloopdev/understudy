# Answer when nothing is usable, and report the skip everywhere

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "Operator and caller
learn different things" (the skip reason reaches the operator through the request's
`LogRecord`), "The reason travels with the skip" (the terminal error carries it when
the skipping exhausted the candidates), and "Two endpoints, two answers" (emptiness
is a valid answer for the listing whatever its cause).

- **Decide what a request gets when no target is usable.** `pickTarget` returns
  `targets[len(targets)-1]` when it falls off the end, which is reachable with every
  target unusable — so the caller's answer comes from whatever that target happens
  to do rather than from a decision. The design forbids the "unknown backend"
  falsehood but does not pick the replacement status: a truthful 404 carries the
  reason to the caller, while a 500 is honest about whose fault it is but has its
  body rewritten to "Internal Server Error" by `writeJSONError`, so the reason
  reaches only the log.

- **`/v1/models` reports no skip at all.** Its own `resolveBackend` rejection is a
  bare `continue`, so a backend that vanishes from the listing leaves no trace for
  the consumer, while the chat path records one on the request's `LogRecord`. The
  listing has no `LogRecord` convention today, so settle what carries the fact there
  before building it.

- **`/v1/models` with nothing configured still answers 500** (`errNoBackendConfigured`
  on `!matched`), which reads against "emptiness is a valid answer whatever its
  cause". Settle whether the listing may fail at all, or whether zero usable
  backends is simply an empty catalog. Test surface: "should return 500 when no
  backend configured".

- **Build the skip reasons once, not per call.** `resolveBackend` constructs a fresh
  error on each rejection — 145ns/16 B for the nil base URL, 568ns/64 B for an
  unregistered provider type, against 22ns and zero allocations when the backend is
  usable. A statically unusable backend never becomes usable, so it pays that on
  every target evaluation of every request forever. A package-level sentinel covers
  the base-URL case; the provider-type message interpolates, so it needs an error
  type carrying the type name. Sentinels also give a consumer `errors.Is` to
  distinguish a skip from a failed attempt, which the shared `FailedOver` list
  otherwise leaves to reading the message.

- **A target naming an absent backend reports a misleading reason.**
  `backends[t.backend]` yields the zero `Backend`, whose empty `ProviderType` fails
  the provider lookup, so the consumer reads `provider type "" has no registered
  handler` when the truth is that no such backend is declared. The skip itself is
  right; the reason blames the wrong thing, and it now travels to the consumer. No
  case pins either.

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
