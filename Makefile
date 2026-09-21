SHELL := /bin/sh

NODE ?= node
NPM ?= npm
IMAGE ?= harness-cursor:local
VERSION ?= local
SOURCE_REPO ?= .provenance-source/homelab-telegram-panel
GO_TEST_IMAGE ?= golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd

.PHONY: quality fmt-check deps-check vet test-race physical-enospc worker-test contracts-test contracts-parity architecture provenance provenance-test provenance-source patch-check build image container-smoke

quality: patch-check fmt-check deps-check vet test-race physical-enospc worker-test contracts-test contracts-parity architecture provenance-test provenance-source build

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

deps-check:
	go list -mod=readonly ./...
	@! go list -deps ./cmd/harness-node | grep -E 'homelab-telegram-panel|/adapters/codex|/panel/|/router/|/agent-service/'

vet:
	go vet ./...

test-race:
	go test -race ./...

physical-enospc:
	docker run --rm --tmpfs /enospc:rw,nosuid,nodev,noexec,size=32m,mode=0700 --mount "type=bind,source=$(CURDIR),target=/src,readonly" --workdir /src $(GO_TEST_IMAGE) sh -c 'go test -c -o /tmp/integration.test ./tests/integration && TMPDIR=/enospc /tmp/integration.test -test.run=^TestPhysicalFullDiskPreservesAdmissionAndStopReserve$$ -test.v -test.timeout=30s -hl252-physical-disk-full'

worker-test:
	cd adapters/cursor/worker && $(NPM) ci --ignore-scripts --no-audit --no-fund && $(NPM) test

contracts-test:
	cd contracts && $(NPM) ci --ignore-scripts --no-audit --no-fund && $(NPM) test

contracts-parity:
	$(NODE) contracts/generate-harness-v1.mjs
	$(NODE) contracts/generate-transcript-view-v1.mjs
	git diff --exit-code -- contracts/harness-v1.schema.json contracts/harness-v1.fixtures.json contracts/harness-v1.scenarios.json contracts/harness-v1.manifest.json contracts/transcript-view-v1.schema.json contracts/transcript-view-v1.fixtures.json contracts/transcript-view-v1.manifest.json

architecture:
	go test ./tests/architecture

provenance:
	$(NODE) scripts/provenance.mjs verify

provenance-test: provenance
	$(NODE) --test scripts/provenance.test.mjs

provenance-source:
	@test -n "$(SOURCE_REPO)" || { echo "SOURCE_REPO is required" >&2; exit 2; }
	$(NODE) scripts/provenance.mjs verify --source-repo "$(SOURCE_REPO)"

patch-check:
	git diff --check
	git diff --cached --check
	git show --check --format= HEAD
	@index=$$(mktemp); rm -f "$$index"; trap 'rm -f "$$index" "$$index.lock"' EXIT INT TERM; \
		GIT_INDEX_FILE="$$index" git read-tree HEAD; \
		GIT_INDEX_FILE="$$index" git add --all; \
		GIT_INDEX_FILE="$$index" git diff --cached --check

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false ./cmd/harness-node ./cmd/harness-tool-runner

image:
	docker build --file delivery/Dockerfile.cursor --tag $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg REVISION=$$(git rev-parse HEAD) .

container-smoke:
	bash delivery/test-container.sh $(IMAGE)
