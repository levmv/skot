GO ?= go
BINARY ?= sk
DIST_DIR ?= dist
VERSION ?= dev

export BINARY DIST_DIR VERSION

.PHONY: build test vet race format-check mod-check staticcheck check release

build:
	$(GO) build -o "$$BINARY" .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

race:
	$(GO) test -race ./...

format-check:
	@unformatted="$$(gofmt -l $$(git ls-files '*.go'))"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following Go files are not formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

mod-check:
	$(GO) mod tidy -diff

staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...

check: test vet format-check mod-check staticcheck race

release: check
	@set -eu; \
	if [ "$$VERSION" = dev ] || [ -z "$$VERSION" ]; then \
		echo "release requires VERSION=vX.Y.Z" >&2; \
		exit 2; \
	fi; \
	mkdir -p "$$DIST_DIR"; \
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		target_os=$${target%/*}; \
		target_arch=$${target#*/}; \
		output="$$DIST_DIR/sk-$${target_os}-$${target_arch}"; \
		CGO_ENABLED=0 GOOS="$$target_os" GOARCH="$$target_arch" \
			$(GO) build -trimpath -ldflags "-s -w -X main.version=$$VERSION" -o "$$output" .; \
	done; \
	( \
		cd "$$DIST_DIR"; \
		if command -v sha256sum >/dev/null 2>&1; then \
			sha256sum sk-* > checksums.txt; \
		else \
			shasum -a 256 sk-* > checksums.txt; \
		fi \
	)
