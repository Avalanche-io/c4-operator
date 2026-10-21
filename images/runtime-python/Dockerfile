# c4-runtime-python: the runtime a Python tool runs on (TOOL.md 2.3):
# everything in c4-runtime-base, and Debian's Python 3.13 with a venv on
# PATH, pip, NumPy and PyYAML. A tool brings anything else it needs in its
# own tree (pure-Python modules beside its script, on sys.path already) or
# runs on an image of its own.
#
# NumPy because most pipeline scripts that touch pixels, curves or caches
# reach for it, and PyYAML because tools read YAML (their own tool.yaml,
# a show's config). Nothing else, so the image stays small and a tool's
# version is mostly its tree.
#
# The venv is read-only at run time (the root filesystem is), so a tool
# cannot pip install into it; `python3 -m venv --system-site-packages` in
# a writable folder works when a test needs to.
#
# Build from the repository root: make runtime-images (or
#   docker buildx build -f images/runtime-python/Dockerfile .)

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
# (end of the steps every runtime shares; c4-runtime-torch shares the next
# one too)
ARG PIP=26.2.2
ARG NUMPY=2.5.4
ARG PYYAML=6.0.3
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "pip==${PIP}" "numpy==${NUMPY}" "PyYAML==${PYYAML}" \
 && find / -xdev -type f -perm /6000 -exec chmod ug-s {} +
ENV PATH=/opt/venv/bin:$PATH PYTHONPATH=/opt/c4/python HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache \
    PYTHONUNBUFFERED=1
LABEL org.opencontainers.image.title=c4-runtime-python \
      org.opencontainers.image.description="C4 runtime: Python 3.13 with NumPy and PyYAML for Python tools, and c4-unit with the tool runner" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
