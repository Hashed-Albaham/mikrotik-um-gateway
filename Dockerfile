# syntax=docker/dockerfile:1

# المرحلة الأولى: بناء ملف تنفيذي ثابت بدون أي مكتبات خارجية.
# البناء يتم على معمارية المُنشئ ثم يُترجم للمعمارية المطلوبة.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT=""

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod main.go ./

RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOARM=${TARGETVARIANT#v} \
    go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/um-status-gateway .

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
