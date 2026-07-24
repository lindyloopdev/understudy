# Per-target request-body normalization: enable-injection (tri-state)

**Tag:** understudy / fallback / cost

**Design:** [UNDERSTUDY.md §Understudy](../UNDERSTUDY.md#understudy) — "Per-target
request-body normalization." Reasoning/rejections:
[notes/2026-07-05-understudy-per-target-body-normalization.md](../notes/2026-07-05-understudy-per-target-body-normalization.md).

Disabling thinking (`?thinking=false`) is shipped. When a driver appears, add the
enabling side so the override is tri-state (absent / disabled / enabled):
`?thinking=true` → inject `thinking:{type:enabled}`, replacing any client value.
`thinking=true` currently errors as reserved at `Config.Resolve`, and `Target`
carries the matching inline `TODO` marker; both flip when this lands.
