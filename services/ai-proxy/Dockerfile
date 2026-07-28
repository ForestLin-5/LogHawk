# ---- Build Stage ----
FROM golang:1.21-alpine AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /app
COPY go.mod go.sum ./
COPY main.go ./
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ai-proxy .

# ---- Runtime Stage ----
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /app/ai-proxy /ai-proxy
EXPOSE 8003
ENV AI_PROXY_PORT=8003
USER 1000
ENTRYPOINT ["/ai-proxy"]
