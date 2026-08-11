# Log a transition where it happens

**Tag:** understudy / bug

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) — "A transition is logged
when it happens", and the health-transition exemption above it, which is what allows
understudy its own log line at all. [DESIGN.md §Recovery probing](../DESIGN.md#recovery-probing)
— demotion and half-open re-admission, the states these records report.

`recordImmediateFailure` is the last demotion that says nothing. A target refused
outright is reported by whichever later walk routes around it, so the record is dated
at the discovery rather than the refusal, cannot say what the backend answered, and is
never written at all when no further request comes. Move it to emit as `recordStalled`
and `recordRateLimited` do: the call has the cause, the error, and the moment in hand.

That also ends the walk's guessing. `noteBackendDown` infers a reason from
`readmitAt`, which is why an entry a stall benched would be named `upstream
retry-after` there and `no response header` at the demotion. Once every demotion
announces, an unannounced entry can only be one a streak accrued — `readmitAt` zero —
so the walk's `benchedUntil` branch becomes unreachable and goes with the move.

**The emission may not happen under `s.mu`.** `recordImmediateFailure` writes health
through `writeHealth`, which holds the lock for the whole write, so logging from
inside it puts a consumer's handler back under the lock every other request's walk
contends for — the fault fixed in 364782a. Compose the record where the cause is
known and emit after the lock is released, as the other paths do.

A streak that merely ages past the failover threshold has no such moment — nothing
runs when the threshold elapses. Where a further failure does the crossing, that
failure is the event; where the streak ages in silence, the discovering walk stays the
emission point, and dates the streak rather than the discovery.

**Which cause a record names is open.** A demotion announces for any entry whose
streak was never reported, so a target refused and later stalling is reported `no
response header`: the cause it is out for now, rather than the one that started the
streak. That is the same choice [[say-why-a-backend-went-down]] leaves open for the
error, and both should be answered once, together.

Emitters also claim `downLogged` under `s.mu` and emit after releasing it, so for a
target that already carries an entry, which reason an operator reads follows lock
order — [[decide-whether-transitions-are-ordered]].

Four behaviors go unasserted, all of them `bench`'s update path, which no test drives
for either caller: **should name the cause a target is out for now, not the one that
started its streak**; **should announce a demotion the streak never reported**;
**should stay silent when a target is demoted twice in one streak**; and **should say
when a benched target's return moves** — a re-bench overwrites `readmitAt` while
`downLogged` stays set, so the last record's `readmit_at` outlives the moment it
named. Answering the third decides the fourth: they are the same question about
whether a second demotion in one streak speaks.

Absorbs [[say-why-a-backend-went-down]]: the error is in hand at the demotion, and
nowhere later.

Guards: "should say an upstream's advertised backoff holds a target back" and "should
say understudy's own probe pacing holds a target back" pin reasons that must survive
the move; the transition-count cases pin once-per-streak, which it must still honor.
