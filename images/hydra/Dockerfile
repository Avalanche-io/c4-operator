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
# JOBS caps the compile's parallel jobs (default: every CPU)
ARG JOBS
# OpenUSD 26.03 is what Blender 5.2 builds Cycles 5.2 against; 26.08 drops
# an override Cycles 5.2's delegate still declares.
ARG USD=v26.03
ARG OSD=v3_7_0
ARG CYCLES=v5.2.0
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates pkg-config \
      python3 python3-dev libtbb-dev zlib1g-dev libgl-dev libx11-dev \
      libopenimageio-dev libopenexr-dev libimath-dev libpugixml-dev libjpeg-dev libpng-dev libtiff-dev \
 && rm -rf /var/lib/apt/lists/*
# OpenSubdiv for USD imaging, with osdGPU, its GL library, which OpenUSD's GL
# imaging links (nothing opens a GL context)
RUN git clone --depth 1 --branch ${OSD} https://github.com/PixarAnimationStudios/OpenSubdiv.git /src/osd \
 && cmake -S /src/osd -B /build/osd -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/usd \
      -DNO_EXAMPLES=ON -DNO_TUTORIALS=ON -DNO_REGRESSION=ON -DNO_DOC=ON -DNO_OMP=ON -DNO_CUDA=ON \
      -DNO_OPENCL=ON -DNO_GLFW=ON -DNO_PTEX=ON -DNO_TBB=OFF -DNO_OPENGL=OFF -DNO_METAL=ON -DNO_DX=ON \
 && cmake --build /build/osd -j"${JOBS:-$(nproc)}" --target install
# OpenUSD with imaging, usdImaging, Python and the usdrecord tool
RUN git clone --depth 1 --branch ${USD} https://github.com/PixarAnimationStudios/OpenUSD.git /src/usd \
 && cmake -S /src/usd -B /build/usd -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/usd \
      -DCMAKE_PREFIX_PATH=/opt/usd -DPXR_ENABLE_PYTHON_SUPPORT=ON \
      -DPXR_BUILD_IMAGING=ON -DPXR_BUILD_USD_IMAGING=ON -DPXR_ENABLE_GL_SUPPORT=ON \
      -DPXR_BUILD_USDVIEW=OFF -DPXR_BUILD_EMBREE_PLUGIN=OFF -DPXR_BUILD_PRMAN_PLUGIN=OFF \
      -DPXR_BUILD_OPENIMAGEIO_PLUGIN=OFF -DPXR_BUILD_OPENCOLORIO_PLUGIN=OFF -DPXR_ENABLE_MATERIALX_SUPPORT=OFF \
      -DPXR_ENABLE_VULKAN_SUPPORT=OFF -DPXR_BUILD_EXAMPLES=OFF -DPXR_BUILD_TUTORIALS=OFF -DPXR_BUILD_TESTS=OFF \
      -DPXR_BUILD_DOCUMENTATION=OFF \
 && cmake --build /build/usd -j"${JOBS:-$(nproc)}" --target install
# Cycles' Hydra render delegate, against that OpenUSD
# Debian's OpenImageIO CMake config names its command-line tools, which
# live in openimageio-tools; Cycles' find_package fails without them, and
# Cycles' Hydra delegate needs Epoxy.
RUN apt-get update && apt-get install -y --no-install-recommends openimageio-tools libepoxy-dev && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /usr/include/opencv4
# Debian's OIIO target lists /usr/include/opencv4 (it was built with
# OpenCV; Cycles includes nothing from it).
# Cycles 5.2 needs OpenColorIO 2.2 or later; Debian 13 has 2.1. The core
# library only, its small dependencies fetched by its own build.
ARG OCIO=v2.4.2
RUN git clone --depth 1 --branch ${OCIO} https://github.com/AcademySoftwareFoundation/OpenColorIO.git /src/ocio \
 && cmake -S /src/ocio -B /build/ocio -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/ocio \
      -DOCIO_BUILD_APPS=OFF -DOCIO_BUILD_PYTHON=OFF -DOCIO_BUILD_TESTS=OFF -DOCIO_BUILD_GPU_TESTS=OFF \
      -DOCIO_BUILD_DOCS=OFF -DOCIO_BUILD_OPENFX=OFF -DOCIO_INSTALL_EXT_PACKAGES=MISSING \
 && cmake --build /build/ocio -j"${JOBS:-$(nproc)}" --target install
# On arm64 Cycles' CPU kernels need sse2neon (one header, not in Debian 13);
# without it half.h uses NEON intrinsics that nothing includes.
ADD https://raw.githubusercontent.com/DLTcollab/sse2neon/v1.8.0/sse2neon.h /opt/sse2neon/sse2neon.h
# The Cycles repository builds its own app too, which reads USD through
# OpenUSD's hdsi without linking it; the linker flag adds it.
RUN git clone --depth 1 --branch ${CYCLES} https://projects.blender.org/blender/cycles.git /src/cycles \
 && cmake -S /src/cycles -B /build/cycles -DOpenColorIO_DIR=/opt/ocio/lib/cmake/OpenColorIO -DSSE2NEON_INCLUDE_DIR=/opt/sse2neon -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/cycles \
      -DWITH_LIBS_PRECOMPILED=OFF -DPXR_ROOT=/opt/usd -DCMAKE_PREFIX_PATH=/opt/usd \
      -DWITH_CYCLES_HYDRA_RENDER_DELEGATE=ON -DWITH_CYCLES_STANDALONE=OFF -DWITH_CYCLES_STANDALONE_GUI=OFF \
      -DWITH_CYCLES_USD=ON -DWITH_CYCLES_EMBREE=OFF -DWITH_CYCLES_OSL=OFF -DWITH_CYCLES_OPENIMAGEDENOISE=OFF \
      -DWITH_CYCLES_OPENVDB=OFF -DWITH_CYCLES_NANOVDB=OFF -DWITH_CYCLES_ALEMBIC=OFF \
      -DWITH_CYCLES_DEVICE_CUDA=OFF -DWITH_CYCLES_DEVICE_OPTIX=OFF -DWITH_CYCLES_DEVICE_HIP=OFF \
      -DWITH_CYCLES_DEVICE_ONEAPI=OFF -DWITH_CYCLES_DEVICE_METAL=OFF \
      -DCMAKE_EXE_LINKER_FLAGS="-Wl,--no-as-needed -L/opt/usd/lib -lusd_hdsi" \
 && cmake --build /build/cycles -j"${JOBS:-$(nproc)}" --target install \
 && find /opt/cycles -name plugInfo.json -print

FROM ${DEBIAN}
# JOBS caps the compile's parallel jobs (default: every CPU)
ARG JOBS
ARG OIIO=3.2.1.3
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv ca-certificates \
      libtbb12 zlib1g libgl1 libx11-6 libepoxy0 libopenimageio2.5 libopenexr-3-1-30 libpugixml1v5 libpython3.13 \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "OpenImageIO==${OIIO}" \
 && useradd --uid 65532 --user-group --create-home --home-dir /home/c4 --shell /usr/sbin/nologin c4
COPY --from=build /opt/usd /opt/usd
COPY --from=build /opt/cycles /opt/cycles
COPY --from=build /opt/ocio /opt/ocio
COPY --from=unit /out/c4-unit /usr/local/bin/c4-unit
COPY cmd/c4-unit/c4unit.py /opt/c4/python/c4unit.py
# usdrecord comes after the venv on PATH, so `python3` in its shebang is
# the system interpreter OpenUSD was built for.
ENV PATH=/opt/usd/bin:/usr/bin:/opt/venv/bin:/usr/local/bin:/bin \
    PYTHONPATH=/opt/usd/lib/python:/opt/c4/python LD_LIBRARY_PATH=/opt/usd/lib:/opt/cycles/lib:/opt/ocio/lib \
    PXR_PLUGINPATH_NAME=/opt/cycles/hydra \
    HOME=/home/c4 XDG_CACHE_HOME=/home/c4/.cache
LABEL org.opencontainers.image.title=c4-job-hydra \
      org.opencontainers.image.description="C4 job image: USD to OpenEXR through Cycles' Hydra delegate with usdrecord, headless, and c4-unit" \
      org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
      org.opencontainers.image.licenses=Apache-2.0
USER 65532:65532
WORKDIR /home/c4
