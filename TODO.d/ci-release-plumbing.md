# CI / release plumbing

- `golangci-lint run ./...` + test workflows.
- Permissive-only dependency license scanner.
- Do not use the "Lindy" wordmark in the repo name or primary branding —
  understudy is its own project that lindy happens to consume.
- `goreleaser` (or equivalent) for single-binary releases — offer all of
  `go install`, binary download, and a Docker image; cover them in a README
  install section.
- Tag `v0.x` — Go semver v0 means no API-stability promise, which fits the
  feedback-gathering phase. Cut v1.0 only after the API has been
  pressure-tested; don't ship v1.0 on day one.
