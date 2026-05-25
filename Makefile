# yaad-index — one-stop verification aliases.
#
# Run `make check` before pushing. `make install-hooks` wires that up as
# a pre-commit block (the hook runs a faster subset — githook-check —
# that skips build + test but enforces fmt / vet / lint / tidy).

LOCAL_PREFIX := github.com/yaad-index/yaad-index
HOOK_PATH    := .githooks

# Build-time identity injected into internal/buildinfo.Version via
# -ldflags -X, per binary. Each component carries its own release-
# please-cut tag prefix (yaad-index-v*, yaad-wikipedia-v*, etc.) so
# the daemon's /v1/health reports its OWN version, not whichever
# component's tag happens to be graph-closest. The `--dirty` suffix
# makes uncommitted-tree builds visibly distinct. GIT_HASH is the
# abbreviated commit for a unique handle even when no tag exists.
#
# Per-binary tags: `--match` filters describe to the component's
# tag namespace. Without it, `git describe` in this monorepo picks
# the most-recent reachable tag ACROSS all components — yaad-index
# could inherit `yaad-bgg-v0.2.0` and silently mis-report.
GIT_HASH           := $(shell git rev-parse --short HEAD 2>/dev/null)
YAAD_INDEX_TAG     := $(shell git describe --tags --always --dirty --match 'yaad-index-v*' 2>/dev/null)
YAAD_WIKIPEDIA_TAG := $(shell git describe --tags --always --dirty --match 'yaad-wikipedia-v*' 2>/dev/null)
YAAD_BGG_TAG       := $(shell git describe --tags --always --dirty --match 'yaad-bgg-v*' 2>/dev/null)
YAAD_GMAIL_TAG     := $(shell git describe --tags --always --dirty --match 'yaad-gmail-v*' 2>/dev/null)
YAAD_GITHUB_TAG    := $(shell git describe --tags --always --dirty --match 'yaad-github-v*' 2>/dev/null)

# version_string(tag) — emit "<tag>+<hash>" normally, but drop the
# redundant "+<hash>" when the tag IS the hash (the `describe
# --always` fallback path when no component-prefixed tag is
# reachable — first release-please cut for a fresh component, OR
# shallow clone without fetched tags). Handles both clean and
# dirty fallback shapes: `<hash>` and `<hash>-dirty`. Bare hash-
# only fallback passes through verbatim instead of doubling to
# "<hash>+<hash>".
define version_string
$(if $(filter $(GIT_HASH) $(GIT_HASH)-dirty,$(1)),$(1),$(1)+$(GIT_HASH))
endef

YAAD_INDEX_VERSION     := $(if $(strip $(YAAD_INDEX_TAG))$(strip $(GIT_HASH)),$(call version_string,$(YAAD_INDEX_TAG)),unknown)
YAAD_WIKIPEDIA_VERSION := $(if $(strip $(YAAD_WIKIPEDIA_TAG))$(strip $(GIT_HASH)),$(call version_string,$(YAAD_WIKIPEDIA_TAG)),unknown)
YAAD_BGG_VERSION       := $(if $(strip $(YAAD_BGG_TAG))$(strip $(GIT_HASH)),$(call version_string,$(YAAD_BGG_TAG)),unknown)
YAAD_GMAIL_VERSION     := $(if $(strip $(YAAD_GMAIL_TAG))$(strip $(GIT_HASH)),$(call version_string,$(YAAD_GMAIL_TAG)),unknown)
YAAD_GITHUB_VERSION    := $(if $(strip $(YAAD_GITHUB_TAG))$(strip $(GIT_HASH)),$(call version_string,$(YAAD_GITHUB_TAG)),unknown)

LDFLAG_PREFIX := -X $(LOCAL_PREFIX)/internal/buildinfo.Version=

.PHONY: help fmt fmt-check lint vet test build build-plugins tidy-check githook-check check install-hooks docker-build

# Container image tag. Override with `make docker-build TAG=v1.2.3`.
TAG ?= latest

# Container user uid/gid. Default 1000:1000 matches the conventional
# first-user uid on debian/ubuntu hosts so bind-mounted host files
# (yaad-index.db, vault/) are writable from inside the container
# without manual chmod. Override on non-1000 hosts:
#   make docker-build YAAD_UID=$(id -u) YAAD_GID=$(id -g)
YAAD_UID ?= 1000
YAAD_GID ?= 1000

help:
	@echo "Targets:"
	@echo "  fmt            format Go files + tidy go.mod/go.sum in place"
	@echo "  fmt-check      verify formatting without changes"
	@echo "  lint           run golangci-lint on all packages"
	@echo "  vet            go vet ./..."
	@echo "  test           go test -race -timeout 2m ./..."
	@echo "  build          build cmd/yaad-index into ./yaad-index"
	@echo "  build-plugins  build all bundled plugin binaries into ./plugins/"
	@echo "  tidy-check     verify go.mod / go.sum are tidy"
	@echo "  githook-check  fmt-check + vet + lint + tidy-check"
	@echo "  check          full CI chain: vet + build + test + fmt-check + lint + tidy-check"
	@echo "  install-hooks  generate .githooks/pre-commit and point git.core.hooksPath at it"
	@echo "  docker-build   build yaad-index:\$$(TAG) container image"

fmt:
	go tool gofumpt -w .
	go tool goimports -w -local $(LOCAL_PREFIX) .
	go mod tidy

fmt-check:
	@out=$$(go tool gofumpt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofumpt: the following files need formatting:"; \
		echo "$$out"; \
		exit 1; \
	fi
	@out=$$(go tool goimports -l -local $(LOCAL_PREFIX) .); \
	if [ -n "$$out" ]; then \
		echo "goimports: the following files need import grouping:"; \
		echo "$$out"; \
		exit 1; \
	fi

lint:
	golangci-lint run ./...

vet:
	go vet ./...

test:
	go test -race -timeout 2m ./...

# e2e harness (yaad-index #1). Build tag keeps it off `make test`.
e2e:
	go test -tags=e2e -timeout 5m -v ./e2e/...

build:
	go build -ldflags "$(LDFLAG_PREFIX)$(YAAD_INDEX_VERSION)" -o yaad-index ./cmd/yaad-index

build-plugins:
	@mkdir -p plugins
	go build -ldflags "$(LDFLAG_PREFIX)$(YAAD_WIKIPEDIA_VERSION)" -o plugins/yaad-wikipedia ./cmd/yaad-wikipedia
	go build -ldflags "$(LDFLAG_PREFIX)$(YAAD_BGG_VERSION)"       -o plugins/yaad-bgg       ./cmd/yaad-bgg
	go build -ldflags "$(LDFLAG_PREFIX)$(YAAD_GMAIL_VERSION)"     -o plugins/yaad-gmail     ./cmd/yaad-gmail
	go build -ldflags "$(LDFLAG_PREFIX)$(YAAD_GITHUB_VERSION)"    -o plugins/yaad-github    ./cmd/yaad-github

tidy-check:
	go mod tidy -diff

githook-check: fmt-check vet lint tidy-check

check: vet build build-plugins test fmt-check lint tidy-check

install-hooks:
	@mkdir -p $(HOOK_PATH)
	@printf '#!/bin/sh\nexec make githook-check\n' > $(HOOK_PATH)/pre-commit
	@chmod +x $(HOOK_PATH)/pre-commit
	@git config core.hooksPath $(HOOK_PATH)
	@echo "installed $(HOOK_PATH)/pre-commit; git core.hooksPath → $(HOOK_PATH)"

# Build the container image. The VERSION ldflag is passed through as
# a build ARG so /v1/health surfaces the same identity on container
# starts as on a local `make build`. The image is the daemon image,
# so YAAD_INDEX_VERSION is the right per-binary identity. TAG
# defaults to `latest`; override via `make docker-build TAG=v1.2.3`
# for releases.
docker-build:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg VERSION=$(YAAD_INDEX_VERSION) \
		--build-arg YAAD_UID=$(YAAD_UID) \
		--build-arg YAAD_GID=$(YAAD_GID) \
		-t yaad-index:$(TAG) \
		.
