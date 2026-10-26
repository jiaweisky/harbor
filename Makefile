GO   ?= go
BIN  ?= bin
DIST ?= dist
PKGS := ./...

# Build metadata. An exact git tag (HEAD tagged) is a release version; anything
# else is a dev build carrying the short commit and a -dirty marker. Outside git
# everything degrades to "dev".
# Tag version: take the exact tag on HEAD only when it is made of
# [0-9A-Za-z._+-] (an optional leading "v" before a digit is stripped:
# v1.2.3 -> 1.2.3, vendor-drop stays). The filter runs inside the shell
# pipeline, so an unsafe tag never reaches make expansion: a tag like
# v1.2.3$(cmd) would otherwise be command-substituted when a recipe expands
# VERSION. Anything else is treated as untagged (dev build).
TAG_VERSION := $(shell git describe --tags --exact-match 2>/dev/null | grep -E '^v?[0-9A-Za-z._+-]+$$' | sed 's/^v\([0-9]\)/\1/')
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
# "-dirty" when the working tree differs from HEAD. Unlike `git describe
# --dirty`, untracked files count too, because `go build` compiles them.
GIT_DIRTY := $(shell git status --porcelain 2>/dev/null | grep -q . && echo -dirty)

ifneq ($(TAG_VERSION),)
# A tagged-but-dirty HEAD is not a release build.
BUILD_VERSION := $(TAG_VERSION)$(GIT_DIRTY)
BUILD_KIND := $(if $(GIT_DIRTY),dev,release)
else ifneq ($(GIT_COMMIT),)
BUILD_VERSION := dev-$(GIT_COMMIT)$(GIT_DIRTY)
BUILD_KIND := dev
else
BUILD_VERSION := dev
BUILD_KIND := dev
endif

# Version stamped into binaries and release archive names; override to stamp a
# custom one (e.g. `make release VERSION=1.2.3` still requires a tagged HEAD).
# VERSION is interpolated into shell recipes, so only pass trusted values; the
# filters above cover repo-derived tags, not caller-supplied overrides.
VERSION ?= $(BUILD_VERSION)

# Link-time stamp for the command that exposes a version (cmd/harbor main.version).
VERSION_FLAG := -X main.version=$(VERSION)

ifeq ($(GOOS),windows)
EXE := .exe
else
EXE :=
endif

HARBOR := $(BIN)/harbor$(EXE)

.PHONY: all build vet test lint fmt tidy clean run version release

all: build

build:
	$(GO) build -ldflags "$(VERSION_FLAG)" -o $(HARBOR) ./cmd/harbor

vet:
	$(GO) vet $(PKGS)

test:
	$(GO) test -race $(PKGS)

# golangci-lint must be built with a Go toolchain >= the module's go directive.
lint:
	golangci-lint run ./...

fmt:
	gofmt -w cmd internal

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN) $(DIST)

run: build
	$(HARBOR) --config examples/agents.yaml

# Print the version and kind that the next build would stamp, e.g.
# "dev-abc1234-dirty (dev)" or "1.2.3 (release)".
version:
	@echo "$(VERSION) ($(BUILD_KIND))"

# Build the command for the current platform and package a release archive into
# $(DIST)/: a tar.gz plus a sha256 checksum file, named harbor-$(VERSION). Fails
# unless HEAD is exactly tagged and clean, so a release archive can never carry
# a dev version stamp.
release:
	@test -n "$(TAG_VERSION)" || { \
		echo "release: HEAD must be tagged with a version tag (e.g. v1.2.3)" >&2; exit 1; }
	@test -z "$(GIT_DIRTY)" || { \
		echo "release: working tree is dirty, commit or stash first:" >&2; \
		git status --short >&2; exit 1; }
	@case "$(VERSION)" in \
		'') echo "release: VERSION is empty" >&2; exit 1;; \
		*[!0-9A-Za-z._+-]*) \
			echo "release: VERSION may only contain [0-9A-Za-z._+-]" >&2; exit 1;; \
	esac
	rm -rf "$(DIST)"
	mkdir -p "$(DIST)/harbor-$(VERSION)"
	$(GO) build -trimpath -ldflags "-s -w $(VERSION_FLAG)" \
		-o "$(DIST)/harbor-$(VERSION)/harbor$(EXE)" ./cmd/harbor
	tar -czf "$(DIST)/harbor-$(VERSION).tar.gz" -C "$(DIST)" "harbor-$(VERSION)"
	rm -rf "$(DIST)/harbor-$(VERSION)"
	cd "$(DIST)" && sha256sum * > sha256sums.txt
	@echo "release archive in $(DIST)/ ($(VERSION))"
