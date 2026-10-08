# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download && go mod verify

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/ftp-binding ./cmd/ftp-binding

FROM alpine:3.24

ARG VERSION=dev

# The release workflow overwrites these via docker/metadata-action, but a
# locally or CI-built image carries them too, so no image is ever anonymous.
LABEL org.opencontainers.image.title="dapr-ftp-binding" \
      org.opencontainers.image.description="FTP and FTPS output binding for Dapr, as a pluggable component" \
      org.opencontainers.image.source="https://github.com/integrio-intropy/dapr-ftp-binding" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

# Certificates are needed to verify FTPS servers
RUN apk add --no-cache ca-certificates \
 && addgroup -S app \
 && adduser -S -G app app \
 && mkdir -p /tmp/dapr-components-sockets \
 && chown app:app /tmp/dapr-components-sockets

COPY --from=builder --chown=app:app /out/ftp-binding /usr/local/bin/ftp-binding

COPY LICENSE NOTICE THIRD_PARTY_LICENSES /usr/local/share/doc/dapr-ftp-binding/

USER app

ENV DAPR_COMPONENTS_SOCKETS_FOLDER=/tmp/dapr-components-sockets

ENTRYPOINT ["/usr/local/bin/ftp-binding"]
