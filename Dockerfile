# syntax=docker/dockerfile:1
# Go 版 DoH 代理容器镜像：多阶段构建，仅标准库，最终镜像基于 alpine（含 CA 证书以访问 https 上游）
FROM golang:1.22-alpine AS build
WORKDIR /src/server
COPY server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/doh-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/doh-proxy /usr/local/bin/doh-proxy

# 非 root 运行
USER 65534:65534

# 运行配置全部走环境变量（与 server/README.md 一致），
# 例：-e TLS_CERT=/tls/x.crt -e TLS_KEY=/tls/x.key 后 ：8080 自动变 HTTPS
ENV LISTEN_ADDR=:8080 \
	HEALTH_PATH=/healthz \
	METRICS_PATH=/metrics \
	DEFAULT_UPSTREAM=cloudflare \
	CACHE_TTL=30

EXPOSE 8080 8444
ENTRYPOINT ["/usr/local/bin/doh-proxy"]

# 健康检查按默认 LISTEN_ADDR :8080 探测；
# 自定义 LISTEN_ADDR 时用 --health-cmd 覆盖，例如：
#   --health-cmd "wget -qO- http://127.0.0.1:<port>/healthz || exit 1"
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
	CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1
