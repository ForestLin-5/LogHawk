FROM golang:1.21-alpine AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /app
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o ingest .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /app/ingest /ingest
EXPOSE 8001
USER 1000
ENTRYPOINT ["/ingest"]
