#!/usr/bin/env bash
# 影子轨 JSONL 真相同步（S-铁5：真相在旧系统 data/，影子栈 ingest 读 data/shadow/）。
#
# 节奏（D-012，2026-08-20 owner 裁决）：
#   - 新单实时主通道 = 旧系统埋点 / 飞书事件订阅 hook（T3 调研定，未落地）；
#   - JSONL / 列表 = 每日对账兜底 → 本脚本按「每日定时」同步（默认 00:30），
#     摄入（shadow-ingest）周期 5m、幂等，文件到位后自动吃掉。
#
# 用法：
#   ./scripts/shadow-sync.sh                 # 跑一轮同步（每日 cron 调用）
#   ./scripts/shadow-sync.sh --install       # 生产一次性安装：SSH 密钥 + 每日 cron（幂等）
#   ./scripts/shadow-sync.sh --uninstall     # 移除每日 cron
#   ./scripts/shadow-sync.sh --cron          # 显示当前 cron 条目
#
# 环境变量（生产部署按实际覆盖）：
#   SHADOW_SYNC_SRC_HOST  旧系统 SSH 地址（默认 root@121.43.58.1 = 测试）
#   SHADOW_SYNC_SRC_DIR   旧系统 JSONL 目录（默认 /root/tripartite-approval-v2/data）
#   SHADOW_SYNC_DEST      影子栈数据目录（默认 /opt/pilot_v3/data/shadow）
#   SHADOW_SYNC_FILES     需同步的 JSONL（默认 5 份）
#   SHADOW_SYNC_SSH_KEY   SSH 私钥路径（默认 ~/.ssh/id_ed25519）
#   SHADOW_SYNC_CRON      cron 表达式（默认 "30 0 * * *" = 每日 00:30 对账兜底）
set -euo pipefail

SRC_HOST="${SHADOW_SYNC_SRC_HOST:-root@121.43.58.1}"
SRC_DIR="${SHADOW_SYNC_SRC_DIR:-/root/tripartite-approval-v2/data}"
DEST="${SHADOW_SYNC_DEST:-/opt/pilot_v3/data/shadow}"
FILES="${SHADOW_SYNC_FILES:-orders.jsonl order_events.jsonl stock_prepare_events.jsonl shipment_events.jsonl invoice_events.jsonl}"
SSH_KEY="${SHADOW_SYNC_SSH_KEY:-$HOME/.ssh/id_ed25519}"
CRON_SCHEDULE="${SHADOW_SYNC_CRON:-30 0 * * *}"
LOG_FILE="${SHADOW_SYNC_LOG:-/var/log/shadow-sync.log}"

EXTRA_SSH=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)
[ -n "$SSH_KEY" ] && [ -f "$SSH_KEY" ] && EXTRA_SSH+=(-i "$SSH_KEY")

# 完整性口径（D-012：丢单/错序 = 采集故障并告警）
# 源端 sha256（certutil）：注意 Windows CRLF 行尾 → 先去 \r，再取 64 位十六进制。
remote_sha256() {
  ssh "${EXTRA_SSH[@]}" "$SRC_HOST" "certutil -hashfile $SRC_DIR/$1 SHA256" 2>/dev/null \
    | tr -d '\r' | grep -iE '^[0-9a-f]{64}$' | tr 'A-F' 'a-f' | head -1
}
local_sha256() {
  sha256sum "$1" 2>/dev/null | awk '{print tolower($1)}'
}

install_cron() {
  # 把显式设置的环境变量烤进 cron 行，避免 cron 运行时环境丢失 → 退回默认（测试）源
  local envs=""
  [ -n "${SHADOW_SYNC_SRC_HOST:-}" ] && envs+=" SHADOW_SYNC_SRC_HOST='$SHADOW_SYNC_SRC_HOST'"
  [ -n "${SHADOW_SYNC_SRC_DIR:-}" ]  && envs+=" SHADOW_SYNC_SRC_DIR='$SHADOW_SYNC_SRC_DIR'"
  [ -n "${SHADOW_SYNC_DEST:-}" ]     && envs+=" SHADOW_SYNC_DEST='$SHADOW_SYNC_DEST'"
  [ -n "${SHADOW_SYNC_FILES:-}" ]    && envs+=" SHADOW_SYNC_FILES='$SHADOW_SYNC_FILES'"
  [ -n "${SHADOW_SYNC_SSH_KEY:-}" ]  && envs+=" SHADOW_SYNC_SSH_KEY='$SHADOW_SYNC_SSH_KEY'"
  local line="$CRON_SCHEDULE$envs $SCRIPT_ABS >> $LOG_FILE 2>&1"
  local content
  content="$(crontab -l 2>/dev/null | grep -v 'shadow-sync.sh' || true)
$line"
  printf '%s\n' "$content" | crontab -
  echo "[shadow-sync] cron installed: $line"
}

uninstall_cron() {
  local content
  content="$(crontab -l 2>/dev/null | grep -v 'shadow-sync.sh' || true)"
  printf '%s\n' "$content" | crontab - || true
  echo "[shadow-sync] cron removed"
}

SCRIPT_ABS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

case "${1:-}" in
  --cron)
    echo "[shadow-sync] schedule=$CRON_SCHEDULE script=$SCRIPT_ABS log=$LOG_FILE"
    crontab -l 2>/dev/null | grep 'shadow-sync.sh' || echo "[shadow-sync] (未安装 cron)"
    exit 0
    ;;
  --uninstall)
    uninstall_cron
    exit 0
    ;;
  --install)
    # 1) SSH 密钥（幂等）
    if [ ! -f "$SSH_KEY" ]; then
      ssh-keygen -t ed25519 -N '' -f "$SSH_KEY" -C 'b2b_shadow_sync' >/dev/null 2>&1
      echo "[shadow-sync] 已生成密钥 $SSH_KEY.pub："
      cat "$SSH_KEY.pub"
      echo "   请把上面这行追加到旧系统 root 的 ~/.ssh/authorized_keys，再重试本命令完成安装。"
      exit 1
    fi
    # 2) 连通性自检
    if ! scp "${EXTRA_SSH[@]}" -q "$SRC_HOST:$SRC_DIR/${FILES%% *}" /dev/null 2>/dev/null; then
      echo "[shadow-sync] 到 $SRC_HOST 连通失败：确认公钥已加入旧系统 authorized_keys。"
      exit 1
    fi
    echo "[shadow-sync] SSH 连通 OK：$SRC_HOST"
    install_cron
    echo "[shadow-sync] 安装完成；可立即手动跑一轮：$SCRIPT_ABS"
    exit 0
    ;;
esac

# ── 同步（默认模式）──
# 完整性口径（D-012）：丢单/错序 = 采集故障并告警 → 每文件拉取后 sha256 逐字节校验，不一致即失败。
trap 'rm -f "$DEST"/*.tmp 2>/dev/null || true' EXIT
mkdir -p "$DEST"
FAILED=0
for f in $FILES; do
  # 先拉到 .tmp 再原子 mv：避免 ingest 读到半写文件（checksum 幂等按完整文件算）
  if ! scp "${EXTRA_SSH[@]}" -q "$SRC_HOST:$SRC_DIR/$f" "$DEST/$f.tmp"; then
    echo "[shadow-sync] $(date '+%F %T') FAIL scp 拉取失败: $f"
    FAILED=1
    continue
  fi
  rh=$(remote_sha256 "$f")
  lh=$(local_sha256 "$DEST/$f.tmp")
  if [ -n "$rh" ] && [ "$rh" = "$lh" ]; then
    mv "$DEST/$f.tmp" "$DEST/$f"
    echo "[shadow-sync] $(date '+%F %T') OK   $f sha256=$lh"
  else
    echo "[shadow-sync] $(date '+%F %T') FAIL sha256 不一致(采集故障): $f remote=$rh local=$lh"
    rm -f "$DEST/$f.tmp"
    FAILED=1
  fi
done

if [ "$FAILED" -eq 0 ]; then
  rm -f "$DEST/.sync-failed"
  echo "[shadow-sync] $(date '+%F %T') synced ${FILES} -> ${DEST} 5/5 全部一致 ($(wc -l "$DEST"/*.jsonl 2>/dev/null | tail -1 | awk '{print $1}') total lines)"
else
  touch "$DEST/.sync-failed"
  echo "[shadow-sync] $(date '+%F %T') 同步完成但存在失败（已写 $DEST/.sync-failed 告警标记）"
  exit 1
fi
