# ===== 构建阶段 =====
FROM golang:1-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o uniflow .

# ===== 运行阶段 =====
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Shanghai

WORKDIR /app
COPY --from=builder /app/uniflow .
COPY --from=builder /app/templates ./templates
COPY --from=builder /app/static ./static

RUN mkdir -p /app/uploads /app/backups

EXPOSE 8080
CMD ["./uniflow"]
