# Common dev tasks. Cross-platform: macOS uses Docker for anything that needs
# the Linux runtime (rootless userns + pivot_root); Linux runs them natively.
#
# Cheat sheet:
#   make help              list every target with a short description
#   make build             cross-compile a Linux static binary into ./bin/
#   make test              cross-platform unit tests (dag, image, ...)
#   make vet               gofmt check + go vet
#   make verify-rootless   end-to-end rootless E2E (Docker on macOS)
#   make verify-btrfs      end-to-end privileged btrfs E2E (Docker, --privileged)
#   make dev-shell         drop into a persistent dev container (fast inner loop)
#   make clean             remove built artifacts

GOOS_LINUX ?= linux
GOARCH     ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/;s/arm64/arm64/')
GOPROXY    ?= $(shell go env GOPROXY)

.PHONY: help build test vet verify-rootless verify-btrfs verify-supervise verify-mcp verify-rollback dev-shell dev-shell-stop clean

help:
	@awk 'BEGIN{FS=":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## cross-compile a Linux static binary (./bin/agentenv-<arch>)
	@mkdir -p bin
	GOOS=$(GOOS_LINUX) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o bin/agentenv-$(GOARCH) .
	@echo "built: bin/agentenv-$(GOARCH)"

test: ## cross-platform unit tests
	go test -race ./...

vet: ## gofmt + go vet
	@if [ -n "$$(gofmt -l .)" ]; then echo "gofmt needs:"; gofmt -l .; exit 1; fi
	go vet ./...

verify-rootless: ## end-to-end rootless E2E (Docker, uid 1001, no privileged)
	docker run --rm --user 1001:1001 --security-opt seccomp=unconfined \
	  -v "$$PWD":/src:ro -v "$$HOME/go/pkg/mod":/go/pkg/mod:ro \
	  -e GOFLAGS=-mod=mod -e GOPROXY="$(GOPROXY)" \
	  -e CGO_ENABLED=0 -e GOCACHE=/tmp/gocache -e HOME=/tmp \
	  -w /src golang:1.26 bash /src/scripts/verify-rootless.sh

verify-btrfs: ## end-to-end privileged btrfs E2E (Docker, --privileged)
	docker run --rm --privileged \
	  -v "$$PWD":/src -v "$$HOME/go/pkg/mod":/go/pkg/mod \
	  -e GOFLAGS=-mod=mod -e GOPROXY="$(GOPROXY)" -e CGO_ENABLED=1 \
	  -w /src golang:1.26 bash /src/scripts/verify.sh

verify-supervise: ## end-to-end supervise + ctl rollback
	docker run --rm --security-opt seccomp=unconfined \
	  -v "$$PWD":/src:ro -v "$$HOME/go/pkg/mod":/go/pkg/mod \
	  -e GOFLAGS=-mod=mod -e GOPROXY="$(GOPROXY)" \
	  -e CGO_ENABLED=0 -e GOCACHE=/tmp/gocache \
	  -w /src golang:1.26 bash /src/scripts/verify-supervise.sh

verify-mcp: ## MCP protocol smoke (JSON-RPC against `agentenv mcp`)
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o verify/docker/agentenv-linux-$(GOARCH) .
	docker build --platform=linux/$(GOARCH) -f verify/docker/Dockerfile.mcp-smoke \
	  -t agentenv-mcp-smoke verify/docker/
	docker run --rm --platform=linux/$(GOARCH) agentenv-mcp-smoke

verify-rollback: ## MCP-driven end-to-end rollback (asserts files actually revert)
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o verify/docker/agentenv-linux-$(GOARCH) .
	docker build --platform=linux/$(GOARCH) -f verify/docker/Dockerfile.rollback-smoke \
	  -t agentenv-rollback-smoke verify/docker/
	docker run --rm --platform=linux/$(GOARCH) \
	  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
	  agentenv-rollback-smoke

# --- Persistent dev container: fast inner loop on macOS -----------------------
# `make dev-shell` starts (or reattaches to) a long-running container with the
# source bind-mounted; rebuilds and verifies inside are seconds (no setup tax).
DEV_NAME ?= agentenv-dev
dev-shell: ## drop into a persistent Linux dev container (auto-creates)
	@docker inspect $(DEV_NAME) >/dev/null 2>&1 || \
	  docker run -d --name $(DEV_NAME) --user 1001:1001 \
	    --security-opt seccomp=unconfined \
	    -v "$$PWD":/src -v "$$HOME/go/pkg/mod":/go/pkg/mod \
	    -e GOPROXY="$(GOPROXY)" -e CGO_ENABLED=0 \
	    -e GOCACHE=/tmp/gocache -e HOME=/tmp \
	    -w /src golang:1.26 sleep infinity
	docker exec -it $(DEV_NAME) bash

dev-shell-stop: ## stop and remove the dev container
	-docker rm -f $(DEV_NAME)

clean: ## remove built artifacts
	rm -rf bin/ /tmp/agentenv /tmp/agentenv-* verify/docker/agentenv-linux-*
