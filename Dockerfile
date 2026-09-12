# relayd 多阶段构建：纯 Go（CGO_ENABLED=0），产物为单静态二进制。
FROM golang:1.25-alpine AS build
ARG GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/relayd ./cmd/relayd

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/relayd /usr/local/bin/relayd
ENTRYPOINT ["relayd", "-config", "/app/relayd.yaml"]
