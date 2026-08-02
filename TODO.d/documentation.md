# Documentation

- Restructure `DESIGN.md` (ported verbatim from lindy) to read as a library
  doc: move the lindy-specific framing (container-as-security-boundary,
  per-scene token mint, provenance stream) out of the main flow into a "How
  lindy uses understudy" section, leaving the availability-not-quality framing,
  the rate-limit ladder, and the session-binding reasoning as the library core.
- README: positioning vs LiteLLM/one-api/OpenRouter (Go, single-binary,
  library-or-daemon, opinionated availability layer, explicit
  availability/quality boundary); 2-line quickstart; "From the upcoming Lindy
  project" section high up (the awareness hook — lindy is the upcoming primary
  consumer). Include an honest capability-variance note (failover across rough
  equivalents is fine for chat/iteration; pin a single smart model when an
  agent needs consistent capability — don't oversell) and a TOS disclaimer
  ("use your own keys; follow each provider's terms").
- `examples/free-tiers.toml` — the killer demo (failover depth in 5 minutes at
  zero cost, where a single-provider demo hides it). The file itself is
  [[free-tier-drop-in-config]]; the README must link and explain it.
- The failure-translation table ([[understudy-error-envelope-type]]) — status,
  envelope `type`, and `Retry-After` per upstream condition. A consumer dispatches
  on it, so it belongs in the library doc rather than only in the code.
- `providers-tested.md` matrix: provider · OpenAI-compat endpoint · free-tier
  limit shape · last-verified date · quirks. Re-verify monthly (free tiers rot;
  Cerebras has paused theirs before).
- `CONTRIBUTING.md` + issue template.
