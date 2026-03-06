# Image URL to use all building/pushing image targets
IMG ?= vpsieinc/vpsie-cluster-scaler:dev
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.32.0

# Go settings
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
GOBIN := $(shell go env GOBIN)
ifeq (,$(GOBIN))
GOBIN := $(shell go env GOPATH)/bin
endif

# Tools
CONTROLLER_GEN ?= $(GOBIN)/controller-gen
KUSTOMIZE ?= $(GOBIN)/kustomize
ENVTEST ?= $(GOBIN)/setup-envtest

# CRD options
CRD_OPTIONS ?= crd:generateEmbeddedObjectMeta=true,allowDangerousTypes=true

.PHONY: all
all: generate manifests build

##@ Development

.PHONY: generate
generate: controller-gen ## Generate deepcopy, conversion, etc.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests, RBAC, webhook configs.
	go mod tidy
	$(CONTROLLER_GEN) $(CRD_OPTIONS) rbac:roleName=manager-role paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run ./...

.PHONY: test
test: generate manifests envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/bin -p path)" \
		go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: generate fmt vet ## Build manager binary.
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="-s -w" -o bin/manager ./cmd/main.go

.PHONY: run
run: generate manifests ## Run the controller from your host (for development).
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push $(IMG)

.PHONY: docker-buildx
docker-buildx: ## Build and push multi-arch docker image.
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMG) --push .

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found -f -

##@ Tools

.PHONY: controller-gen
controller-gen: ## Download controller-gen if necessary.
	@test -f $(CONTROLLER_GEN) || go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.0

.PHONY: kustomize
kustomize: ## Download kustomize if necessary.
	@test -f $(KUSTOMIZE) || go install sigs.k8s.io/kustomize/kustomize/v5@v5.4.2

.PHONY: envtest
envtest: ## Download setup-envtest if necessary.
	@test -f $(ENVTEST) || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

##@ Helpers

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
