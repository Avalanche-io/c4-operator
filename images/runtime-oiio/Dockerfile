# c4-runtime-oiio: the runtime an image tool runs on (TOOL.md 2.3):
# everything in c4-runtime-base, and OpenImageIO's oiiotool and Python
# bindings with OpenColorIO 2.2+ built in (ocio://studio-config-latest),
# from the OpenImageIO wheel, which carries its own image and color
# libraries for amd64 and arm64 alike (Debian 13's OpenColorIO is 2.1).
#
# Build from the repository root: make runtime-images (or
#   docker buildx build -f images/runtime-oiio/Dockerfile .)

ARG DEBIAN=debian:13.7-slim
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
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash coreutils findutils ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4 \
 && find / -xdev -type f -perm /6000 -exec chmod ug-s {} +
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
# (end of the steps every runtime shares)
ARG PIP=26.2.2
ARG OIIO=3.2.1.3
ARG NUMPY=2.5.4
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "pip==${PIP}" "OpenImageIO==${OIIO}" "numpy==${NUMPY}" \
 && find / -xdev -type f -perm /6000 -exec chmod ug-s {} +
ENV PATH=/opt/venv/bin:$PATH PYTHONPATH=/opt/c4/python HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache \
    PYTHONUNBUFFERED=1
LABEL org.opencontainers.image.title=c4-runtime-oiio \
      org.opencontainers.image.description="C4 runtime: oiiotool and OpenImageIO for Python with OpenColorIO's built-in configs, and c4-unit with the tool runner" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
