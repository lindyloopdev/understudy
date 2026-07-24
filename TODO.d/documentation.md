# Documentation

- Port lindy's `UNDERSTUDY.md` to the library's primary doc. Keep the
  availability-not-quality framing, the rate-limit ladder, and the
  session-binding reasoning as library docs; move the lindy-specific framing
  (container-as-security-boundary, per-scene token mint, provenance stream)
  into a "How lindy uses understudy" section. This doc becomes the design
  backref home for the other TODO items.
- README: positioning vs LiteLLM/one-api/OpenRouter (Go, single-binary,
  library-or-daemon, opinionated availability layer, explicit
  availability/quality boundary); 2-line quickstart; "From the upcoming Lindy
  project" section high up (the awareness hook — lindy is the upcoming primary
  consumer). Include an honest capability-variance note (failover across rough
  equivalents is fine for chat/iteration; pin a single smart model when an
  agent needs consistent capability — don't oversell) and a TOS disclaimer
  ("use your own keys; follow each provider's terms").
- `examples/free-tiers.toml`: commented sample config with multiple free-tier
  providers (Gemini free, Groq, Cerebras, OpenRouter `:free`, Mistral free),
  grouped by capability tier (`fast`, `smart`), each backend commented with
  signup URL + limit shape + last-verified date; `default` → `fast`. The killer
  demo — exercises failover depth in 5 minutes at zero cost, where a
  single-provider demo hides it.
- `providers-tested.md` matrix: provider · OpenAI-compat endpoint · free-tier
  limit shape · last-verified date · quirks. Re-verify monthly (free tiers rot;
  Cerebras has paused theirs before).
- `CONTRIBUTING.md` + issue template.
