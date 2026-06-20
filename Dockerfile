# ===== 构建阶段 =====
FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o uniflow .

# ===== 运行阶段 =====
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -u 1001 uniflow && \
    mkdir -p /app/uploads /app/backups /app/data /app/static && \
    chown -R uniflow:uniflow /app

ENV TZ=Asia/Shanghai

WORKDIR /app
COPY --from=builder --chown=uniflow:uniflow /app/uniflow .
COPY --from=builder --chown=uniflow:uniflow /app/templates ./templates
COPY --from=builder --chown=uniflow:uniflow /app/static ./static

USER uniflow

EXPOSE 9090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q --spider http://localhost:9090/ || exit 1

CMD ["./uniflow"]
