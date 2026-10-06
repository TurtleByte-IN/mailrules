VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Pinned here so local runs and CI lint with the same version.
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

.PHONY: check fmt vet lint test vuln web web-check build dev

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
	@if [ -f web/package.json ]; then \
		cd web && npm ci && npm run check && npm run gen:api && git diff --exit-code -- src/lib/api/schema.d.ts; \
	else echo "web/ is not in this checkout: skipping the web checks"; fi

# The UI is embedded from internal/web/dist (go:embed cannot reach web/dist). That
# directory holds a tracked placeholder index.html; `make web` builds the Svelte app and
# copies it to internal/web/dist/app, which git ignores and the binary prefers when it is
# there. In a checkout without web/ the binary still builds and serves the placeholder.
web:
	@if [ -f web/package.json ]; then \
		(cd web && npm ci && npm run build) && \
		rm -rf internal/web/dist/app && cp -R web/dist internal/web/dist/app; \
	else echo "web/ is not in this checkout: the binary will serve the placeholder page"; fi

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/mailrules ./cmd/mailrules

# The daemon, plus the Vite dev server (which proxies /api to the daemon) when web/ exists.
# Ctrl-C stops both.
dev:
	@trap 'kill 0' EXIT; \
	if [ -f web/package.json ]; then (cd web && { [ -d node_modules ] || npm ci; } && npm run dev) & fi; \
	go run ./cmd/mailrules serve
