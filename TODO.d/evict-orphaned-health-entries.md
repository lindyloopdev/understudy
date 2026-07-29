# Evict health entries no live target can reach

**Tag:** understudy / fallback

**Design:** [DESIGN.md §Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization)
(health keys on the canonical `(url + key + model)`, so a rotated credential is a
different key), [DESIGN.md §Understudy](../DESIGN.md#understudy) (the per-target
health the map holds), [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing)
(the probe schedule that would iterate these entries).

`s.health` only ever grows. A rotated credential mints a new key and strands the
old entry, and a validator that stops returning a backend strands its entries too
— nothing removes either, and understudy cannot ask which keys are still live,
since the backend set arrives per request from the [TokenValidator].

- Bound the map by age: drop an entry untouched for long enough that no
  in-flight or future request can be measuring it.
- Sequence before [[demand-triggered-recovery-probe]] if that lands first: a
  probe schedule walking stranded entries would spend requests on accounts no
  token still routes to.
