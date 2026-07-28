# Probe interval for a refused credential

**Tag:** understudy / fallback / ha

**Design:** [DESIGN.md §Understudy](../DESIGN.md#understudy) (per-target health and
the half-open re-probe this tunes),
[DESIGN.md §LLM API Keys via Understudy](../DESIGN.md#llm-api-keys-via-understudy)
(the refused-credential class it would apply to).

`recoveryInterval` is 30s, tuned for faults that heal on their own. A refused
credential (`401`, `402`) heals only by out-of-band operator action — a topped-up
balance, a rotated key — so probing every 30s spends a request per interval on a
target that cannot recover in that window. Settle empirically whether this class
wants a longer interval; a constant, not a mechanism.
