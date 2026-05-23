FROM golang:1.26-alpine AS builder
ARG VERSION=dev
WORKDIR /app
COPY go.mod go.sum ./
# BuildKit cache mounts persist Go's module and build caches across
# `docker build` invocations. Without them every rebuild re-downloads every
# module and recompiles the whole world — especially painful under QEMU
# cross-builds (scripts/push-to-prod.sh's emergency arm64 path).
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${VERSION}" -o hikyaku ./cmd/hikyaku
# Pre-create /capture so the default sig_message_capture.output_folder works
# when the container runs as a non-root UID. Owned by the runtime UID/GID.
RUN mkdir -p /rootfs/capture && chown -R 65532:65532 /rootfs

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/hikyaku /hikyaku
COPY --from=builder /rootfs/ /
USER 65532:65532
ENTRYPOINT ["/hikyaku"]
