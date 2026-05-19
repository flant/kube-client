
.PHONY: go-module-version
go-module-version: go-check git-check
	@echo "go get $(shell $(GO) list ./client)@$(shell $(GIT) rev-parse HEAD)"


.PHONY: lint
lint: golangci-lint ## Run linter.
	@$(GOLANGCI_LINT) run --fix

.PHONY: test
test: go-check
	@$(GO) test --race --cover ./...

## Run all generate-* jobs in bulk.
.PHONY: generate
generate: update-workflows-go-version update-workflows-golangci-lint-version


##@ Dependencies

WHOAMI ?= $(shell whoami)

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
GO=$(shell which go)
GIT=$(shell which git)

.PHONY: go-check
go-check:
	$(call error-if-empty,$(GO),go)

.PHONY: git-check
git-check:
	$(call error-if-empty,$(GIT),git)

## Tool installations

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: yq
yq: $(YQ) ## Download yq locally if necessary.
$(YQ): $(LOCALBIN)
	$(call go-install-tool,$(YQ),github.com/mikefarah/yq/v4,$(YQ_VERSION))


# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) GOTOOLCHAIN=$(GO_TOOLCHAIN_AUTOINSTALL_VERSION) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef


define error-if-empty
@if [[ -z $(1) ]]; then echo "$(2) not installed"; false; fi
endef