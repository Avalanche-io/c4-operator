# c4-job-oiio: OpenImageIO's oiiotool with OpenColorIO 2.2+ built in, for
# comps written as oiiotool scripts and for every frame preview.
#
# Debian 13 packages OpenColorIO 2.1, which predates the built-in configs
# (ocio://studio-config-latest), so oiiotool comes from the OpenImageIO
# wheel, which carries its own OpenImageIO and color libraries, for amd64
# and arm64 alike.
#
# Build from the repository root: make job-images (or
#   docker buildx build -f images/oiio/Dockerfile .)

ARG DEBIAN=debian:13.7-slim
ARG GO=golang:1.26.9-trixie

FROM ${GO} AS unit
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/c4-unit ./cmd/c4-unit
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/c4-unit ./cmd/c4-unit

FROM ${DEBIAN}
ARG OIIO=3.2.1.3
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "OpenImageIO==${OIIO}" \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
ENV PATH=/opt/venv/bin:$PATH PYTHONPATH=/opt/c4/python HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache
LABEL org.opencontainers.image.title=c4-job-oiio \
      org.opencontainers.image.description="C4 job image: oiiotool with OpenColorIO's built-in ACES configs, and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
