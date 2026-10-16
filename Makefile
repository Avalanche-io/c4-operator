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

.PHONY: all generate manifests test envtest build image kind-load e2e e2e-down tools

all: generate manifests test build

tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	go install sigs.k8s.io/kind@latest

generate:
	$(CONTROLLER_GEN) object paths=./api/...

manifests:
	$(CONTROLLER_GEN) crd:allowDangerousTypes=false paths=./api/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=c4-operator-manager paths=./internal/controller/... paths=./internal/webhook/... output:rbac:artifacts:config=config/rbac/manager
	$(CONTROLLER_GEN) rbac:roleName=c4-operator-agent paths=./internal/agent/... output:rbac:artifacts:config=config/rbac/agent

envtest:
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/k8s -p path

test:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/k8s -p path)" go test ./... -count=1

build:
	CGO_ENABLED=0 go build -trimpath -o bin/c4-operator ./cmd/c4-operator

# The image is assembled from a binary built on the host because go.mod
# still points at ../c4 with a replace directive during development.
image:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags="-s -w" -o bin/linux_$(ARCH)/c4-operator ./cmd/c4-operator
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMG) .

e2e:
	hack/e2e/run.sh

e2e-down:
	$(KIND) delete cluster --name c4-operator-e2e
