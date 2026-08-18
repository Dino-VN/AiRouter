# Build the web UI, then the binary, then ship only the binary.
#
# The result is a distroless image of a few tens of megabytes containing one
# static executable with the UI inside it.

# ---- stage 1: web UI -------------------------------------------------------
FROM oven/bun:1 AS ui

WORKDIR /src/web
# Dependencies first, so a source-only change reuses the install layer.
COPY web/package.json web/bun.lock* ./
RUN bun install --frozen-lockfile || bun install
COPY web/ ./
RUN bun run build

# ---- stage 2: server -------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The UI build wrote into internal/webui/dist; overwrite whatever the context
# carried (typically the committed placeholder) with the freshly built files.
COPY --from=ui /src/internal/webui/dist ./internal/webui/dist

ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/aihub ./cmd/aihub

# ---- stage 3: runtime ------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/aihub /aihub

# Secrets that are not supplied through the environment are generated once and
# written here, so this must be a volume for them to survive a restart.
ENV AIHUB_DATA_DIR=/data
VOLUME ["/data"]

ENV AIHUB_LISTEN=:8317
EXPOSE 8317

USER nonroot:nonroot
ENTRYPOINT ["/aihub"]
