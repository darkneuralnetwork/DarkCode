# DarkCode build & verification. `make ci` is what CI runs; run it locally
# before pushing. Everything here uses only the Go toolchain — no extra deps.

GO      ?= go
BINARY  ?= darkcode
PKGS    := ./...

.DEFAULT_GOAL := build

.PHONY: build
build: ## Compile the binary
	$(GO) build -o $(BINARY) .

.PHONY: install
install: ## Install into GOBIN
	$(GO) install .

.PHONY: run
run: ## Build and run
	$(GO) run .

.PHONY: test
test: ## Run the test suite
	$(GO) test $(PKGS)

.PHONY: test-race
test-race: ## Run tests with the race detector
	$(GO) test -race $(PKGS)

.PHONY: cover
cover: ## Run tests and write a coverage profile
	$(GO) test -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt: ## Format all Go files in place
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is not gofmt-clean
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; echo "$$unformatted"; \
		echo "Run 'make fmt'."; exit 1; \
	fi

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are not tidy
	$(GO) mod tidy
	@if ! git diff --quiet -- go.mod go.sum; then \
		echo "go.mod/go.sum not tidy — run 'go mod tidy' and commit."; \
		git --no-pager diff -- go.mod go.sum; exit 1; \
	fi

.PHONY: bench
bench: build ## Run the benchmark suite against the built binary
	$(GO) run ./bench/cmd/benchrun -tasks bench/tasks -agent ./$(BINARY) -json bench-report.json

.PHONY: sbom
sbom: build ## Write the bill of materials read back out of the built binary
	@{ echo "# SBOM for $(BINARY)"; echo "# Verify with: go version -m $(BINARY)"; echo; \
	   $(GO) version -m ./$(BINARY); } > SBOM.txt
	@echo "wrote SBOM.txt"

.PHONY: ci
ci: fmt-check vet build test-race ## The full gate CI enforces

.PHONY: clean
clean: ## Remove build artifacts
	rm -f $(BINARY) coverage.out SBOM.txt bench-report.json

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'
