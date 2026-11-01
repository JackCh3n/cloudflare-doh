#!/usr/bin/env bash
# ============================================================
# deploy-linux.sh - doh-proxy Linux 一键部署脚本（在目标服务器上执行）
#
# 用法：
#   sudo bash deploy-linux.sh                      # 从 GitHub Releases 安装最新版
#   sudo VERSION=v1.4.0 bash deploy-linux.sh       # 安装指定版本
#   sudo bash deploy-linux.sh --file ./doh-proxy   # 安装本地二进制（内网/镜像）
#   sudo bash deploy-linux.sh --url <直链>          # 从指定地址下载
#   sudo bash deploy-linux.sh --no-tls             # 纯 HTTP（TLS 由前置 CDN 终结时）
#   sudo bash deploy-linux.sh --force-unit         # 覆盖已存在的 systemd 单元
#   sudo bash deploy-linux.sh --no-start           # 只安装不启动
#
# 常用环境变量（也可直接改本文件头部默认值）：
#   REPO=JackCh3n/cloudflare-doh   GitHub 仓库（决定 Releases 下载地址）
#   INSTALL_DIR=/opt/doh-proxy     安装目录
#   LISTEN=:8080                   监听地址（配了证书即自动变 HTTPS）
#   TLS_LISTEN=:8444               额外 TLS 监听（留空则单监听）
#   TLS_CN=dohj.example.com         自签证书 CN/SAN，默认取主机名
#   ALLOWED_HOSTS=...              Host 白名单，逗号分隔；空 = 不限制
#   CACHE_TTL=30                   GET DoH 应答缓存秒数
#   HEALTH_PATH=/healthz           健康检查路径（空 = 关闭）
#   METRICS_PATH=/metrics          指标端点（空 = 关闭）
#
# 行为约定：
#   - 幂等：重复执行安全
#   - 已存在 systemd 单元时默认保留（只换二进制并重启），--force-unit 才覆盖
#   - 证书已存在时不重新生成
# ============================================================
set -euo pipefail

REPO="${REPO:-JackCh3n/cloudflare-doh}"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-/opt/doh-proxy}"
SERVICE_NAME="${SERVICE_NAME:-doh-proxy}"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

LISTEN="${LISTEN:-:8080}"
TLS_LISTEN="${TLS_LISTEN:-}"
ENABLE_TLS="${ENABLE_TLS:-1}"
TLS_CN="${TLS_CN:-$(hostname -f 2>/dev/null || hostname || echo doh-proxy.local)}"
ALLOWED_HOSTS="${ALLOWED_HOSTS:-}"
DEFAULT_UPSTREAM="${DEFAULT_UPSTREAM:-cloudflare}"
CACHE_TTL="${CACHE_TTL:-30}"
CACHE_MAX_TTL="${CACHE_MAX_TTL:-300}"
CACHE_ENTRIES="${CACHE_ENTRIES:-10000}"
HEALTH_PATH="${HEALTH_PATH:-/healthz}"
METRICS_PATH="${METRICS_PATH:-/metrics}"
MAX_INFLIGHT="${MAX_INFLIGHT:-512}"
MAX_CONNS_PER_HOST="${MAX_CONNS_PER_HOST:-64}"
MEM_LIMIT_MB="${MEM_LIMIT_MB:-512}"
CB_THRESHOLD="${CB_THRESHOLD:-5}"
CB_COOLDOWN_SEC="${CB_COOLDOWN_SEC:-30}"
FAILOVER_CHAIN="${FAILOVER_CHAIN:-cloudflare,google,adguard}"

FROM_FILE=""
FROM_URL=""
FORCE_UNIT=0
NO_START=0

log() { printf '[deploy] %s\n' "$*"; }
die() { printf '[error] %s\n' "$*" >&2; exit 1; }

usage() {
	sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
	exit 0
}

while [ $# -gt 0 ]; do
	case "$1" in
	--file)   FROM_FILE="${2:?缺少参数}"; shift 2 ;;
	--url)    FROM_URL="${2:?缺少参数}";  shift 2 ;;
	--no-tls) ENABLE_TLS=0;               shift ;;
	--force-unit) FORCE_UNIT=1;           shift ;;
	--no-start) NO_START=1;               shift ;;
	-h|--help) usage ;;
	*) die "未知参数：$1（--help 查看用法）" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "请用 root 运行（需要写 ${INSTALL_DIR} 与 systemd）"
command -v systemctl >/dev/null || die "未检测到 systemd"
command -v install >/dev/null || die "未检测到 install 命令"

fetch() { # fetch <url> <输出文件>
	if command -v curl >/dev/null; then
		curl -fSL --retry 3 --connect-timeout 15 -o "$2" "$1"
	elif command -v wget >/dev/null; then
		wget -q -O "$2" "$1"
	else
		die "需要 curl 或 wget 之一"
	fi
}

# ---------- 1. 获取二进制 ----------
case "$(uname -m)" in
	x86_64)          ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "不支持的架构：$(uname -m)（仅 amd64/arm64）" ;;
esac
BIN_LOCAL="${INSTALL_DIR}/${SERVICE_NAME}"
BIN_NAME="doh-proxy-linux-${ARCH}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if [ -n "$FROM_FILE" ]; then
	[ -f "$FROM_FILE" ] || die "文件不存在：$FROM_FILE"
	log "使用本地二进制：$FROM_FILE"
	cp "$FROM_FILE" "$tmp/$BIN_NAME"
elif [ -n "$FROM_URL" ]; then
	log "从 $FROM_URL 下载 ..."
	fetch "$FROM_URL" "$tmp/$BIN_NAME"
else
	GH_BASE="https://github.com/${REPO}/releases"
	if [ "$VERSION" = "latest" ]; then
		BIN_URL="${GH_BASE}/latest/download/${BIN_NAME}"
		SUMS_URL="${GH_BASE}/latest/download/SHA256SUMS"
	else
		BIN_URL="${GH_BASE}/download/${VERSION}/${BIN_NAME}"
		SUMS_URL="${GH_BASE}/download/${VERSION}/SHA256SUMS"
	fi
	log "从 GitHub Releases 下载：$BIN_URL"
	fetch "$BIN_URL" "$tmp/$BIN_NAME" || die "下载失败。GitHub 不可达时可用 --url 指向镜像直链，或 --file 安装本地编译产物"
	# 尽力校验（Releases 附带 SHA256SUMS）
	if fetch "$SUMS_URL" "$tmp/SHA256SUMS" 2>/dev/null; then
		expected="$(awk -v n="$BIN_NAME" '$2==n {print $1}' "$tmp/SHA256SUMS")"
		if [ -n "$expected" ]; then
			actual="$(sha256sum "$tmp/$BIN_NAME" | awk '{print $1}')"
			[ "$expected" = "$actual" ] || die "SHA256 校验失败：$expected != $actual"
			log "SHA256 校验通过"
		fi
	else
		log "（未取到 SHA256SUMS，跳过校验）"
	fi
fi

[ -s "$tmp/$BIN_NAME" ] || die "下载的二进制为空"
chmod 0755 "$tmp/$BIN_NAME"

# ---------- 2. 安装 ----------
log "安装到 ${BIN_LOCAL}"
mkdir -p "$INSTALL_DIR" "$INSTALL_DIR/tls"
install -m 0755 "$tmp/$BIN_NAME" "$BIN_LOCAL"

# ---------- 3. TLS ----------
TLS_FLAGS=""
TLS_LISTEN_FLAG=""
if [ "$ENABLE_TLS" = "1" ]; then
	CERT="${INSTALL_DIR}/tls/${TLS_CN}.crt"
	KEY="${INSTALL_DIR}/tls/${TLS_CN}.key"
	if [ -f "$CERT" ] && [ -f "$KEY" ]; then
		log "复用已有证书：$CERT"
	else
		command -v openssl >/dev/null || die "启用 TLS 需要 openssl（或改用 --no-tls / 手动放证书）"
		# SAN：TLS_CN 是 IPv4 就记 IP，否则记 DNS（不能用「含点」判断——IPv4 也含点）
		san=""
		if [[ "$TLS_CN" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
			san="IP:${TLS_CN}"
		else
			san="DNS:${TLS_CN}"
		fi
		primary_ip="$(ip -4 route get 1 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p' || true)"
		if [ -n "$primary_ip" ] && [ "$primary_ip" != "$TLS_CN" ]; then
			san="${san},IP:${primary_ip}"
		fi
		log "生成自签证书（10 年，SAN=${san}）"
		openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
			-keyout "$KEY" -out "$CERT" \
			-subj "/CN=${TLS_CN}" \
			-addext "subjectAltName=${san}" >/dev/null 2>&1
		chmod 600 "$KEY"
		chmod 644 "$CERT"
	fi
	TLS_FLAGS=" -tls-cert ${CERT} -tls-key ${KEY}"
	[ -n "$TLS_LISTEN" ] && TLS_LISTEN_FLAG=" -tls-listen ${TLS_LISTEN}"
fi

# ---------- 4. systemd 单元 ----------
if [ -f "$SERVICE_FILE" ] && [ "$FORCE_UNIT" -eq 0 ]; then
	log "已存在 ${SERVICE_FILE}，保留原配置（只替换二进制）。需要重写请加 --force-unit"
else
	log "写入 ${SERVICE_FILE}"
	cat > "$SERVICE_FILE" <<UNIT
[Unit]
Description=DoH path-forward proxy (Go, cloudflare-doh)
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN_LOCAL} -listen ${LISTEN}${TLS_FLAGS}${TLS_LISTEN_FLAG}
Environment=HEALTH_PATH=${HEALTH_PATH}
Environment=METRICS_PATH=${METRICS_PATH}
Environment=DEFAULT_UPSTREAM=${DEFAULT_UPSTREAM}
Environment=CACHE_TTL=${CACHE_TTL}
Environment=CACHE_MAX_TTL=${CACHE_MAX_TTL}
Environment=CACHE_ENTRIES=${CACHE_ENTRIES}
Environment=MAX_INFLIGHT=${MAX_INFLIGHT}
Environment=MAX_CONNS_PER_HOST=${MAX_CONNS_PER_HOST}
Environment=MEM_LIMIT_MB=${MEM_LIMIT_MB}
Environment=CB_THRESHOLD=${CB_THRESHOLD}
Environment=CB_COOLDOWN_SEC=${CB_COOLDOWN_SEC}
Environment=FAILOVER_CHAIN=${FAILOVER_CHAIN}
UNIT
	if [ -n "$ALLOWED_HOSTS" ]; then
		echo "Environment=ALLOWED_HOSTS=${ALLOWED_HOSTS}" >> "$SERVICE_FILE"
	fi
	cat >> "$SERVICE_FILE" <<'UNIT'

# cgroup 硬护栏：Go 运行时失控也不拖垮整机
MemoryMax=768M
MemoryHigh=640M
CPUQuota=200%
TasksMax=512

Restart=always
RestartSec=2
LimitNOFILE=65535
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
UNIT
fi
systemctl daemon-reload

# ---------- 5. 启动 ----------
if [ "$NO_START" -eq 1 ]; then
	log "已安装（按要求不启动）。手动启动：systemctl enable --now ${SERVICE_NAME}"
	exit 0
fi
systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
systemctl restart "$SERVICE_NAME"
sleep 1
systemctl is-active --quiet "$SERVICE_NAME" || {
	journalctl -u "$SERVICE_NAME" -n 20 --no-pager || true
	die "服务启动失败，见上方日志"
}
log "服务已启动：$(systemctl is-active "$SERVICE_NAME")"

# ---------- 6. 健康检查 ----------
if [ -n "$HEALTH_PATH" ]; then
	port="${LISTEN##*:}"
	scheme="http"
	curl_flags="-s"
	if [ "$ENABLE_TLS" = "1" ] && [ -z "$TLS_LISTEN" ]; then
		scheme="https"
		curl_flags="-sk"
	fi
	ok=0
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		code="$(curl $curl_flags -o /dev/null -w '%{http_code}' --max-time 3 \
			"${scheme}://127.0.0.1:${port}${HEALTH_PATH}" 2>/dev/null || true)"
		[ "$code" = "200" ] && ok=1 && break
		sleep 1
	done
	if [ "$ok" -eq 1 ]; then
		log "健康检查通过：${scheme}://127.0.0.1:${port}${HEALTH_PATH} → 200"
	else
		die "健康检查未通过（服务可能在跑但 ${HEALTH_PATH} 不通），查看：journalctl -u ${SERVICE_NAME} -f"
	fi
fi

echo
log "完成。"
echo "  二进制：${BIN_LOCAL}"
echo "  单元：  ${SERVICE_FILE}（改配置后 systemctl daemon-reload && systemctl restart ${SERVICE_NAME}）"
echo "  监听：  ${LISTEN}${TLS_LISTEN:+ + ${TLS_LISTEN}}（TLS=$([ "$ENABLE_TLS" = 1 ] && echo 开 || echo 关)）"
echo "  日志：  journalctl -u ${SERVICE_NAME} -f"
echo "  指标：  curl ${LISTEN:+127.0.0.1:${LISTEN##*:}}${METRICS_PATH:+${METRICS_PATH}}"
