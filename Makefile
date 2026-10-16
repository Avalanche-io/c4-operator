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
LINUX_TEST_PKGS ?= ./internal/capture ./internal/layer ./internal/overlay ./internal/node

.PHONY: all generate manifests test test-linux envtest build image e2e e2e-down tools

all: generate manifests test build

tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	go install sigs.k8s.io/kind@latest

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
	  GOOS=linux GOARCH=$(ARCH) go test -c -o bin/linux-tests/$$n.test $$p || exit 1; \
	done
	docker volume create c4-linux-tests >/dev/null
	@for t in bin/linux-tests/*.test; do \
	  echo "== $$t"; \
	  docker run --rm --privileged -v $(CURDIR)/bin/linux-tests:/t:ro -v c4-linux-tests:/scratch \
	    -e TMPDIR=/scratch -e C4_PRIVILEGED_TESTS=1 alpine:latest /t/$$(basename $$t) -test.count=1 || exit 1; \
	done

build:
	CGO_ENABLED=0 go build -trimpath -o bin/c4-operator ./cmd/c4-operator

# The image is assembled from a binary cross-compiled on the host: Go builds
# for any architecture without emulation, and the image needs no toolchain.
image:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o bin/linux_$(ARCH)/c4-operator ./cmd/c4-operator
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMG) .

e2e:
	hack/e2e/run.sh

e2e-down:
	$(KIND) delete cluster --name c4-operator-e2e
