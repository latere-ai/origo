# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

GO ?= go

.PHONY: build check clean dev test-e2e

# The whole bar. Every gate lives in latere.ai/x/ci-gate, pinned as a tool in
# go.mod and configured in .lateregate.yaml, so this target is a name for
# `go tool lateregate` and nothing else. One gate at a time:
# `go tool lateregate cover`. The plan: `go tool lateregate list`.
check:
	@$(GO) tool lateregate

.DEFAULT_GOAL := check

OUT_DIR := out
SERVICE := origoweb
MODULE := $(shell $(GO) list -m)

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -X main.Version=$(VERSION) -X main.Commit=$(COMMIT)$(DIRTY) -X main.Date=$(BUILD_DATE)

build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(OUT_DIR)/$(SERVICE) ./cmd/$(SERVICE)
	@echo "built $(OUT_DIR)/$(SERVICE)"

# The end-to-end tier of spec 023: the interface against a real Origo with
# the stub issuer and stub authorizer of Origo's spec 013. It skips when the
# installation is not named, so `go test ./...` stays hermetic.
test-e2e:
	$(GO) test -race -count=1 -tags=e2e ./test/e2e/... -v

# The interface against the `make dev` stack of the Origo checkout above.
# ORIGOWEB_TEST_REPO_ID is the repository that stack creates.
dev: build
	ORIGOWEB_ADDR=:8090 \
	ORIGOWEB_ORIGO_URL=$${ORIGO_URL:-http://localhost:8080} \
	ORIGOWEB_PUBLIC_URL=http://localhost:8090 \
	ORIGOWEB_AUTH_INSECURE_COOKIES=1 \
	$(OUT_DIR)/$(SERVICE)

clean:
	rm -rf $(OUT_DIR)
