# syntax=docker/dockerfile:1.4

FROM golang:1.24.2-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o solana-balance-api .

FROM alpine:latest

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /app/solana-balance-api .

ENV REDIS_ADDR=redis:6379
ENV MONGODB_URI=mongodb://mongo:27017
ENV MONGODB_DB=infra
ENV MONGODB_COL=api_keys

EXPOSE 8080

ENTRYPOINT ["./solana-balance-api"]