# The version without the leading v (0.1.1, not v0.1.1), like release archives and image tags.
VERSION  ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
# Pinned here so local runs and CI lint with the same version.
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

.PHONY: version check fmt vet lint test vuln settings-doc settings-doc-check web web-check build dev

# Prints the version a build from this checkout reports; used for the compose build arg.
version:
	@echo $(VERSION)

check: fmt vet lint test settings-doc-check web-check

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:
	$(GOLANGCI) run

# The core packages must keep at least COVER_MIN% statement coverage (CLAUDE.md). The test
# output is kept so the floor is read from the same run; a failing test fails first.
COVER_MIN  ?= 80
COVER_PKGS := internal/rules internal/pipeline internal/actions

test:
	@out=$$(mktemp); go test -race -cover ./... > $$out 2>&1; status=$$?; cat $$out; \
	if [ $$status -ne 0 ]; then rm -f $$out; exit $$status; fi; \
	awk -v min=$(COVER_MIN) -v pkgs="$(COVER_PKGS)" ' \
		BEGIN { n = split(pkgs, want, " ") } \
		$$1 == "ok" { for (i = 3; i < NF; i++) if ($$i == "coverage:") { c = $$(i + 1); sub("%", "", c); got[$$2] = c } } \
		END { bad = 0; for (k = 1; k <= n; k++) { p = ""; for (g in got) if (g ~ ("/" want[k] "$$")) p = g; \
			if (p == "") { printf "coverage: no figure for %s\n", want[k]; bad = 1 } \
			else if (got[p] + 0 < min) { printf "coverage: %s is at %s%%, below the %s%% floor\n", want[k], got[p], min; bad = 1 } \
			else printf "coverage: %s %s%% (floor %s%%)\n", want[k], got[p], min } \
		exit bad }' $$out; status=$$?; rm -f $$out; exit $$status

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# docs/guide/settings.md is generated from the settings internal/config registers.
# settings-doc rewrites it; settings-doc-check fails when the committed page differs.
settings-doc:
	go run ./internal/config/docgen -o docs/guide/settings.md

settings-doc-check:
	go run ./internal/config/docgen -check docs/guide/settings.md

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
