# c4-job-hydra: renders a USD stage through a Hydra render delegate to
# OpenEXR with usdrecord, headless, on the CPU. The delegate is Cycles
# (its Hydra delegate, hdCycles, from the Cycles repository), so the local
# farm renders the same way a studio renders with Karma, RenderMan,
# Arnold or V-Ray: USD in, a delegate, EXR out.
#
# Everything is built from source for both architectures: there is no
# OpenUSD imaging package in Debian and no arm64 usd-core wheel, and the
# delegate must be built against the exact OpenUSD it loads into.
#
# OpenUSD is built with GL support so usdrecord and UsdAppUtils are built;
# usdrecord runs with --disableGpu, which keeps it to CPU delegates and
# never opens a GL context, so no display or GPU is needed at run time.
#
# The fallback, for a first run where this recipe does not hold, is
# Dockerfile.blender (amd64 only): see README.md.

ARG DEBIAN=debian:13.7-slim
ARG GO=golang:1.26.9-trixie

FROM ${GO} AS unit
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/c4-unit ./cmd/c4-unit
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/c4-unit ./cmd/c4-unit

FROM ${DEBIAN} AS build
ARG USD=v26.08
ARG OSD=v3_7_0
ARG CYCLES=v5.2.0
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates pkg-config \
      python3 python3-dev libtbb-dev zlib1g-dev libgl-dev libx11-dev \
      libopenimageio-dev libopenexr-dev libimath-dev libpugixml-dev libjpeg-dev libpng-dev libtiff-dev \
 && rm -rf /var/lib/apt/lists/*
# OpenSubdiv, CPU only, for USD imaging
RUN git clone --depth 1 --branch ${OSD} https://github.com/PixarAnimationStudios/OpenSubdiv.git /src/osd \
 && cmake -S /src/osd -B /build/osd -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/usd \
      -DNO_EXAMPLES=ON -DNO_TUTORIALS=ON -DNO_REGRESSION=ON -DNO_DOC=ON -DNO_OMP=ON -DNO_CUDA=ON \
      -DNO_OPENCL=ON -DNO_GLFW=ON -DNO_PTEX=ON -DNO_TBB=OFF -DNO_OPENGL=ON -DNO_METAL=ON -DNO_DX=ON \
 && cmake --build /build/osd -j"$(nproc)" --target install
# OpenUSD with imaging, usdImaging, Python and the usdrecord tool
RUN git clone --depth 1 --branch ${USD} https://github.com/PixarAnimationStudios/OpenUSD.git /src/usd \
 && cmake -S /src/usd -B /build/usd -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/usd \
      -DCMAKE_PREFIX_PATH=/opt/usd -DPXR_ENABLE_PYTHON_SUPPORT=ON \
      -DPXR_BUILD_IMAGING=ON -DPXR_BUILD_USD_IMAGING=ON -DPXR_ENABLE_GL_SUPPORT=ON \
      -DPXR_BUILD_USDVIEW=OFF -DPXR_BUILD_EMBREE_PLUGIN=OFF -DPXR_BUILD_PRMAN_PLUGIN=OFF \
      -DPXR_BUILD_OPENIMAGEIO_PLUGIN=OFF -DPXR_BUILD_OPENCOLORIO_PLUGIN=OFF -DPXR_ENABLE_MATERIALX_SUPPORT=OFF \
      -DPXR_ENABLE_VULKAN_SUPPORT=OFF -DPXR_BUILD_EXAMPLES=OFF -DPXR_BUILD_TUTORIALS=OFF -DPXR_BUILD_TESTS=OFF \
      -DPXR_BUILD_DOCUMENTATION=OFF \
 && cmake --build /build/usd -j"$(nproc)" --target install
# Cycles' Hydra render delegate, against that OpenUSD
RUN git clone --depth 1 --branch ${CYCLES} https://projects.blender.org/blender/cycles.git /src/cycles \
 && cmake -S /src/cycles -B /build/cycles -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/cycles \
      -DWITH_LIBS_PRECOMPILED=OFF -DPXR_ROOT=/opt/usd -DCMAKE_PREFIX_PATH=/opt/usd \
      -DWITH_CYCLES_HYDRA_RENDER_DELEGATE=ON -DWITH_CYCLES_STANDALONE=OFF -DWITH_CYCLES_STANDALONE_GUI=OFF \
      -DWITH_CYCLES_USD=ON -DWITH_CYCLES_EMBREE=OFF -DWITH_CYCLES_OSL=OFF -DWITH_CYCLES_OPENIMAGEDENOISE=OFF \
      -DWITH_CYCLES_OPENVDB=OFF -DWITH_CYCLES_NANOVDB=OFF -DWITH_CYCLES_ALEMBIC=OFF \
      -DWITH_CYCLES_DEVICE_CUDA=OFF -DWITH_CYCLES_DEVICE_OPTIX=OFF -DWITH_CYCLES_DEVICE_HIP=OFF \
      -DWITH_CYCLES_DEVICE_ONEAPI=OFF -DWITH_CYCLES_DEVICE_METAL=OFF \
 && cmake --build /build/cycles -j"$(nproc)" --target install \
 && find /opt/cycles -name plugInfo.json -print

FROM ${DEBIAN}
ARG OIIO=3.2.1.3
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv ca-certificates \
      libtbb12 zlib1g libgl1 libx11-6 libopenimageio2.5 libopenexr-3-1-30 libpugixml1v5 \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "OpenImageIO==${OIIO}" \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=build /opt/usd /opt/usd
COPY --from=build /opt/cycles /opt/cycles
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
# usdrecord comes after the venv on PATH, so `python3` in its shebang is
# the system interpreter OpenUSD was built for.
ENV PATH=/opt/usd/bin:/usr/bin:/opt/venv/bin:/usr/local/bin:/bin \
    PYTHONPATH=/opt/usd/lib/python:/opt/c4/python LD_LIBRARY_PATH=/opt/usd/lib:/opt/cycles/lib \
    PXR_PLUGINPATH_NAME=/opt/cycles/hydra \
    HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache
LABEL org.opencontainers.image.title=c4-job-hydra \
      org.opencontainers.image.description="C4 job image: USD to OpenEXR through Cycles' Hydra delegate with usdrecord, headless, and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
