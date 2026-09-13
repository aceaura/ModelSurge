FROM golang:1.25-alpine AS build
WORKDIR /src

COPY upstream/go.mod upstream/go.sum ./upstream/
COPY replay/go.mod replay/go.sum ./replay/
COPY agent/go.mod agent/go.sum ./agent/
RUN cd /src/upstream && go mod download \
    && cd /src/replay && go mod download \
    && cd /src/agent && go mod download
COPY upstream/ ./upstream/
COPY replay/ ./replay/
COPY agent/ ./agent/
WORKDIR /src/agent
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget su-exec \
    && addgroup -S modelsurge \
    && adduser -S -G modelsurge -h /app modelsurge \
    && mkdir -p /app /data \
    && chown -R modelsurge:modelsurge /app /data
WORKDIR /app
COPY --from=build /out/agent /usr/local/bin/agent
COPY agent/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
EXPOSE 18099
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh", "/usr/local/bin/agent"]
CMD ["-config", "/app/agent.yaml"]
