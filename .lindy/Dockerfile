# The dev image this repository's lindy beats run in.
#
# Pins mirror .github/workflows/ci.yml, whose test and lint jobs are the gates a
# beat has to pass: Go from go.mod, gopherlint at GOPHERLINTVERSION, and
# goimports from go.mod's golang.org/x/tools. A cycle that lints with a different
# linter than CI reports a gate it did not pass.
FROM golang:1.26.5

ARG GOPHERLINT_VERSION=1.26.3
RUN set -eu; \
    name="gopherlint-${GOPHERLINT_VERSION}-linux-$(dpkg --print-architecture)"; \
    base="https://github.com/flimzy/gopherlint/releases/download/v${GOPHERLINT_VERSION}"; \
    cd /tmp; \
    curl -sSfLO "${base}/${name}.tar.gz"; \
    curl -sSfL "${base}/gopherlint-${GOPHERLINT_VERSION}-checksums.txt" \
        | grep " ${name}.tar.gz\$" | sha256sum -c -; \
    tar -xzf "${name}.tar.gz"; \
    install "${name}/gopherlint" /usr/local/bin/gopherlint; \
    rm -rf "${name}" "${name}.tar.gz"

# gopherlint is a golangci-lint fork; the golangci-lint name is the one a
# cycle's instructions use, so it resolves to CI's linter rather than failing.
RUN ln -s /usr/local/bin/gopherlint /usr/local/bin/golangci-lint

RUN go install golang.org/x/tools/cmd/goimports@v0.48.0
