# c4-job-ci: test shards for a repository imported as a view: Go and
# Python toolchains, pytest with pytest-shard, gotestsum for JUnit
# reports, and c4-test, which runs this unit's shard of whatever the
# repository holds.

ARG DEBIAN=debian:13.7-slim
ARG GO=golang:1.26.9-trixie

FROM ${GO} AS unit
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/c4-unit ./cmd/c4-unit
# the packages c4-unit imports from this module (images_test.go checks)
COPY internal/needs ./internal/needs
COPY schema ./schema
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/c4-unit ./cmd/c4-unit \
 && GOBIN=/out go install gotest.tools/gotestsum@v1.13.0

FROM ${GO} AS go

FROM ${DEBIAN}
ARG PYTEST=9.1.1
ARG PYTEST_SHARD=0.1.2
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv git ca-certificates build-essential \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "pytest==${PYTEST}" "pytest-shard==${PYTEST_SHARD}" \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=go /usr/local/go /usr/local/go
COPY --from=unit /out/c4-unit /out/gotestsum /usr/local/bin/
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
COPY images/ci/c4-test /usr/local/bin/c4-test
# Module and build caches under HOME, so a mounted cache volume keeps them
ENV PATH=/usr/local/go/bin:/opt/venv/bin:$PATH PYTHONPATH=/opt/c4/python HOME=/home/c4 \
    XDG_CACHE_HOME=/home/c4/.cache GOPATH=/home/c4/go GOCACHE=/home/c4/.cache/go-build \
    GOFLAGS=-buildvcs=false
LABEL org.opencontainers.image.title=c4-job-ci \
      org.opencontainers.image.description="C4 job image: sharded Go and Python tests with JUnit reports, and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
