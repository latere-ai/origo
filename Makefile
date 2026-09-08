# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

GO ?= go

.PHONY: build check clean dev fmt hooks test-integration

# The whole bar. Every gate lives in latere.ai/x/ci-gate, pinned as a tool
# in go.mod and configured in .lateregate.yaml, so this target is a name for
# `go tool lateregate` and nothing else. One gate at a time: `go tool
# lateregate cover`. The plan: `go tool lateregate list`.
check: specindex
	@$(GO) tool lateregate

.DEFAULT_GOAL := check

OUT_DIR := out
SERVICE := origod
MODULE := $(shell $(GO) list -m)

# Build metadata, deferred so the git and date calls run only for a build. A
# dirty tree marks the commit, because a binary built from uncommitted
# changes cannot be reproduced from its commit.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = $(MODULE)/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) \
          -X $(VERSION_PKG).Commit=$(COMMIT)$(DIRTY) \
          -X $(VERSION_PKG).Date=$(BUILD_DATE)

build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(OUT_DIR)/$(SERVICE) ./cmd/$(SERVICE)
	@echo "built $(OUT_DIR)/$(SERVICE)"

fmt:
	gofmt -w $$(git ls-files '*.go')

# Point git at the delegating hooks. Per clone, so it is a target.
hooks:
	chmod +x .githooks/*
	git config core.hooksPath .githooks

# The local stack is MinIO from docker-compose.yml. The project name and
# the ports derive from the checkout's directory name so two checkouts run
# side by side. Docker by default; another engine that speaks the same
# command line is selected with DEV_ENGINE.
DEV_ENGINE ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)
DEV_PROJECT ?= $(notdir $(CURDIR))
COMPOSE = $(DEV_ENGINE) compose -f docker-compose.yml -p $(DEV_PROJECT)
DEV_PORT_BASE ?= $(shell printf '%s' '$(DEV_PROJECT)' | cksum | awk '{print 20000 + ($$1 % 300) * 10}')
DEV_PUBLIC_PORT ?= $(DEV_PORT_BASE)
DEV_INTERNAL_PORT ?= $(shell expr $(DEV_PORT_BASE) + 1)
DEV_S3_PORT ?= $(shell expr $(DEV_PORT_BASE) + 2)
DEV_S3_CONSOLE_PORT ?= $(shell expr $(DEV_PORT_BASE) + 3)
DEV_S3_KEY ?= minioadmin
DEV_S3_SECRET ?= minioadmin
DEV_S3_BUCKET ?= origo
DEV_S3_ENDPOINT ?= http://127.0.0.1:$(DEV_S3_PORT)
DEV_DATA_DIR ?= $(CURDIR)/$(OUT_DIR)/data
DEV_COMPOSE_ENV = DEV_PROJECT=$(DEV_PROJECT) DEV_S3_PORT=$(DEV_S3_PORT) \
                  DEV_S3_CONSOLE_PORT=$(DEV_S3_CONSOLE_PORT) DEV_S3_KEY=$(DEV_S3_KEY) \
                  DEV_S3_SECRET=$(DEV_S3_SECRET) DEV_S3_BUCKET=$(DEV_S3_BUCKET)

# The node reads the same variables locally as in production; only the
# values differ. The identity variables of spec 007 (ORIGO_OIDC_ISSUERS,
# ORIGO_AUTHORIZER_URL, ORIGO_AUTHORIZER_TOKEN, ORIGO_TOKEN_KEY) are set
# by the form of `make dev` spec 013 builds, which runs the stub issuer
# and authorizer beside MinIO and generates the key under out/.
DEV_SERVICE_ENV = ORIGO_S3_ENDPOINT=$(DEV_S3_ENDPOINT) ORIGO_S3_REGION=us-east-1 \
                  ORIGO_S3_BUCKET=$(DEV_S3_BUCKET) ORIGO_S3_KEY=$(DEV_S3_KEY) \
                  ORIGO_S3_SECRET=$(DEV_S3_SECRET) ORIGO_S3_PATH_STYLE=1 \
                  ORIGO_DATA_DIR=$(DEV_DATA_DIR) \
                  ORIGO_PUBLIC_URL=http://localhost:$(DEV_PUBLIC_PORT) \
                  ORIGO_PUBLIC_ADDR=:$(DEV_PUBLIC_PORT) ORIGO_INTERNAL_ADDR=:$(DEV_INTERNAL_PORT) \
                  ORIGO_GOSSIP_ADDR=127.0.0.1:0

# stack-up starts MinIO and waits for a fact, not a duration: the health
# endpoint answers and the one-shot container that made the bucket exited.
define stack-up
	$(DEV_COMPOSE_ENV) $(COMPOSE) up -d >/dev/null
	for i in $$(seq 1 60); do \
		if curl -sf -o /dev/null "$(DEV_S3_ENDPOINT)/minio/health/live" \
		   && $(DEV_ENGINE) ps -a --filter name=minio-init --format '{{.Status}}' 2>/dev/null | grep -qi 'exited (0)'; then \
			echo "MinIO is ready at $(DEV_S3_ENDPOINT) with bucket $(DEV_S3_BUCKET)"; break; \
		fi; \
		if [ $$i = 60 ]; then echo "MinIO did not become ready"; $(DEV_COMPOSE_ENV) $(COMPOSE) ps; exit 1; fi; \
		sleep 1; \
	done
endef

# One command from a clean clone to a serving node: MinIO with the bucket,
# then origod in the foreground. Out of service between spec 007 and
# spec 013: the phase 1 bearer is gone and the node needs an issuer, an
# authorizer, and a signing key, which the stub binary spec 013 builds
# (test/stubs/cmd/origo-stubs) provides. Until then the unit suites and
# `make test-integration` run the stubs in-process.
dev:
	@echo "make dev is out of service until spec 013 lands: origod needs the stub issuer and authorizer of test/stubs/cmd/origo-stubs (see specs/007, Current state)" >&2
	@exit 1

# The tiers that need MinIO beside them, which is why they are here rather
# than gates: latere-ai/ci's lateregate.yml has no services step, so CI runs
# the unit tiers and a developer runs this before a push that touches the
# log. First the store suite the in-process fake also passes, then the
# end-to-end suite with origod as a subprocess and the real git.
# ORIGO_E2E_MEASURE=1 adds the measurements spec 004's Outcome records.
TEST_S3_ENV = ORIGO_TEST_S3_ENDPOINT=$(DEV_S3_ENDPOINT) ORIGO_TEST_S3_REGION=us-east-1 \
              ORIGO_TEST_S3_BUCKET=$(DEV_S3_BUCKET) ORIGO_TEST_S3_KEY=$(DEV_S3_KEY) \
              ORIGO_TEST_S3_SECRET=$(DEV_S3_SECRET) ORIGO_TEST_S3_PATH_STYLE=1
test-integration:
	@$(stack-up)
	$(TEST_S3_ENV) $(GO) test -race -count=1 -tags=integration ./internal/wal/...
	$(TEST_S3_ENV) $(GO) test -race -count=1 -timeout 60m -tags=e2e ./test/e2e/...

# Stop the stack, remove its volume, and remove the build output and the
# local repository cache. Local state is disposable.
clean:
	-@$(DEV_COMPOSE_ENV) $(COMPOSE) down --volumes --remove-orphans 2>/dev/null
	rm -rf $(OUT_DIR)

# The cross-reference table at the end of specs/README.md is generated from
# the specs; this fails when a spec and the table disagree.
specindex:
	cd tools/specindex && go test ./...
.PHONY: specindex
