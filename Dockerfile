# Stage 1: build
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=$(cat VERSION 2>/dev/null || echo dev)" -o llm-router .

# Stage 2: minimal runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/llm-router .
EXPOSE 8080
VOLUME ["/data"]
ENV DB_PATH=/data/router.db
ENTRYPOINT ["./llm-router"]
