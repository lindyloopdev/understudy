# Demand-triggered async recovery probe

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Recovery probing is demand-triggered and off the request
path](../DESIGN.md#recovery-probing) (the trigger, the schedule, and the two
rejected alternatives),
[DESIGN.md §Understudy](../DESIGN.md#understudy) (per-target health and the
failover walk the probe re-admits into),
[DESIGN.md §Upstream-identity canonicalization](../DESIGN.md#upstream-identity-canonicalization)
(health keys on `(url + key + model)` — why a rotated credential needs no probe at
all), [DESIGN.md §Control plane](../DESIGN.md#daemon-control-plane) (where an
operator's "recheck now" would live).

Today `pickTarget` re-admits a demoted target by handing it to a live client
request once `recoveryInterval` (30s) has elapsed, so that client pays the
probe's failure latency. A fatal (5xx) failure fails over to a healthy untried
alternate; a non-fatal one still reaches the client even with one in the list.

- Serve the triggering request from the first healthy target and launch the probe
  asynchronously, so nothing on the request path waits for it.
- Send a synthetic minimal chat completion, not the client's body.
- Escalate the interval from `failingSince`, jittered, capped ≈15m, reset on
  success; skip entirely while a `readmitAt` is known.
- Give the server a lifetime probes can outlive their triggering request within:
  a server-scoped context, a config copy taken at trigger, single-flight per
  target, and a shutdown that drains in-flight probes. `New` returns a bare
  `http.Handler` today, so this is new surface — settle whether it is an
  `Option`, a `Close`, or a constructor change.
- Calibrate the base interval and the cap empirically; both are constants, and
  `402` (no rotation heals it) is the class that decides the base.
