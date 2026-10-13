# syntax=docker/dockerfile:1

# المرحلة الأولى: بناء ملف تنفيذي ثابت بدون أي مكتبات خارجية.
# البناء يتم على معمارية المُنشئ ثم يُترجم للمعمارية المطلوبة.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

# These BuildKit-provided arguments must not have defaults. A hard-coded
# TARGETARCH default can accidentally build an amd64 executable for ARM images.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod main.go ./

RUN set -eu; \
    export GOOS="$TARGETOS" GOARCH="$TARGETARCH"; \
    if [ "$TARGETARCH" = "arm" ]; then \
        case "$TARGETVARIANT" in \
            v5|v6|v7) export GOARM="${TARGETVARIANT#v}" ;; \
            *) echo "Unsupported ARM variant: $TARGETVARIANT" >&2; exit 1 ;; \
        esac; \
    fi; \
    echo "Building $GOOS/$GOARCH${TARGETVARIANT:+/$TARGETVARIANT} (GOARM=${GOARM:-default})"; \
    test "$(go env GOARCH)" = "$TARGETARCH"; \
    if [ "$TARGETARCH" = "arm" ]; then test "$(go env GOARM)" = "${TARGETVARIANT#v}"; fi; \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/um-status-gateway .

# المرحلة الثانية: صورة فارغة لا تحتوي إلا الملف التنفيذي وشهادات الثقة.
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/um-status-gateway /um-status-gateway

# القيم الافتراضية، وكلها قابلة للتغيير من إعدادات الحاوية.
ENV UM_PROTO=http \
    UM_HOST=192.168.216.1 \
    UM_PORT=12008 \
    LISTEN_ADDR=:8080 \
    LOGOUT_DELAY_SECONDS=60 \
    REQUEST_TIMEOUT_SECONDS=8 \
    UM_INSECURE_SKIP_VERIFY=true \
    GOGC=20 \
    GOMEMLIMIT=4MiB

EXPOSE 8080

USER 65534:65534

ENTRYPOINT ["/um-status-gateway"]
