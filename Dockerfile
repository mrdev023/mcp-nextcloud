# syntax=docker/dockerfile:1

# Build on the native platform and cross-compile: no emulation is needed for
# multi-arch images (docker buildx build --platform linux/amd64,linux/arm64).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mcp-nextcloud ./cmd/mcp-nextcloud

# distroless/static: CA certificates + tzdata, no shell, non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mcp-nextcloud /mcp-nextcloud
ENV MCP_PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/mcp-nextcloud"]
