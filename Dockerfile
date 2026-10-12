# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1: build a fully static, CGO-free binary for the target architecture.
# The builder always runs on the native build platform and cross compiles.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT=""

# ca-certificates is copied into the runtime stage so an https:// portal works.
RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod ./
COPY main.go buildstatus.go parse.go um.go ./

RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOARM=${TARGETVARIANT#v} \
    go build -trimpath -buildvcs=false \
        -ldflags="-s -w -extldflags=-static" \
        -o /out/um-status-gateway .

# ---------------------------------------------------------------------------
# Stage 2: empty runtime image. Nothing but the binary, the trust store and
# the configuration defaults; no shell, no package manager, no libc.
# ---------------------------------------------------------------------------
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/um-status-gateway /um-status-gateway

# Runtime defaults. UM_HOST must be supplied by the caller.
ENV LISTEN_ADDR=:8080 \
    LOGOUT_DELAY_SECONDS=60 \
    REQUEST_TIMEOUT_SECONDS=15 \
    ALLOWED_ORIGIN=* \
    RELEASE_MEMORY_ON_SESSION_END=true \
    GOMEMLIMIT=4MiB \
    GOGC=20

EXPOSE 8080

# Run unprivileged: nobody:nogroup, resolved numerically so scratch needs no passwd file.
USER 65534:65534

ENTRYPOINT ["/um-status-gateway"]
