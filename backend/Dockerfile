FROM golang:1.25-alpine AS build
ARG GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/relay ./cmd/relay && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/upstream ./cmd/upstream

FROM alpine:3.20 AS relay
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/relay /usr/local/bin/relay
ENTRYPOINT ["relay", "-config", "/app/relay.yaml"]

FROM alpine:3.20 AS upstream
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/upstream /usr/local/bin/upstream
ENTRYPOINT ["upstream", "-config", "/app/upstream.yaml"]
