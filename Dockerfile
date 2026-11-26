# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM oven/bun:1.4.2 AS ui
WORKDIR /app/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/assets.go web/assets.go
COPY --from=ui /app/web/dist/ web/dist/
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/cdn-manager ./cmd/cdn-manager
RUN mkdir -p /out/state && chmod 0700 /out/state && chown 65532:65532 /out/state

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/cdn-manager /usr/local/bin/cdn-manager
COPY --chown=65532:65532 --from=build /out/state /var/lib/cdn-manager
USER 65532:65532
EXPOSE 8080 9090
ENV CDN_MANAGER_STATE_DIR=/var/lib/cdn-manager
ENTRYPOINT ["/usr/local/bin/cdn-manager"]
