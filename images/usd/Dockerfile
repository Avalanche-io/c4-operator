# c4-job-usd: OpenUSD's command-line tools (usdcat --flatten, usdchecker,
# usdzip, usdtree, usddiff, usdresolve), core libraries only.
#
# The usd-core wheel on PyPI carries no command-line tools and has no
# Linux arm64 build (26.8 ships manylinux x86_64 only), so OpenUSD is built
# from source here, without imaging, for both architectures.

ARG DEBIAN=debian:13.7-slim
ARG GO=golang:1.26.9-trixie

FROM ${GO} AS unit
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/c4-unit ./cmd/c4-unit
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/c4-unit ./cmd/c4-unit

FROM ${DEBIAN} AS build
ARG USD=v26.08
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates \
      python3 python3-dev libtbb-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch ${USD} https://github.com/PixarAnimationStudios/OpenUSD.git /src/usd
RUN cmake -S /src/usd -B /build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/usd \
      -DPXR_ENABLE_PYTHON_SUPPORT=ON -DPXR_BUILD_IMAGING=OFF -DPXR_BUILD_USD_IMAGING=OFF \
      -DPXR_BUILD_USDVIEW=OFF -DPXR_BUILD_EXAMPLES=OFF -DPXR_BUILD_TUTORIALS=OFF -DPXR_BUILD_TESTS=OFF \
      -DPXR_BUILD_DOCUMENTATION=OFF -DPXR_ENABLE_MATERIALX_SUPPORT=OFF \
 && cmake --build /build -j"$(nproc)" --target install

FROM ${DEBIAN}
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 libtbb12 zlib1g \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=build /opt/usd /opt/usd
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
ENV PATH=/opt/usd/bin:$PATH PYTHONPATH=/opt/usd/lib/python:/opt/c4/python LD_LIBRARY_PATH=/opt/usd/lib \
    HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache
LABEL org.opencontainers.image.title=c4-job-usd \
      org.opencontainers.image.description="C4 job image: OpenUSD command-line tools (flatten, check, package), and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
