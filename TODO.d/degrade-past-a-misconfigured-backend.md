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
  reason is already on `Excluded`; only the one promoted into the answer is
  arbitrary. No case pins which one, so nothing has to change alongside.

- **Decide whether re-walking records a target again.** `pickTarget` re-walks the
  candidate list from the start on every failover, so targets `[broken, limited,
  good]` where `limited` fails over yield `broken, limited, broken`. The walk-log
  reading is settled — `DESIGN.md` and the `Excluded` doc both say candidates are
  recorded in the order they were walked, and a case pins that interleaving — but
  nothing says whether walking past the same unusable backend twice should record it
  twice, and no case exercises it. If it should, the field name is the problem:
  `Excluded` reads as a set, and a set does not repeat. lindy is the only consumer
  that could settle it, and reads none of this today.

- **A listing's failed catalog fetch reaches only the log.** The listing's two
  omission paths answer a consumer differently: a backend understudy cannot use is
  recorded on `Excluded`, while one whose catalog fetch fails is written to
  `s.logger` at ERROR and left off the record. Both are the same fact from the
  consumer's side — this backend is missing from your listing, and here is why.

- **`/v1/models` with nothing configured still answers 500** (`errNoBackendConfigured`
  on `!matched`), which reads against "emptiness is a valid answer whatever its
  cause". Settle whether the listing may fail at all, or whether zero usable
  backends is simply an empty catalog. Test surface: "should return 500 when no
  backend configured". It is the sole remaining use of `errNoBackendConfigured`, so
  retiring it retires the error too.

- **Decide whether a consumer needs to tell the skip reasons apart.** `Attempt.Called`
  separates a backend understudy never called from one it called and abandoned, but
  the reasons within a skip — no such backend, no registered handler, no base URL —
  are distinguishable only by reading the message, since each is unexported. Settle
  whether that distinction is one a consumer acts on, and so whether the reasons
  become exported sentinels and a frozen contract.

## Out of scope

- **Failover for a target unusable at runtime.** Refused access or an
  unreachable host still demotes and is walked past; only *static* unusability is
  a skip. See [[fail-over-in-place-from-a-demoted-target]].
- **An unset credential under `auth = "auto"`.** The document is correct and the
  world is not supplying a key, which seeds health state rather than skipping —
  see [[auth-requirement-and-key-env-source]] and [[resolve-validate-split]].
- **Exposing the registered provider set** so a consumer can pre-check
  routability itself. Deliberately deferred until one needs it; a consumer can
  already enforce any rule it *can* see from its own `TokenValidator`.
