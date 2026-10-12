# rms — registry mirror service
#
# The Go caches live inside the repository (see scripts/env.sh) because
# /root/go/pkg/mod is mounted read-only in this environment. Always drive the
# build through make, or `source scripts/env.sh` first.

SHELL := /usr/bin/env bash

ROOT    := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
BIN_DIR := $(ROOT)/bin
BIN     := $(BIN_DIR)/rms
IMAGE   ?= rms:dev

# --- Go environment (mirrors scripts/env.sh) ---------------------------------
export GOPATH      := $(ROOT)/.gocache/gopath
export GOMODCACHE  := $(GOPATH)/pkg/mod
export GOCACHE     := $(ROOT)/.gocache/build

# Docker's client/buildx state; /root/.docker is read-only in this sandbox.
export DOCKER_CONFIG ?= $(ROOT)/.docker

# Outbound proxy: required on 192.168.188.101, unused on 192.168.188.103.
PROXY    ?= http://192.168.188.1:3128
export HTTP_PROXY  ?= $(PROXY)
export HTTPS_PROXY ?= $(PROXY)
export NO_PROXY    ?= localhost,127.0.0.1,192.168.188.0/24,10.82.0.0/16,10.44.0.0/16

GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Download and verify module dependencies (not needed when vendored)
	$(GO) mod download
	$(GO) mod verify

.PHONY: tidy
tidy: ## Reconcile go.mod / go.sum
	$(GO) mod tidy

.PHONY: vendor
vendor: ## Refresh vendor/ (keeps the image build hermetic and offline)
	$(GO) mod vendor
	@echo "vendored: $$(find vendor -type f | wc -l) files, $$(du -sh vendor | cut -f1)"

.PHONY: fmt
fmt: ## Format all Go sources
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: build
build: ## Build the rms binary into bin/
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/rms

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run unit tests with a coverage summary
	$(GO) test -coverprofile=$(ROOT)/coverage.out ./...
	$(GO) tool cover -func=$(ROOT)/coverage.out | tail -1

.PHONY: e2e
e2e: ## Run the deterministic end-to-end suite (in-process upstream, no network)
	$(GO) test -tags e2e -count=1 ./test/e2e/...

.PHONY: e2e-docker
e2e-docker: ## Also drive the real docker CLI through rms (needs a docker daemon)
	RMS_E2E_DOCKER=1 $(GO) test -tags e2e -count=1 -run TestDockerClient -v ./test/e2e/...

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: run
run: build ## Run locally against config/rms.yaml (uses ./data, not /data)
	RMS_STORAGE_ROOT=$(ROOT)/data \
	RMS_HTPASSWD=$(ROOT)/config/htpasswd \
	$(BIN) serve --config $(ROOT)/config/rms.yaml

.PHONY: htpasswd
htpasswd: build ## Add/update a user: make htpasswd USER=alice PASS=secret
	@test -n "$(USER)" || { echo "usage: make htpasswd USER=<name> PASS=<password>"; exit 1; }
	$(BIN) htpasswd add --file $(ROOT)/config/htpasswd --user '$(USER)' --password '$(PASS)'
	@# The command writes the file 0600 as its owner on purpose (it holds
	@# password hashes). The container runs as a different uid, so grant group
	@# read here rather than leaving a file the service cannot open.
	@chgrp $(CONTAINER_GID) $(ROOT)/config/htpasswd && chmod 0640 $(ROOT)/config/htpasswd
	@echo "note: run 'docker kill -s HUP rms' (or restart) for a running container to pick this up"

.PHONY: image
image: ## Build the container image with the multi-stage Dockerfile
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .

.PHONY: image-binary
image-binary: build ## Build an image from the host-compiled binary (no Go toolchain image needed)
	@# A dynamically linked binary fails on Alpine/musl with a confusing
	@# "no such file or directory", so assert it is static first.
	@file $(BIN) | grep -q 'statically linked' \
		|| { echo "error: $(BIN) is not statically linked; build with CGO_ENABLED=0"; file $(BIN); exit 1; }
	docker build -f Dockerfile.binary \
		--build-arg RUNTIME_BASE=$(RUNTIME_BASE) \
		-t $(IMAGE) .

# The container runs as uid 65532 (distroless nonroot), so the bind-mounted cache
# directory must belong to it. This is why `make up` depends on `prepare`.
CONTAINER_UID ?= 65532
CONTAINER_GID ?= 65532

# Runtime base for `image-binary`. It must provide CA certificates, otherwise
# every HTTPS upstream fails with "certificate signed by unknown authority".
RUNTIME_BASE ?= gcr.io/distroless/static-debian12:nonroot

.PHONY: prepare
prepare: ## Make the host directories and files usable by the container user
	@mkdir -p $(ROOT)/data $(DOCKER_CONFIG)
	@chown -R $(CONTAINER_UID):$(CONTAINER_GID) $(ROOT)/data
	@echo "prepared $(ROOT)/data for uid:gid $(CONTAINER_UID):$(CONTAINER_GID)"
	@# The container runs unprivileged, so the read-only mounts must be readable
	@# by it. The htpasswd file is group-readable only: it holds password hashes.
	@if [ -f $(ROOT)/config/rms.yaml ]; then chmod 0644 $(ROOT)/config/rms.yaml; fi
	@if [ -f $(ROOT)/config/htpasswd ]; then \
		chgrp $(CONTAINER_GID) $(ROOT)/config/htpasswd && chmod 0640 $(ROOT)/config/htpasswd && \
		echo "prepared $(ROOT)/config/htpasswd as root:$(CONTAINER_GID) 0640"; \
	fi

.PHONY: up
up: prepare ## Start the compose stack
	docker compose up -d --build

.PHONY: down
down: ## Stop the compose stack
	docker compose down

.PHONY: logs
logs: ## Follow compose logs
	docker compose logs -f --tail=100

.PHONY: clean
clean: ## Remove build output and caches
	rm -rf $(BIN_DIR) $(ROOT)/coverage.out $(ROOT)/.gocache

.PHONY: distclean
distclean: clean ## Also remove cached registry data (DESTRUCTIVE)
	@echo "removing $(ROOT)/data ..."
	rm -rf $(ROOT)/data
