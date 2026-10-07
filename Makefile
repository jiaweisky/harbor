GO  ?= go
BIN ?= bin
PKGS := ./...

ifeq ($(GOOS),windows)
EXE := .exe
else
EXE :=
endif

HARBOR := $(BIN)/harbor$(EXE)

.PHONY: all build vet test lint fmt tidy clean run

all: build

build:
	$(GO) build -o $(HARBOR) ./cmd/harbor

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
	rm -rf $(BIN)

run: build
	$(HARBOR) --config examples/agents.yaml
