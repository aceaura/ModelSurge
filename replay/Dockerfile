FROM golang:1.25-alpine AS build
WORKDIR /src

COPY upstream/go.mod upstream/go.sum ./upstream/
COPY replay/go.mod replay/go.sum ./replay/
RUN cd /src/upstream && go mod download \
    && cd /src/replay && go mod download
COPY upstream/ ./upstream/
COPY replay/ ./replay/
WORKDIR /src/replay
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/replay ./cmd/replay

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget su-exec \
    && addgroup -S modelsurge \
    && adduser -S -G modelsurge -h /app modelsurge \
    && mkdir -p /app /data \
    && chown -R modelsurge:modelsurge /app /data
WORKDIR /app
COPY --from=build /out/replay /usr/local/bin/replay
COPY replay/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
EXPOSE 18101
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh", "/usr/local/bin/replay"]
CMD ["-config", "/app/replay.yaml"]
