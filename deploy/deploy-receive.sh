#!/usr/bin/env bash
# Assay 部署接收端（netcup 宿主 nginx 蓝绿版）：由 GitHub Actions 经 SSH 调用。
# 通过 authorized_keys 的 command= 强制命令绑定到部署专用 key——那把 key 只能执行本脚本。
#
# 协议：SSH_ORIGINAL_COMMAND = "deploy <tag> <sha256>"，stdin = docker 镜像 tar.gz（含 assay:<tag>）
# 无迁移：起新色容器 → 健康检查 → 宿主 nginx upstream 切流（nginx -t 通过才平滑 reload）→ 停旧色，零停机
# 有迁移：pg_dump 备份 → 停旧色 → 起新色（启动时自动迁移）→ 切流，短停机
# 失败：新容器不健康则删除之、保持旧色在线（迁移路径下重启旧色）；数据库只 roll-forward
set -euo pipefail

ROOT=/opt/assay
ENV_FILE=$ROOT/env
UPSTREAM_CONF=/etc/nginx/conf.d/assay-upstream.conf   # 全局 http{} 已 include conf.d/*.conf
declare -A PORT=([blue]=18820 [green]=18821)

write_upstream() {
  printf 'upstream assay_backend {\n    server 127.0.0.1:%s;\n}\n' "$1" > "$UPSTREAM_CONF"
}

read -r action tag sum _rest <<< "${SSH_ORIGINAL_COMMAND:-}" || true
if [[ "${action:-}" != "deploy" || ! "${tag:-}" =~ ^v[0-9][A-Za-z0-9.+~-]*$ || ! "${sum:-}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "拒绝：非法部署命令" >&2
  exit 1
fi

echo "[deploy] 接收 $tag"
tmp=$(mktemp -d "$ROOT/tmp.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/image.tar.gz"

echo "$sum  $tmp/image.tar.gz" | sha256sum -c - >/dev/null \
  || { echo "拒绝：sha256 校验失败" >&2; exit 1; }

docker load -qi "$tmp/image.tar.gz" >/dev/null
docker image inspect "assay:$tag" >/dev/null 2>&1 \
  || { echo "拒绝：镜像包内没有 assay:$tag" >&2; exit 1; }

# 判定当前活动颜色与目标颜色
active=""
docker ps --format '{{.Names}}' | grep -qx assay-blue && active=blue
docker ps --format '{{.Names}}' | grep -qx assay-green && active=green
target=blue
[[ "$active" == "blue" ]] && target=green
echo "[deploy] 活动颜色: ${active:-无} → 目标颜色: $target (127.0.0.1:${PORT[$target]})"

pending=$(docker run --rm --network assay-net --env-file "$ENV_FILE" "assay:$tag" migrate pending)
echo "[deploy] 待应用迁移: $pending"

if [[ "$pending" != "0" && -n "$active" ]]; then
  echo "[deploy] 迁移路径：先备份数据库再短停机"
  backup=$ROOT/backups/assay-$(date +%Y%m%d-%H%M%S)-pre-$tag.sql.gz
  docker exec assay-postgres pg_dump -U assay assay | gzip > "$backup"
  echo "[deploy] 备份完成 $backup ($(du -h "$backup" | cut -f1))"
  docker stop "assay-$active" >/dev/null # 新旧 schema 不并跑：先停旧再让新版迁移
fi

docker rm -f "assay-$target" >/dev/null 2>&1 || true
docker run -d --name "assay-$target" --restart unless-stopped \
  --network assay-net -p "127.0.0.1:${PORT[$target]}:8321" \
  --env-file "$ENV_FILE" "assay:$tag" >/dev/null

# 新色容器健康检查：20 秒内 /api/version 必须返回新 tag
ok=""
for _ in $(seq 1 20); do
  sleep 1
  if curl -sf --max-time 2 "http://127.0.0.1:${PORT[$target]}/api/version" | grep -q "\"$tag\""; then
    ok=1
    break
  fi
done
if [[ -z "$ok" ]]; then
  echo "[deploy] 新容器健康检查失败，回退保持旧版" >&2
  docker logs --tail 30 "assay-$target" >&2 || true
  docker rm -f "assay-$target" >/dev/null 2>&1 || true
  [[ "$pending" != "0" && -n "$active" ]] && docker start "assay-$active" >/dev/null
  exit 1
fi

# 切流：写宿主 nginx upstream include，先 nginx -t 校验（保护同机 ~30 个站点）通过才平滑 reload
write_upstream "${PORT[$target]}"
if ! nginx -t >/dev/null 2>&1; then
  echo "[deploy] nginx 配置校验失败，恢复原 upstream" >&2
  [[ -n "$active" ]] && write_upstream "${PORT[$active]}"
  docker rm -f "assay-$target" >/dev/null 2>&1 || true
  [[ "$pending" != "0" && -n "$active" ]] && docker start "assay-$active" >/dev/null
  exit 1
fi
systemctl reload nginx

# 入口验证：经宿主 nginx（本地回环 + SNI）必须已切到新版本
entry_ok=""
for _ in $(seq 1 5); do
  sleep 1
  curl -sk --max-time 3 https://assay.yukihoapi.com/api/version \
    --resolve assay.yukihoapi.com:443:127.0.0.1 | grep -q "\"$tag\"" && { entry_ok=1; break; }
done
[[ -n "$entry_ok" ]] || { echo "[deploy] 入口未切到 $tag，请人工检查" >&2; exit 1; }

# 下线旧色
if [[ -n "$active" && "$active" != "$target" ]]; then
  docker rm -f "assay-$active" >/dev/null 2>&1 || true
fi

echo "[deploy] $tag 上线成功（$([[ "$pending" == "0" ]] && echo 蓝绿零停机 || echo 迁移短停机)，活动颜色: $target）"
# 清理旧镜像：保留最近 3 个 assay:*（docker images 默认按创建时间倒序）
docker images assay --format '{{.Tag}}' | awk 'NR>3' | xargs -r -I{} docker rmi "assay:{}" >/dev/null 2>&1 || true
