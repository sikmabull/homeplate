
BINARY      := homerun
MODULE      := github.com/homerun-ci/homerun
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS     := -s -w \
	-X $(MODULE)/internal/cli.Version=$(VERSION) \
	-X $(MODULE)/internal/cli.Commit=$(COMMIT)

# Distributors who have registered an OAuth App can bake in the client_id:
#   make CLIENT_ID=Iv1.xxxxxxxx
ifdef CLIENT_ID
LDFLAGS += -X $(MODULE)/internal/auth.DefaultClientID=$(CLIENT_ID)
endif

.PHONY: all build install test lint fmt vet clean dist check

all: check build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/homerun

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/homerun

test:
	go test ./...

# The four guarantees the README makes, tested explicitly.
test-guarantees:
	go test -v -run 'TestProfileMultiplexing|TestTokensNeverWrittenToDisk' ./internal/auth
	go test -v -run 'Idempotent' ./internal/store ./internal/syncbrain
	go test -v -run 'TestResourceCaps|TestSecurityDefaults' ./internal/runner
	go test -v -run 'TestReplay' ./internal/syncbrain

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet test

# Cross-compilation targets. macOS first, Linux second.
dist: clean
	@mkdir -p dist
	@for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/$(BINARY)-$$os-$$arch ./cmd/homerun; \
		tar -czf dist/$(BINARY)-$(VERSION)-$$os-$$arch.tar.gz -C dist $(BINARY)-$$os-$$arch; \
	done
	@cd dist && shasum -a 256 *.tar.gz > checksums.txt
	@echo "\ndist/ contents:" && ls -1 dist/

clean:
	rm -rf dist $(BINARY)
