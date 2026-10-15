# b2b_pilot_v3 —— 多目标构建（api / worker / wecom / sync + 影子轨 shadow-*）
FROM golang:1.25-alpine AS build
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -o /out/wecom ./cmd/wecom && \
    CGO_ENABLED=0 go build -o /out/sync ./cmd/sync && \
    CGO_ENABLED=0 go build -o /out/seedrel ./scripts/seedrel && \
    CGO_ENABLED=0 go build -o /out/shadowingest ./cmd/shadow-ingest && \
    CGO_ENABLED=0 go build -o /out/shadowfeed ./cmd/shadow-feed && \
    CGO_ENABLED=0 go build -o /out/shadowaudit ./cmd/shadow-audit && \
    CGO_ENABLED=0 go build -o /out/spicedbhealthcheck ./cmd/spicedb-healthcheck

FROM alpine:3.20 AS api
COPY --from=build /out/api /usr/local/bin/api
ENTRYPOINT ["/usr/local/bin/api"]

FROM alpine:3.20 AS worker
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["/usr/local/bin/worker"]

FROM alpine:3.20 AS wecom
COPY --from=build /out/wecom /usr/local/bin/wecom
ENTRYPOINT ["/usr/local/bin/wecom"]

FROM alpine:3.20 AS sync
COPY --from=build /out/sync /usr/local/bin/sync
ENTRYPOINT ["/usr/local/bin/sync"]

FROM alpine:3.20 AS seedrel
COPY --from=build /out/seedrel /usr/local/bin/seedrel
ENTRYPOINT ["/usr/local/bin/seedrel"]

# ── 影子轨 S（docs/v0.1/12-shadow-full-flow.html §7）──
# shadow-api 复用 api 二进制（桥接禁用、身份白名单收紧由环境变量 + 启动守卫强制）。

FROM alpine:3.20 AS shadow-api
COPY --from=build /out/api /usr/local/bin/api
ENTRYPOINT ["/usr/local/bin/api"]

FROM alpine:3.20 AS shadow-ingest
COPY --from=build /out/shadowingest /usr/local/bin/shadow-ingest
ENTRYPOINT ["/usr/local/bin/shadow-ingest"]

FROM alpine:3.20 AS shadow-feed
COPY --from=build /out/shadowfeed /usr/local/bin/shadow-feed
ENTRYPOINT ["/usr/local/bin/shadow-feed"]

FROM alpine:3.20 AS shadow-audit
COPY --from=build /out/shadowaudit /usr/local/bin/shadow-audit
ENTRYPOINT ["/usr/local/bin/shadow-audit"]

# spicedb = 官方 spicedb 镜像（无 shell/curl/wget）+ 本项目 gRPC 健康探针。
# SpiceDB v1.39.1 没有 `healthcheck` 子命令，compose healthcheck 用探针直连
# grpc.health.v1.Health/Check，并携带 preshared key。
FROM authzed/spicedb:v1.39.1 AS spicedb
COPY --from=build /out/spicedbhealthcheck /usr/local/bin/spicedb-healthcheck

FROM spicedb AS shadow-spicedb
