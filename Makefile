# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

GO ?= go

.PHONY: build build-stubs check clean dev dev-up dev-down docs fmt fuzz hooks release-archives test-integration test-tiers

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

# The release archives of spec 017: every binary of RELEASE_BINARIES for
# the four os/arch pairs with the same LDFLAGS as build, one tar.gz each,
# and checksums.txt with their SHA-256 sums, under out/release/. The
# binaries stay under out/release/bin/<os>_<arch>/, where Dockerfile.ci
# copies the one of its target platform from. release.yml runs this with
# VERSION set to the tag, so a binary from the pipeline and one from make
# build carry their identity the same way (internal/version). The target
# is not called `release`: the shared gate reserves that name for the
# command that cuts a tag, and a target of a gate's name that does
# something else fails `lateregate contract`.
#
# origo is the agent client of spec 025. It runs on the machine an agent
# runs on rather than in a cluster, which is why it takes an archive and
# no image. The `release-verify` job of release.yml downloads by an
# explicit --pattern and then runs `sha256sum -c checksums.txt`, so a
# binary added here needs its pattern added there in the same change, or
# the next tag fails on sums whose files it never fetched.
RELEASE_DIR := $(OUT_DIR)/release
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
RELEASE_BINARIES := $(SERVICE) origo
SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo "shasum -a 256")
release-archives:
	@rm -rf $(RELEASE_DIR) && mkdir -p $(RELEASE_DIR)
	@set -e; for p in $(RELEASE_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; dir=$(RELEASE_DIR)/bin/$${os}_$${arch}; mkdir -p $$dir; \
		for b in $(RELEASE_BINARIES); do \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/$$b ./cmd/$$b; \
			tar -czf $(RELEASE_DIR)/$${b}_$(VERSION)_$${os}_$${arch}.tar.gz -C $$dir $$b; \
			echo "built $(RELEASE_DIR)/$${b}_$(VERSION)_$${os}_$${arch}.tar.gz"; \
		done; \
	done
	@cd $(RELEASE_DIR) && $(SHA256) *.tar.gz > checksums.txt && cat checksums.txt

# The stub binary of spec 013: the issuer, authorizer, sink, and source
# `make dev` runs beside MinIO and the kind overlay runs as a pod.
STUBS := origo-stubs
build-stubs:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -o $(OUT_DIR)/$(STUBS) ./test/stubs/cmd/$(STUBS)
	@echo "built $(OUT_DIR)/$(STUBS)"

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
DEV_COMPOSE_ENV = DEV_PROJECT=$(DEV_PROJECT) DEV_S3_PORT=$(DEV_S3_PORT) \
                  DEV_S3_CONSOLE_PORT=$(DEV_S3_CONSOLE_PORT) DEV_S3_KEY=$(DEV_S3_KEY) \
                  DEV_S3_SECRET=$(DEV_S3_SECRET) DEV_S3_BUCKET=$(DEV_S3_BUCKET)

# The stubs of spec 013 take the four ports after the node's and
# MinIO's, on the loopback interface, so two checkouts run side by side.
DEV_ISSUER_PORT ?= $(shell expr $(DEV_PORT_BASE) + 4)
DEV_AUTHORIZER_PORT ?= $(shell expr $(DEV_PORT_BASE) + 5)
DEV_SINK_PORT ?= $(shell expr $(DEV_PORT_BASE) + 6)
DEV_ISSUER_URL = http://localhost:$(DEV_ISSUER_PORT)
DEV_DIR = $(CURDIR)/$(OUT_DIR)/dev/$(DEV_PROJECT)
# The cache is per project like the bucket: a cache warmed from one
# project's bucket serves another's repositories from a state the other
# bucket never held.
DEV_DATA_DIR ?= $(DEV_DIR)/data
DEV_TOKEN_KEY = $(DEV_DIR)/token-key.pem
DEV_STUBS_PID = $(DEV_DIR)/stubs.pid
DEV_STUBS_LOG = $(DEV_DIR)/stubs.log
DEV_REPO_ID = 0d5e7a1c-6f2b-4c3d-9e8f-1a2b3c4d5e6f

# The node reads the same variables locally as in production; only the
# values differ. The identity variables of spec 007 point at the stub
# issuer and authorizer `make dev` runs (spec 013), and the key is
# generated under out/ at the first start.
DEV_SERVICE_ENV = ORIGO_S3_ENDPOINT=$(DEV_S3_ENDPOINT) ORIGO_S3_REGION=us-east-1 \
                  ORIGO_S3_BUCKET=$(DEV_S3_BUCKET) ORIGO_S3_KEY=$(DEV_S3_KEY) \
                  ORIGO_S3_SECRET=$(DEV_S3_SECRET) ORIGO_S3_PATH_STYLE=1 \
                  ORIGO_DATA_DIR=$(DEV_DATA_DIR) \
                  ORIGO_PUBLIC_URL=http://localhost:$(DEV_PUBLIC_PORT) \
                  ORIGO_PUBLIC_ADDR=:$(DEV_PUBLIC_PORT) ORIGO_INTERNAL_ADDR=:$(DEV_INTERNAL_PORT) \
                  ORIGO_GOSSIP_ADDR=127.0.0.1:0 \
                  ORIGO_OIDC_ISSUERS=$(DEV_ISSUER_URL) ORIGO_OIDC_INSECURE_ISSUERS=$(DEV_ISSUER_URL) \
                  ORIGO_AUTHORIZER_URL=http://127.0.0.1:$(DEV_AUTHORIZER_PORT) \
                  ORIGO_AUTHORIZER_TOKEN=stub-authorizer-token \
                  ORIGO_EVENTS_URL=http://127.0.0.1:$(DEV_SINK_PORT) ORIGO_EVENTS_SECRET=stub-sink-secret

# stack-up starts MinIO and waits for a fact, not a duration: the health
# endpoint answers and this project's one-shot container that made the
# bucket exited (the filter names the project, because another
# checkout's init container satisfies a bare name).
define stack-up
	$(DEV_COMPOSE_ENV) $(COMPOSE) up -d >/dev/null
	for i in $$(seq 1 60); do \
		if curl -sf -o /dev/null "$(DEV_S3_ENDPOINT)/minio/health/live" \
		   && $(DEV_ENGINE) ps -a --filter 'name=$(DEV_PROJECT)[-_]minio-init' --format '{{.Status}}' 2>/dev/null | grep -qi 'exited (0)'; then \
			echo "MinIO is ready at $(DEV_S3_ENDPOINT) with bucket $(DEV_S3_BUCKET)"; break; \
		fi; \
		if [ $$i = 60 ]; then echo "MinIO did not become ready"; $(DEV_COMPOSE_ENV) $(COMPOSE) ps; exit 1; fi; \
		sleep 1; \
	done
endef

# stubs-up starts origo-stubs in the background, once per checkout, and
# waits for the issuer's key set to answer.
define stubs-up
	mkdir -p $(DEV_DIR)
	if ! { [ -f $(DEV_STUBS_PID) ] && kill -0 $$(cat $(DEV_STUBS_PID)) 2>/dev/null; }; then \
		$(OUT_DIR)/$(STUBS) -issuer-listen 127.0.0.1:$(DEV_ISSUER_PORT) \
			-authorizer-listen 127.0.0.1:$(DEV_AUTHORIZER_PORT) -sink-listen 127.0.0.1:$(DEV_SINK_PORT) \
			-issuer-url $(DEV_ISSUER_URL) -authorizer-token stub-authorizer-token \
			>$(DEV_STUBS_LOG) 2>&1 & echo $$! >$(DEV_STUBS_PID); \
	fi
	for i in $$(seq 1 30); do \
		if curl -sf -o /dev/null "$(DEV_ISSUER_URL)/jwks"; then echo "stubs are ready: issuer $(DEV_ISSUER_URL), authorizer http://127.0.0.1:$(DEV_AUTHORIZER_PORT), sink http://127.0.0.1:$(DEV_SINK_PORT)"; break; fi; \
		if [ $$i = 30 ]; then echo "origo-stubs did not become ready"; cat $(DEV_STUBS_LOG); exit 1; fi; \
		sleep 1; \
	done
endef

# clone-line waits for the node, mints a token for the dev subject at the
# stub issuer, creates the repository dev/hello once, and prints the
# clone line. It runs in the background beside the node.
define clone-line
	for i in $$(seq 1 120); do \
		if curl -sf -o /dev/null "http://127.0.0.1:$(DEV_INTERNAL_PORT)/readyz"; then break; fi; \
		if [ $$i = 120 ]; then echo "origod did not become ready" >&2; exit 1; fi; \
		sleep 1; \
	done; \
	token=$$(curl -sf -X POST "$(DEV_ISSUER_URL)/mint" -d '{"sub":"dev"}' | sed 's/.*"token":"\([^"]*\)".*/\1/'); \
	curl -sf -o /dev/null -X POST -H "Authorization: Bearer $$token" -H "Content-Type: application/json" \
		"http://127.0.0.1:$(DEV_PUBLIC_PORT)/v1/repos" -d '{"id":"$(DEV_REPO_ID)","owner":"dev","slug":"hello"}' || true; \
	echo "git clone http://x:$$token@localhost:$(DEV_PUBLIC_PORT)/dev/hello.git"
endef

# One command from a clean clone to a serving node: MinIO with the
# bucket, the stub issuer, authorizer, and sink beside it, a signing key
# generated once under out/, then origod in the foreground, with a clone
# line whose token the stub issuer minted printed once the node is ready.
dev: build build-stubs
	@$(stack-up)
	@$(stubs-up)
	@test -s $(DEV_TOKEN_KEY) || openssl ecparam -genkey -name prime256v1 -out $(DEV_TOKEN_KEY)
	@mkdir -p $(DEV_DATA_DIR)
	@( $(clone-line) ) &
	$(DEV_SERVICE_ENV) ORIGO_TOKEN_KEY="$$(cat $(DEV_TOKEN_KEY))" $(OUT_DIR)/$(SERVICE)

# The three-node stack the cluster tiers target, in a kind cluster named
# after the checkout: both images built with the engine, saved as
# tarballs, and handed to deploy/examples/kind/up.sh. kind follows the
# engine `make dev` uses.
KIND_PROVIDER = $(if $(filter podman,$(DEV_ENGINE)),KIND_EXPERIMENTAL_PROVIDER=podman,)
dev-up:
	@mkdir -p $(OUT_DIR)/images
	$(DEV_ENGINE) build -t ghcr.io/latere-ai/origod:candidate .
	$(DEV_ENGINE) build -f Dockerfile.stubs -t ghcr.io/latere-ai/origo-stubs:candidate .
	$(DEV_ENGINE) save -o $(OUT_DIR)/images/origod.tar ghcr.io/latere-ai/origod:candidate
	$(DEV_ENGINE) save -o $(OUT_DIR)/images/origo-stubs.tar ghcr.io/latere-ai/origo-stubs:candidate
	$(KIND_PROVIDER) deploy/examples/kind/up.sh -name $(DEV_PROJECT) $(OUT_DIR)/images/origod.tar $(OUT_DIR)/images/origo-stubs.tar

# Stops what `make dev` or `make dev-up` started for this checkout: the
# stubs, the compose stack, and the kind cluster. Each half is a no-op
# when nothing of it runs; out/ stays in place (`make clean` removes it).
dev-down:
	-@if [ -f $(DEV_STUBS_PID) ]; then kill $$(cat $(DEV_STUBS_PID)) 2>/dev/null; rm -f $(DEV_STUBS_PID); echo "stubs stopped"; fi
	-@$(DEV_COMPOSE_ENV) $(COMPOSE) down --remove-orphans 2>/dev/null
	-@if command -v kind >/dev/null 2>&1; then $(KIND_PROVIDER) deploy/examples/kind/down.sh -name $(DEV_PROJECT); fi

# The tiers that need a bucket beside them (spec 013): the store suite
# (integration tag) and the e2e tier's one-node run, the TestE2E prefix,
# against whatever values of the ORIGO_TEST_S3_* variables the
# environment carries; the tiers skip when the endpoint is unset. The
# integration job calls this with its MinIO; a cluster test targets the
# stack through ORIGO_TEST_URL and is selected by its job's prefix.
# ORIGO_E2E_MEASURE=1 with -run TestMeasure prints the measurements spec
# 004's Outcome records.
test-tiers:
	$(GO) test -race -count=1 -tags=integration ./internal/wal/...
	$(GO) test -race -count=1 -timeout 20m -tags=e2e ./test/e2e/... -run 'TestE2E'

# The developer's form: starts compose and runs the same tiers against it.
TEST_S3_ENV = ORIGO_TEST_S3_ENDPOINT=$(DEV_S3_ENDPOINT) ORIGO_TEST_S3_REGION=us-east-1 \
              ORIGO_TEST_S3_BUCKET=$(DEV_S3_BUCKET) ORIGO_TEST_S3_KEY=$(DEV_S3_KEY) \
              ORIGO_TEST_S3_SECRET=$(DEV_S3_SECRET) ORIGO_TEST_S3_PATH_STYLE=1
test-integration:
	@$(stack-up)
	$(TEST_S3_ENV) $(MAKE) test-tiers

# Every fuzz function in the module for 40 seconds, one package at a
# time, the list from `go test -list`; the fuzz job runs it weekly.
fuzz:
	@for pkg in $$($(GO) list ./...); do \
		for fn in $$($(GO) test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz'); do \
			echo "== $$pkg $$fn"; \
			$(GO) test -run='^$$' -fuzz="^$$fn\$$" -fuzztime=40s $$pkg || exit 1; \
		done; \
	done

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

# The two generated pages: docs/configuration.md from internal/config,
# where the reference lives beside the code that reads each variable, and
# docs/api.md from the endpoint, header, and code tables of the specs.
# The specindex job runs this and then `git diff --exit-code docs/`, so a
# change to a variable or to a spec table shows up as a documentation
# diff on the same push.
docs:
	$(GO) run ./tools/configdoc -write
	cd tools/apidoc && $(GO) run . -write
.PHONY: docs
