# c4-job-media: ACES-managed review movies. oiiotool (from the OpenImageIO
# wheel, with OpenColorIO's built-in configs) applies the ACES output
# transform; FFmpeg (Debian 13, 7.1) writes ProRes 422 HQ and 4444
# (prores_ks) and DNxHR HQX (dnxhd), all native encoders.

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
 && apt-get install -y --no-install-recommends python3 python3-venv ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "OpenImageIO==${OIIO}" \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
ENV PATH=/opt/venv/bin:$PATH PYTHONPATH=/opt/c4/python HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache
LABEL org.opencontainers.image.title=c4-job-media \
      org.opencontainers.image.description="C4 job image: ACES review movies (ProRes, DNxHR) with oiiotool and FFmpeg, and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
