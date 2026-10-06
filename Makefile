VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Pinned here so local runs and CI lint with the same version.
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

.PHONY: check fmt vet lint test vuln build dev

check: fmt vet lint test

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:
	$(GOLANGCI) run

test:
	go test -race ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# The web app joins build and dev once web/ exists.
build:
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/mailrules ./cmd/mailrules

dev:
	go run ./cmd/mailrules serve
