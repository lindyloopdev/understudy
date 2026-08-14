# The dev image lindy's own beats run in (DESIGN.md §Project Dev Image).
#
# Both pins are duplicated from .gitlab-ci.yml's `variables`, whose `test` and
# `lint` jobs are the environments a beat has to satisfy: a cycle that lints
# against a different version than CI reports a gate it did not pass.
FROM golangci/golangci-lint:v2.10.1 AS lint

FROM golang:1.26.5
COPY --from=lint /usr/bin/golangci-lint /usr/bin/golangci-lint
