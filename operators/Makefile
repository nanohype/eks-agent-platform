SHELL := /bin/bash

VERSION ?= dev
IMG ?= ghcr.io/nanohype/eks-agent-platform/operator:$(VERSION)

CONTROLLER_GEN ?= go tool controller-gen
ENVTEST ?= go tool setup-envtest
CRD_REF_DOCS ?= go tool crd-ref-docs
ENVTEST_K8S_VERSION ?= 1.33

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-25s\033[0m %s\n", $$1, $$2}'

.PHONY: generate
generate: ## Run controller-gen for deepcopy + RBAC + CRDs
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." \
	  output:crd:artifacts:config=config/crd/bases \
	  output:rbac:artifacts:config=config/rbac

.PHONY: manifests
manifests: generate crd-docs ## Copy generated CRDs into the Helm chart + regen markdown reference
	mkdir -p ../charts/operator/crds
	cp config/crd/bases/*.yaml ../charts/operator/crds/

.PHONY: crd-docs
crd-docs: generate ## Render docs/crd-reference/v1alpha1.md from the Go types
	mkdir -p ../docs/crd-reference
	$(CRD_REF_DOCS) \
	  --source-path=./api \
	  --config=hack/crd-ref-docs.yaml \
	  --renderer=markdown \
	  --output-path=../docs/crd-reference/v1alpha1.md

.PHONY: fmt
fmt: ## go fmt
	go fmt ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: test
test: generate fmt vet ## Unit + conformance tests with envtest (merged coverage profile)
	@KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test ./... -coverpkg=./internal/... -coverprofile=cover.out
	@go tool cover -func=cover.out | tail -1

.PHONY: coverage-check
coverage-check: test ## Enforce per-package + security-critical coverage floors
	go run ./hack/coverage-check cover.out

.PHONY: test-conformance
test-conformance: generate ## Just the conformance suite (no race detector, fast feedback)
	@KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./test/conformance/... -count=1 -v

.PHONY: build
build: generate fmt vet ## Build binary
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=$(VERSION)" \
	  -o bin/manager ./cmd

.PHONY: build-agentctl
build-agentctl: generate fmt vet ## Build agentctl CLI
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=$(VERSION)" \
	  -o bin/agentctl ./cmd/agentctl

.PHONY: run
run: generate fmt vet ## Run locally against current kubecontext
	go run ./cmd --leader-elect=false

.PHONY: docker-build
docker-build: ## Build image
	docker build -t $(IMG) --build-arg VERSION=$(VERSION) .

.PHONY: docker-push
docker-push: ## Push image
	docker push $(IMG)

.PHONY: lint
lint: ## golangci-lint
	golangci-lint run ./...

.PHONY: clean
clean:
	rm -rf bin/ cover.out config/crd/bases/*.yaml config/rbac/role.yaml
