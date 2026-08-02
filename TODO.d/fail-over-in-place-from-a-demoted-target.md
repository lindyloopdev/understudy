# Fail over in place from a target past the failover threshold

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — the Retry-After
ladder's "next target healthy → fail over in place" rung, and the blip exemption
that scopes it ("a 5xx *blip* is still retried in place **under** the failover
threshold"); also the failover walk and the per-target health it reads.
[DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing) (the demand-triggered
probe that decides when a demoted target is offered a request at all).

A 5xx from a target already past the failover threshold is surfaced to the client
even when an untried alternate is healthy: only `sustainedRate` and refused access
replay onto the next target today. So a demoted target's request ends in a 502 the
candidate list could have routed around.

- Widen the within-request replay condition (the
  `sustainedRate || isAccessRefused` branch in `chatCompletions`) to include a
  failure from a target whose streak is past the failover threshold.
- Keep the blip exemption: a target *within* the threshold is still retried in
  place, not failed over.
- The terminal reject already requires an exhausted list, so it stays reachable
  only once the widened replay has nowhere left to go.
