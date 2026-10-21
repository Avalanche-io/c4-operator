# c4-runtime-torch: the runtime a model tool runs on (TOOL.md 2.3):
# everything in c4-runtime-python (Python 3.13, NumPy, PyYAML), and
# PyTorch for the CPU. A GPU build is the same file with
#   --build-arg TORCH_INDEX=https://download.pytorch.org/whl/cu128
# on linux/amd64. Models are not in the image: a tool reads them from
# its tree or the farm's model cache.
#
# Build from the repository root: make runtime-images (or
#   docker buildx build -f images/runtime-torch/Dockerfile .)

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
ARG TORCH=2.14.1
ARG TORCH_INDEX=https://download.pytorch.org/whl/cpu
RUN /opt/venv/bin/pip install --no-cache-dir --index-url "${TORCH_INDEX}" "torch==${TORCH}"
ENV PATH=/opt/venv/bin:$PATH PYTHONPATH=/opt/c4/python HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache \
    PYTHONUNBUFFERED=1
LABEL org.opencontainers.image.title=c4-runtime-torch \
      org.opencontainers.image.description="C4 runtime: PyTorch with NumPy and PyYAML for model tools, and c4-unit with the tool runner" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
