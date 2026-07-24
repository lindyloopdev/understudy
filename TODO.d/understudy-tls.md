# Serve understudy over TLS so the token + payload aren't cleartext on the bridge

**Tag:** understudy / security

**Design:** [UNDERSTUDY.md §LLM API Keys via Understudy](../UNDERSTUDY.md#llm-api-keys-via-understudy)
(transport encryption), [DESIGN.md §Host-Side Security Invariants](../DESIGN.md#host-side-security-invariants)
(only the minimum crosses the wall — the understudy endpoint + token),
[DESIGN.md §Container Lifecycle](../DESIGN.md#container-lifecycle)
(the read-only mounts + `ExtraHosts` reachability the cert and CA ride on).

The shared proxy daemon now serves understudy over TLS (`serveTLS`/`serveOnCert`,
CA-pinned) for `lindy run` and `lindy review`; the retired loopback path served it
over plain HTTP. The opencode/Bun CA-trust mechanism the TLS path reuses
is validated in
[notes/2026-07-10-opencode-bun-self-signed-tls-spike.md](../notes/2026-07-10-opencode-bun-self-signed-tls-spike.md).

- **Serve lindyd over TLS**: relocate the TLS primitives (`selfSignedCert`,
  `serveOnCert`) out of `internal/cli` so `server.go` (lindyd) can serve
  understudy over TLS. The loopback `lindy review` gateway stays plain.
- **Startup smoke test:** one HTTPS fetch through opencode to understudy, failing
  loudly — guards against an opencode/Bun bump regressing Bun's CA handling.
- **Shared-daemon variant:** a per-`$HOME` CA whose cert the daemon returns to
  each registering run in the control response, so the client can trust the HTTPS
  data plane ([[understudy-shared-daemon-subserver]]).
