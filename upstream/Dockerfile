FROM golang:1.25-alpine AS build
WORKDIR /src

COPY upstream/go.mod upstream/go.sum ./upstream/
WORKDIR /src/upstream
RUN go mod download
COPY upstream/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/upstream ./cmd/upstream

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget su-exec \
    && addgroup -S modelsurge \
    && adduser -S -G modelsurge -h /app modelsurge \
    && mkdir -p /app /data \
    && chown -R modelsurge:modelsurge /app /data
WORKDIR /app
COPY --from=build /out/upstream /usr/local/bin/upstream
COPY upstream/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
EXPOSE 18100
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh", "/usr/local/bin/upstream"]
CMD ["-config", "/app/upstream.yaml"]
