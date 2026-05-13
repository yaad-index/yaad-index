# yaad-index — one-stop verification aliases.
#
# Run `make check` before pushing. `make install-hooks` wires that up as
# a pre-commit block (the hook runs a faster subset — githook-check —
# that skips build + test but enforces fmt / vet / lint / tidy).

LOCAL_PREFIX := github.com/yaad-index/yaad-index
HOOK_PATH    := .githooks

# Build-time identity injected into internal/buildinfo.Version via
# -ldflags -X. The `--dirty` suffix on GIT_TAG makes uncommitted-tree
# builds visibly distinct on /v1/health. GIT_HASH is the abbreviated
# commit for a unique handle even when no tag exists.
GIT_TAG  := $(shell git describe --tags --always --dirty 2>/dev/null)
GIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null)
ifeq ($(strip $(GIT_TAG)),)
VERSION := unknown
else ifeq ($(strip $(GIT_HASH)),)
VERSION := unknown
else
VERSION := $(GIT_TAG)+$(GIT_HASH)
endif
LDFLAGS  := -X '$(LOCAL_PREFIX)/internal/buildinfo.Version=$(VERSION)'

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
	go build -ldflags "$(LDFLAGS)" -o yaad-index ./cmd/yaad-index

build-plugins:
	@mkdir -p plugins
	go build -ldflags "$(LDFLAGS)" -o plugins/yaad-wikipedia ./cmd/yaad-wikipedia
	go build -ldflags "$(LDFLAGS)" -o plugins/yaad-bgg       ./cmd/yaad-bgg
	go build -ldflags "$(LDFLAGS)" -o plugins/yaad-gmail     ./cmd/yaad-gmail

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
# starts as on a local `make build`. TAG defaults to `latest`; override
# via `make docker-build TAG=v1.2.3` for releases.
docker-build:
	DOCKER_BUILDKIT=1 docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg YAAD_UID=$(YAAD_UID) \
		--build-arg YAAD_GID=$(YAAD_GID) \
		-t yaad-index:$(TAG) \
		.
