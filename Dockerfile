# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nasuta ./cmd/nasuta

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git
WORKDIR /app
COPY --from=build /out/nasuta /usr/local/bin/nasuta
ENV NASUTA_WORKSPACE_ROOT=/workspace \
    NASUTA_SQLITE_PATH=/workspace/.nasuta/index.db
EXPOSE 8201
VOLUME ["/data", "/workspace"]
ENTRYPOINT ["nasuta"]
