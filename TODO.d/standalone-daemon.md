# Standalone understudyd daemon

Port the simple-server subset of lindy's `internal/cli/proxy.go` +
`internal/cli/registry.go` into `cmd/understudyd/` — a thin main wrapping the
library.

- Single process, one TOML config, cleartext on localhost by default, TLS
  optional. Hold back the rendezvous file / spawn-lock / version-handshake
  machinery — host-orchestration concerns, not a standalone user's.
- Ship multi-tenant registry mode as an *optional* mode (the `registry.go`
  code travels) — showcases the credential-broker use case that differentiates
  understudy.
- Hold back the per-tenant `/requestlog` provenance stream — it is lindy's
  `ResponseInterceptor` implementation, kept in lindy as a study example, not a
  v0 server feature.
