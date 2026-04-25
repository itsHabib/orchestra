BINARY := orchestra
GOBIN  ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
  GOBIN = $(shell go env GOPATH)/bin
endif

.PHONY: build install uninstall test test-race vet lint check clean \
        e2e-ma e2e-ma-single e2e-ma-multi

build:
	go build -o $(BINARY) .

install: build
	cp $(BINARY) $(GOBIN)/$(BINARY)
	@echo "Installed $(BINARY) to $(GOBIN)/$(BINARY)"

uninstall:
	rm -f $(GOBIN)/$(BINARY)
	@echo "Removed $(BINARY) from $(GOBIN)"

test:
	go test ./...

# test-race runs the unit + integration suite with the race detector.
# Requires CGO (gcc / clang). CI runs this on Linux; locally on Windows you
# usually need a MinGW toolchain or to defer to CI.
test-race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

lint:
	go vet ./...
	go tool golangci-lint run ./...

check: lint test build

# --- E2E targets (live Managed Agents) ---------------------------------------
# These hit real Anthropic infrastructure and spend real tokens. They are
# opt-in and are NOT part of `make test` or CI. Each requires an API key
# with managed-agents access. See TESTING.md for cost estimates and the
# expected post-run state.json checks.

e2e-ma: e2e-ma-single e2e-ma-multi

e2e-ma-single: build
	@if [ -z "$$ANTHROPIC_API_KEY" ]; then \
		echo "ANTHROPIC_API_KEY required for e2e-ma-single (live Managed Agents)"; \
		exit 1; \
	fi
	ORCHESTRA_MA_INTEGRATION=1 ./$(BINARY) run test/integration/ma_single_team/orchestra.yaml

e2e-ma-multi: build
	@if [ -z "$$ANTHROPIC_API_KEY" ]; then \
		echo "ANTHROPIC_API_KEY required for e2e-ma-multi (live Managed Agents)"; \
		exit 1; \
	fi
	ORCHESTRA_MA_INTEGRATION=1 ./$(BINARY) run test/integration/ma_multi_team/orchestra.yaml

clean:
	rm -f $(BINARY)
