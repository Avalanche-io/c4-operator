# c4-job-sim: fluid sims baked to OpenVDB caches with Blender's Mantaflow,
# headless (blender -b), for both architectures.
#
# Why Debian sid: Mantaflow writes OpenVDB only when Blender is built with
# it. Debian 13 and forky build Blender with OpenVDB on amd64 alone, and
# blender.org publishes Linux builds for x64 alone; sid's Blender 5.2.2 (snapshot 2026-10-15; the 10-05 one has 5.0.1) is
# built with OpenVDB on amd64 and arm64. The packages come from a dated
# snapshot of the archive, so a rebuild installs exactly the same ones.
#
# Houdini sims use the studio's own image (houdini.Dockerfile, built from
# its installer and licensed through hserver); the job template is the same.

ARG SNAPSHOT=20261015T000000Z
ARG DEBIAN=debian:sid-20261005-slim
ARG GO=golang:1.26.9-trixie

FROM ${GO} AS unit
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/c4-unit ./cmd/c4-unit
# the packages c4-unit imports from this module (images_test.go checks)
COPY internal/needs ./internal/needs
COPY schema ./schema
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/c4-unit ./cmd/c4-unit

FROM ${DEBIAN}
ARG SNAPSHOT
ARG OIIO=3.2.1.3
RUN rm -f /etc/apt/sources.list.d/debian.sources \
 && echo "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${SNAPSHOT} sid main" > /etc/apt/sources.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends blender python3 python3-venv ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "OpenImageIO==${OIIO}" \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
ENV PATH=$PATH:/opt/venv/bin PYTHONPATH=/opt/c4/python HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache
LABEL org.opencontainers.image.title=c4-job-sim \
      org.opencontainers.image.description="C4 job image: Blender Mantaflow sims to OpenVDB, headless, and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
