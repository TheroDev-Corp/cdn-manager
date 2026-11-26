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
RUN mkdir -p /out/rootfs/etc/ssl/certs /out/rootfs/usr/local/bin /out/rootfs/var/lib/cdn-manager && \
    cp /etc/ssl/certs/ca-certificates.crt /out/rootfs/etc/ssl/certs/ && \
    printf 'nobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin\n' > /out/rootfs/etc/passwd && \
    printf 'nobody:x:65534:\n' > /out/rootfs/etc/group && \
    chmod 0700 /out/rootfs/var/lib/cdn-manager && \
    chown 65534:65534 /out/rootfs/var/lib/cdn-manager
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/rootfs/usr/local/bin/cdn-manager ./cmd/cdn-manager

FROM scratch
COPY --from=build /out/rootfs/ /
USER 65534:65534
EXPOSE 8080 9090
ENV CDN_MANAGER_STATE_DIR=/var/lib/cdn-manager
ENTRYPOINT ["/usr/local/bin/cdn-manager"]
