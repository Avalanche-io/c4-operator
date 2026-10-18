# c4-job-infer: per-frame model inference over EXR plates: depth (Depth
# Anything V2 Small, Apache-2.0), mattes (BiRefNet, MIT) or 2x upscales
# (Swin2SR, Apache-2.0). PyTorch is the CPU build (amd64 and arm64); a GPU
# pool uses the CUDA variant, as for c4-job-train:
#   --build-arg TORCH_INDEX=https://download.pytorch.org/whl/cu128 --platform linux/amd64
#
# Model weights are not in the image. They download on first use into
# $HOME/.cache/huggingface; mount a cache volume there to keep them.

ARG DEBIAN=debian:13.7-slim
ARG GO=golang:1.26.9-trixie

FROM ${GO} AS unit
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/c4-unit ./cmd/c4-unit
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/c4-unit ./cmd/c4-unit

FROM ${DEBIAN}
ARG TORCH=2.14.1
ARG TORCH_INDEX=https://download.pytorch.org/whl/cpu
ARG TRANSFORMERS=5.20.1
ARG OIIO=3.2.1.3
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir --index-url "${TORCH_INDEX}" "torch==${TORCH}" \
 && /opt/venv/bin/pip install --no-cache-dir "transformers==${TRANSFORMERS}" "OpenImageIO==${OIIO}" \
      pillow timm einops kornia \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
COPY images/infer/c4-infer /usr/local/bin/c4-infer
ENV PATH=/opt/venv/bin:$PATH PYTHONPATH=/opt/c4/python HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache \
    HF_HOME=/home/c4/.cache/huggingface HF_HUB_DISABLE_TELEMETRY=1
LABEL org.opencontainers.image.title=c4-job-infer \
      org.opencontainers.image.description="C4 job image: per-frame depth, matte and upscale inference on EXR, and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
