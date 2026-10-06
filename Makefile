VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Pinned here so local runs and CI lint with the same version.
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

.PHONY: check fmt vet lint test vuln build dev web web-check

check: fmt vet lint test web-check

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

# Type check and tests, then fail if the generated API types are stale against api/openapi.yaml.
web-check:
	cd web && npm ci && npm run check && npm run gen:api && git diff --exit-code -- src/lib/api/schema.d.ts

web:
	cd web && npm ci && npm run build

# Embedding web/dist in the binary is backend milestone M10.
build: web
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/mailrules ./cmd/mailrules

# Daemon plus the Vite dev server (http://localhost:5173), which proxies /api to the daemon.
dev:
	cd web && npm ci
	(trap 'kill 0' EXIT; go run ./cmd/mailrules serve & cd web && npm run dev)
