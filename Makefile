# Treat the whole recipe as a one shell script/invocation instead of one-per-line
.ONESHELL:
# Use bash instead of plain sh
SHELL := bash
.SHELLFLAGS := -o pipefail -euc

VERSION ?=
ifeq ($(VERSION),)
	VERSION := $(shell go run --buildvcs=true ./script/version/)
endif
CGO_ENABLED := 0
export CGO_ENABLED

GITHUB_REPOSITORY_OWNER ?= $(shell git remote get-url origin 2>/dev/null | sed -E 's|.*[:/]([^/]+)/[^/]+\.git.*|\1|;s|.*[:/]([^/]+)/[^/]+$$|\1|')
HELM_REPO_NAME ?= ghcr.io/${GITHUB_REPOSITORY_OWNER}/charts

########### Tools variables ###########
# Tool versions
GOLANGCI_LINT_VERSION  := v2.11.2

GORELEASER_VER := v2.12.2
GORELEASER_BIN := goreleaser

# Directories.
TOOLS_BIN_DIR := $(abspath bin)

# Tool binaries with versions
GODEPGRAPH := go tool godepgraph
GORELEASER := $(TOOLS_BIN_DIR)/$(GORELEASER_BIN)-$(GORELEASER_VER)
GOLANGCI_LINT := $(TOOLS_BIN_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)
#######################################

all: help

help: ## Print this help
	@grep --no-filename -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sed 's/:.*##/·/' | sort | column -ts '·' -c 120

.PHONY: build
build: generate $(GORELEASER) ## Build the Tinkerbell and Tink Agent binaries
	$(GORELEASER) build --clean --auto-snapshot

TEST_PKG ?=
TEST_PKGS :=
ifeq ($(TEST_PKG),)
	TEST_PKGS := ./...
else
	TEST_PKGS := ./$(TEST_PKG)/...
endif

.PHONY: test
test: ## Run go test
	CGO_ENABLED=1 go test -race -coverprofile=coverage.txt -covermode=atomic -v ${TEST_ARGS} ${TEST_PKGS}

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Run go fmt
	$(GOLANGCI_LINT) fmt ./...

FILE_TO_NOT_INCLUDE_IN_COVERAGE := script/version/main.go|*.pb.go|zz_generated.deepcopy.go|facility_string.go|severity_string.go|*_templ.go

.PHONY: coverage
coverage: test ## Show test coverage
## Filter out generated files
	cat coverage.txt | grep -v -E '$(FILE_TO_NOT_INCLUDE_IN_COVERAGE)' > coverage.out
	go tool cover -func=coverage.out
	mv coverage.out coverage.txt

.PHONY: ci
ci: coverage lint vet ## Runs all the same validations and tests that run in CI

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	go generate ./...
	$(MAKE) format

.PHONY: format
format: $(GOLANGCI_LINT) ## Format the code using go fmt
	$(GOLANGCI_LINT) fmt ./...

.PHONY: dep-graph
dep-graph: ## Generate a dependency graph
	rm -rf out/dep-graph.txt out/dep-graph.png
	go tool godepgraph -s -novendor -onlyprefixes "github.com/tinkerbell/tinkerbell,./cmd/agent,./cmd/tinkerbell" ./cmd/agent ./cmd/tinkerbell > out/dep-graph.txt
	cat out/dep-graph.txt | dot -Txdot -o out/dep-graph.dot

######### Helm charts - start #########
helm-files := $(shell git ls-files helm/tinkerbell/ | grep -v helm/tinkerbell/docs)
helm-package: out/helm/tinkerbell-$(VERSION).tgz ## Helm chart for Tinkerbell
out/helm/tinkerbell-$(VERSION).tgz: $(helm-files)
	helm package -d out/helm/ helm/tinkerbell --version $(VERSION) --app-version $(VERSION)

.PHONY: helm-publish
helm-publish: out/helm/tinkerbell-$(VERSION).tgz ## Publish the Helm chart
	helm push out/helm/tinkerbell-$(VERSION).tgz oci://$(HELM_REPO_NAME)

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint helm/tinkerbell --set "trustedProxies={127.0.0.1/24}" --set "publicIP=1.1.1.1"

.PHONY: helm-template
helm-template: ## Helm template for Tinkerbell
	helm template test helm/tinkerbell --set "trustedProxies={127.0.0.1/24}" --set "publicIP=1.1.1.1" 2>&1 >/dev/null

######### Helm charts - end   #########

######### Build container images - start #########
.PHONY: build-image
build-image: $(GORELEASER) ## Build the container images
	$(GORELEASER) release --clean --skip=validate --skip=sign --auto-snapshot --verbose

.PHONY: build-image-push
build-image-push: $(GORELEASER) ## Build and push the container images
	$(GORELEASER) release --clean --skip=validate --skip=sign ${GORELEASER_EXTRA_FLAGS}

######### Build container images - end   #########

.PHONY: clean
clean: ## Remove all generated binaries
	rm -rf dist out

.PHONY: clean-tools
clean-tools: ## Remove all tools
	rm -rf $(TOOLS_BIN_DIR)

.PHONY: clean-all
clean-all: clean clean-tools ## Remove all binaries and tools

############## Tools ##############

$(GORELEASER):
	mkdir -p $(TOOLS_BIN_DIR)
	GOBIN=$(TOOLS_BIN_DIR) go install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VER)
	@mv $(TOOLS_BIN_DIR)/goreleaser $(GORELEASER)

$(GOLANGCI_LINT):
	mkdir -p $(TOOLS_BIN_DIR)
	GOBIN=$(TOOLS_BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS_BIN_DIR)/golangci-lint $(GOLANGCI_LINT)

.PHONY: tools
tools: $(GORELEASER) $(GOLANGCI_LINT) ## Install all tools

############## Linting ##############
.PHONY: lint
lint: _lint  ## Run linting

LINTERS :=
FIXERS :=

GOLANGCI_LINT_CONFIG := .golangci.yml
LINTERS += golangci-lint-lint
golangci-lint-lint: $(GOLANGCI_LINT)
	find $(PWD) -name go.mod -not -path "./out/*" -execdir sh -c '"$(GOLANGCI_LINT)" run --timeout 10m -c "$(GOLANGCI_LINT_CONFIG)"' '{}' '+'

FIXERS += golangci-lint-fix
golangci-lint-fix: $(GOLANGCI_LINT)
	find $(PWD) -name go.mod -not -path "./out/*" -execdir "$(GOLANGCI_LINT)" run -c "$(GOLANGCI_LINT_CONFIG)" --fix \;

.PHONY: _lint $(LINTERS)
_lint: $(LINTERS)

.PHONY: fix $(FIXERS)
fix: $(FIXERS)
