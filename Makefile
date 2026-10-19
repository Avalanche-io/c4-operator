GOBIN ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif
CONTROLLER_GEN ?= $(GOBIN)/controller-gen
SETUP_ENVTEST ?= $(GOBIN)/setup-envtest
KIND ?= $(GOBIN)/kind
ENVTEST_K8S_VERSION ?= 1.37.0
IMG ?= c4-operator:dev
ARCH ?= $(shell go env GOARCH)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REGISTRY ?= ghcr.io/avalanche-io
RELEASE_IMG ?= $(REGISTRY)/c4-operator:$(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64
LDFLAGS := -s -w -X main.version=$(VERSION)
comma := ,
# The prebuilt job images (images/<name>/Dockerfile), named c4-job-<name>.
JOB_IMAGES ?= oiio media usd ci train infer sim hydra
LINUX_TEST_PKGS ?= ./internal/capture ./internal/layer ./internal/overlay ./internal/node

.PHONY: all generate manifests test test-linux envtest build image images image-sync image-block sync-yaml demo-dailies demo-github job-images job-image-hydra-blender install-yaml release-snapshot verify e2e e2e-down tools tutorials stress stress-quick

all: generate manifests test build

tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
	go install sigs.k8s.io/kind@v0.33.0

# What CI checks besides the tests: formatting, vet, generated code and
# manifests in step with the API, and the manifests render.
verify:
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || { gofmt -l $$(git ls-files '*.go'); exit 1; }
	go vet ./...
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd:allowDangerousTypes=false paths=./api/... output:crd:artifacts:config=config/crd
	git diff --exit-code -- api config/crd
	kubectl kustomize config/default >/dev/null
	kubectl kustomize config/release >/dev/null

generate:
	$(CONTROLLER_GEN) object paths=./api/...


manifests:
	$(CONTROLLER_GEN) crd:allowDangerousTypes=false paths=./api/... output:crd:artifacts:config=config/crd

envtest:
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/k8s -p path

test:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/k8s -p path)" go test ./... -count=1

# Overlay mounts, whiteouts and capture need root on Linux. The test
# binaries are cross-compiled here and run in a privileged container on a
# Docker volume (ext4), never on the host.
test-linux:
	@mkdir -p bin/linux-tests
	@for p in $(LINUX_TEST_PKGS); do \
	  [ -d $$p ] || continue; \
	  n=$$(basename $$p); \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go test -c -o bin/linux-tests/$$n.test $$p || exit 1; \
	done
	docker volume create c4-linux-tests >/dev/null
	@for t in bin/linux-tests/*.test; do \
	  echo "== $$t"; \
	  docker run --rm --privileged -v $(CURDIR)/bin/linux-tests:/t:ro -v c4-linux-tests:/scratch \
	    -e TMPDIR=/scratch -e C4_PRIVILEGED_TESTS=1 alpine:latest /t/$$(basename $$t) -test.count=1 || exit 1; \
	done

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/c4-operator ./cmd/c4-operator
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/c4-farm ./cmd/c4-farm
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/c4-agent ./cmd/c4-agent
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/c4-sync ./cmd/c4-sync

# The image is assembled from a binary cross-compiled on the host: Go builds
# for any architecture without emulation, and the image needs no toolchain.
image:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="$(LDFLAGS)" -o bin/linux_$(ARCH)/c4-operator ./cmd/c4-operator
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMG) .

# c4-sync's image for this machine's architecture (experimental; its own
# image, Dockerfile.c4-sync). SYNC_IMG names it.
SYNC_IMG ?= c4-sync:dev
image-sync:
	@mkdir -p bin/sync-context/linux/$(ARCH)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="$(LDFLAGS)" -o bin/sync-context/linux/$(ARCH)/c4-sync ./cmd/c4-sync
	docker build -f Dockerfile.c4-sync --build-arg TARGETPLATFORM=linux/$(ARCH) -t $(SYNC_IMG) bin/sync-context

# A block's image for this machine's architecture (images/block): Claude
# Code, bash and git, with linux c4-agent and c4-unit. BLOCK_IMG names it.
BLOCK_IMG ?= c4-block:dev
image-block:
	@mkdir -p bin/block-context
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="$(LDFLAGS)" -o bin/block-context/c4-agent ./cmd/c4-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="$(LDFLAGS)" -o bin/block-context/c4-unit ./cmd/c4-unit
	docker build -f images/block/Dockerfile -t $(BLOCK_IMG) bin/block-context

# c4-sync's install manifest of a release: its two CRDs and config/sync,
# with the release's c4-sync image.
SYNC_RELEASE_IMG ?= $(REGISTRY)/c4-sync:$(VERSION)
sync-yaml:
	@mkdir -p bin
	{ cat config/crd/c4.avalanche.io_trackers.yaml; echo ---; cat config/crd/c4.avalanche.io_trackerlinks.yaml; echo ---; \
	  kubectl kustomize config/sync | sed 's#image: c4-sync:dev#image: $(SYNC_RELEASE_IMG)#'; } > bin/c4-sync.yaml
	@grep -q 'image: $(SYNC_RELEASE_IMG)' bin/c4-sync.yaml || { echo "sync-yaml: the image was not set"; exit 1; }
	@echo bin/c4-sync.yaml uses $(SYNC_RELEASE_IMG)

# The release image for every platform in PLATFORMS, in one buildx build.
# The Dockerfile only copies the binary, so no platform is emulated.
# Without PUSH=1 the image is written to bin/ as an OCI archive and nothing
# leaves this machine; with PUSH=1 it goes to RELEASE_IMG (log in first).
images:
	@for p in $$(echo $(PLATFORMS) | tr , ' '); do \
	  a=$${p#linux/}; \
	  echo "build linux/$$a"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -trimpath -ldflags="$(LDFLAGS)" -o bin/linux_$$a/c4-operator ./cmd/c4-operator || exit 1; \
	done
	docker buildx build --platform $(PLATFORMS) -t $(RELEASE_IMG) \
	  --label org.opencontainers.image.source=https://github.com/Avalanche-io/c4-operator \
	  --label org.opencontainers.image.version=$(VERSION) \
	  $(if $(PUSH),--push,--output type=oci$(comma)dest=bin/c4-operator-$(VERSION).oci.tar) .

# The prebuilt job images, for every platform in PLATFORMS, from the
# repository root (each copies cmd/c4-unit). Without PUSH=1 each is written
# to bin/ as an OCI archive; with PUSH=1 it goes to
# $(REGISTRY)/c4-job-<name>:$(VERSION). JOB_IMAGES=oiio builds one; JOBS=2 caps
# the compiles (usd, hydra) at two parallel jobs.
job-images:
	@mkdir -p bin
	@for i in $(JOB_IMAGES); do \
	  echo "build c4-job-$$i ($(PLATFORMS))"; \
	  docker buildx build --platform $(PLATFORMS) -f images/$$i/Dockerfile $(if $(JOBS),--build-arg JOBS=$(JOBS)) \
	    -t $(REGISTRY)/c4-job-$$i:$(VERSION) \
	    --label org.opencontainers.image.version=$(VERSION) \
	    --label org.opencontainers.image.revision=$$(git rev-parse HEAD) \
	    $(if $(PUSH),--push,--output type=oci$(comma)dest=bin/c4-job-$$i-$(VERSION).oci.tar) . || exit 1; \
	done

# The fallback hydra image (Blender's Cycles reading USD), amd64 only.
job-image-hydra-blender:
	@mkdir -p bin
	docker buildx build --platform linux/amd64 -f images/hydra/Dockerfile.blender \
	  -t $(REGISTRY)/c4-job-hydra:$(VERSION)-blender \
	  --label org.opencontainers.image.version=$(VERSION) \
	  $(if $(PUSH),--push,--output type=oci$(comma)dest=bin/c4-job-hydra-blender-$(VERSION).oci.tar) .

# The install manifest of a release: config/release with RELEASE_IMG.
install-yaml:
	@mkdir -p bin
	kubectl kustomize config/release | sed 's#ghcr.io/avalanche-io/c4-operator:v0.1.0#$(RELEASE_IMG)#' > bin/install.yaml
	@echo bin/install.yaml uses $(RELEASE_IMG)

# A local dry run of the release: binaries, archives, checksums and the
# images, into dist/, published nowhere.
release-snapshot:
	goreleaser release --snapshot --clean

e2e:
	hack/e2e/run.sh

# The Flow dailies demo on the running e2e cluster: a fake Flow site,
# c4-sync and a render, with HUD captures in hack/demo/out/dailies; it
# tears down what it made (KEEP=1 keeps it; hack/demo/dailies.sh down).
demo-dailies:
	hack/demo/dailies.sh

demo-github:
	hack/demo/github.sh

e2e-down:
	$(KIND) delete cluster --name c4-operator-e2e

# The scheduler stress grid (hack/stress): a seeded sample of farms and
# workloads run through sched.Decide and the baselines, with invariant
# and metamorphic checks on every decision. Writes hack/stress/out/ and
# fails if an invariant fails for a cause not yet known. stress-quick is
# the CI grid (small farms, short windows); TestQuickGrid runs the same
# grid as a Go test.
STRESS_WORKERS ?= 4

stress-quick:
	go run ./hack/stress -quick -workers $(STRESS_WORKERS)

stress:
	go run ./hack/stress -workers $(STRESS_WORKERS)
