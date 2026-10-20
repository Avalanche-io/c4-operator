# c4-runtime-base: the runtime a bash tool runs on (TOOL.md 2.3): bash,
# coreutils, findutils and CA certificates on Debian, with c4-unit, whose
# `c4-unit run` is the tool runner. A tool's script reaches the unit as
# content at $C4_TOOLS, so nothing about the tool is in the image.
#
# It runs as uid 65532 with nothing setuid, so it is admitted under Pod
# Security "restricted" and runs with a read-only root filesystem.
#
# The other runtimes (oiio, python, torch) begin with these same steps,
# word for word, so one `make runtime-images` builds the shared layers once
# and the images share them in the registry and on every node. Change them
# in all four files or in none.
#
# Build from the repository root: make runtime-images (or
#   docker buildx build -f images/runtime-base/Dockerfile .)

ARG DEBIAN=debian:13.7-slim
ARG GO=golang:1.26.9-trixie

FROM ${GO} AS unit
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/c4-unit ./cmd/c4-unit
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/c4-unit ./cmd/c4-unit

FROM ${DEBIAN}
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash coreutils findutils ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4 \
 && find / -xdev -type f -perm /6000 -exec chmod ug-s {} +
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
# (end of the steps every runtime shares)
ENV HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache PYTHONPATH=/opt/c4/python
LABEL org.opencontainers.image.title=c4-runtime-base \
      org.opencontainers.image.description="C4 runtime: bash, coreutils and findutils for bash tools, and c4-unit with the tool runner" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
