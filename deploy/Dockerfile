# Build from the repository root:
#   docker build -f deploy/Dockerfile -t mailrules .
# No secret is baked in: the master key is generated into the /data volume on first run,
# and provider keys come from the environment or are set in the browser.

# 1. The web app. The glob matches nothing when web/ is not in the checkout, and then
#    the binary serves its placeholder page.
FROM node:24-alpine AS web
WORKDIR /src/web
COPY we[b]/ ./
RUN if [ -f package.json ]; then npm ci && npm run build; else mkdir dist; fi

# 2. The binary: static, no CGO (the SQLite driver is pure Go).
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist/ internal/web/dist/app/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/mailrules ./cmd/mailrules \
 && mkdir /out/data

# 3. The image: distroless static, running as its non-root user (uid 65532). Only /data
#    is written, so the root filesystem can be read-only.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/mailrules /mailrules
COPY --from=build /src/LICENSE /LICENSE
COPY --from=build --chown=65532:65532 --chmod=700 /out/data /data
# Inside a container the daemon must listen on every interface for the published port to
# reach it; keep it private by publishing to 127.0.0.1 only (see docker-compose.yml).
ENV MAILRULES_DATA_DIR=/data MAILRULES_LISTEN=0.0.0.0:8080
VOLUME /data
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/mailrules"]
CMD ["serve"]
