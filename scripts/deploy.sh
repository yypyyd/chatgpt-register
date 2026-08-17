#!/usr/bin/env bash
# 一键部署脚本：全新服务器可直接用——自动建工作目录、写 systemd 服务、装 Chrome、建 venv 补依赖，
# 再装二进制和 scripts/，重启服务并校验，校验失败自动回滚。
# 用法：
#   scripts/deploy.sh root@1.2.3.4 [root@5.6.7.8 ...]
# 常用环境变量：
#   SSH_PASSWORD       ssh/scp 密码（设置后用 sshpass，未设置则用密钥登录）
#   SSH_PORT           ssh 端口，默认 22
#   BIN               本地 Linux 二进制路径，默认 ./chatgpt-register-linux；不存在时用 go 交叉编译
#   SCRIPTS_SRC       本地脚本目录，默认 ./scripts
#   SERVICE            systemd 服务名，默认 chatgpt-register
#   REMOTE_BIN        远端二进制路径，默认 /usr/local/bin/chatgpt-register
#   SCRIPTS_DST       远端脚本目录，默认 /usr/local/share/chatgpt-register/scripts
#   WORKDIR            远端工作目录（数据库所在），默认 /opt/chatgpt-register
#   ADDR               服务监听地址，默认 127.0.0.1:9000（只在首次写 service 文件时生效）
#   VENV              Python 虚拟环境，默认 /opt/cloakbrowser-venv
#   FORCE_VENV=1      强制重装 requirements.txt 里的依赖
#   SKIP_VENV=1       跳过 Python 依赖检查
#   SKIP_CHROME=1     跳过 Chrome 检查/安装
#   DB_SRC             本地数据库文件路径；设置后在远端没有数据库时装为初始库（绝不覆盖已有库）
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${BIN:-$REPO_ROOT/chatgpt-register-linux}"
SCRIPTS_SRC="${SCRIPTS_SRC:-$REPO_ROOT/scripts}"
SERVICE="${SERVICE:-chatgpt-register}"
REMOTE_BIN="${REMOTE_BIN:-/usr/local/bin/chatgpt-register}"
SCRIPTS_DST="${SCRIPTS_DST:-/usr/local/share/chatgpt-register/scripts}"
WORKDIR="${WORKDIR:-/opt/chatgpt-register}"
ADDR="${ADDR:-127.0.0.1:9000}"
VENV="${VENV:-/opt/cloakbrowser-venv}"
SSH_PORT="${SSH_PORT:-22}"
FORCE_VENV="${FORCE_VENV:-0}"
SKIP_VENV="${SKIP_VENV:-0}"
SKIP_CHROME="${SKIP_CHROME:-0}"
DB_SRC="${DB_SRC:-}"

[ $# -ge 1 ] || { echo "用法: scripts/deploy.sh root@1.2.3.4 [root@5.6.7.8 ...]" >&2; exit 2; }
if [ -n "$DB_SRC" ] && [ ! -f "$DB_SRC" ]; then echo "找不到数据库文件 $DB_SRC" >&2; exit 2; fi

# 有密码走 sshpass，没密码走密钥
SSH=(ssh -o StrictHostKeyChecking=no -o ConnectTimeout=20 -p "$SSH_PORT")
SCP=(scp -o StrictHostKeyChecking=no -o ConnectTimeout=20 -P "$SSH_PORT")
if [ -n "${SSH_PASSWORD:-}" ]; then
  command -v sshpass >/dev/null || { echo "缺少 sshpass，请先安装（apt-get install sshpass）" >&2; exit 2; }
  SSH=(sshpass -p "$SSH_PASSWORD" "${SSH[@]}")
  SCP=(sshpass -p "$SSH_PASSWORD" "${SCP[@]}")
fi

# 没有现成二进制就交叉编译一个
if [ ! -f "$BIN" ]; then
  command -v go >/dev/null || { echo "找不到 $BIN，也没有 go 可用于编译" >&2; exit 2; }
  echo "== 未找到 $BIN，开始交叉编译 Linux 二进制"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BIN" .)
fi
[ -d "$SCRIPTS_SRC" ] || { echo "找不到脚本目录 $SCRIPTS_SRC" >&2; exit 2; }

BIN_MD5="$(md5sum "$BIN" | awk '{print $1}')"
echo "== 二进制 $BIN ($BIN_MD5)"
echo "== 脚本目录 $SCRIPTS_SRC: $(ls "$SCRIPTS_SRC" | tr '\n' ' ')"

# 先把文件传到远端暂存目录
stage_files() {
  "${SSH[@]}" "$1" 'rm -rf /tmp/cgr-deploy && mkdir -p /tmp/cgr-deploy/scripts'
  "${SCP[@]}" "$BIN" "$1":/tmp/cgr-deploy/bin
  "${SCP[@]}" "$SCRIPTS_SRC"/* "$1":/tmp/cgr-deploy/scripts/
  if [ -n "$DB_SRC" ]; then
    "${SCP[@]}" "$DB_SRC" "$1":/tmp/cgr-deploy/adskull.db
  fi
}

# 远端安装：首次自动初始化（工作目录/service 文件/Chrome/venv），备份旧二进制 -> 装新二进制和脚本
# -> 补依赖 -> 重启 -> 校验，校验失败自动回滚
remote_install() {
  "${SSH[@]}" "$1" \
    "SERVICE='$SERVICE' REMOTE_BIN='$REMOTE_BIN' SCRIPTS_DST='$SCRIPTS_DST' WORKDIR='$WORKDIR' ADDR='$ADDR' VENV='$VENV' BIN_MD5='$BIN_MD5' FORCE_VENV='$FORCE_VENV' SKIP_VENV='$SKIP_VENV' SKIP_CHROME='$SKIP_CHROME' bash -s" <<'REMOTE'
set -euo pipefail
STAGE=/tmp/cgr-deploy
TS=$(date -u +%Y%m%d%H%M%S)

[ "$(md5sum "$STAGE/bin" | awk '{print $1}')" = "$BIN_MD5" ] || { echo "上传的二进制校验失败"; exit 1; }

# 工作目录（数据库所在）
mkdir -p "$WORKDIR"

# systemd 服务文件：已有就沿用其中的 ADDR，没有就写一份并 enable
UNIT="/etc/systemd/system/${SERVICE}.service"
if [ -f "$UNIT" ]; then
  EXIST_ADDR="$(sed -n 's/^Environment=ADDR=//p' "$UNIT" | head -1)"
  [ -n "$EXIST_ADDR" ] && ADDR="$EXIST_ADDR"
fi
if [ ! -f "$UNIT" ]; then
  echo "== 首次部署，写入 $UNIT"
  cat > "$UNIT" <<UNITEOF
[Unit]
Description=ChatGPT Register Management Console
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=${WORKDIR}
Environment=ADDR=${ADDR}
Environment=HOME=/root
ExecStart=${REMOTE_BIN}
Restart=on-failure
RestartSec=3
TimeoutStopSec=30
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNITEOF
  systemctl daemon-reload
  systemctl enable "$SERVICE" >/dev/null 2>&1 || true
fi

# Chrome：turnstile/cloakbrowser 依赖，没有就装
if [ "$SKIP_CHROME" != "1" ] && ! command -v google-chrome >/dev/null 2>&1; then
  echo "== 未检测到 google-chrome，开始安装"
  export DEBIAN_FRONTEND=noninteractive
  curl -fsSL -o /tmp/chrome.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
  apt-get update -qq || true
  apt-get install -y -qq /tmp/chrome.deb
  rm -f /tmp/chrome.deb
  command -v google-chrome >/dev/null || { echo "Chrome 安装失败"; exit 1; }
fi

# 初始数据库：只在远端不存在时装（绝不覆盖已有库）
if [ -f "$STAGE/adskull.db" ]; then
  if [ -f "$WORKDIR/adskull.db" ]; then
    echo "== 远端已有数据库，跳过 DB_SRC"
  else
    install -m 0600 "$STAGE/adskull.db" "$WORKDIR/adskull.db"
    echo "== 已装入初始数据库"
  fi
fi

BACKUP=""
if [ -f "$REMOTE_BIN" ]; then
  BACKUP="${REMOTE_BIN}.bak.${TS}"
  cp -a "$REMOTE_BIN" "$BACKUP"
  # 只保留最近 3 份备份
  ls -1t "${REMOTE_BIN}".bak.* 2>/dev/null | tail -n +4 | xargs -r rm -f
fi

install -D -m 0755 "$STAGE/bin" "$REMOTE_BIN"
mkdir -p "$SCRIPTS_DST"
for f in "$STAGE"/scripts/*; do
  [ -f "$f" ] || continue
  case "$f" in
    *.py) install -m 0755 "$f" "$SCRIPTS_DST/" ;;
    *)    install -m 0644 "$f" "$SCRIPTS_DST/" ;;
  esac
done

# Python 依赖：venv 不存在就建，FORCE_VENV=1 强制重装
if [ "$SKIP_VENV" != "1" ]; then
  if [ ! -x "$VENV/bin/python" ]; then
    echo "== venv 不存在，创建 $VENV"
    python3 -m venv "$VENV" 2>/dev/null || {
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq || true
      apt-get install -y -qq python3-venv
      python3 -m venv "$VENV"
    }
    FORCE_VENV=1
  fi
  if [ "$FORCE_VENV" = "1" ] && [ -f "$SCRIPTS_DST/requirements.txt" ]; then
    "$VENV/bin/pip" install -q --upgrade pip
    "$VENV/bin/pip" install -q -r "$SCRIPTS_DST/requirements.txt"
  fi
  "$VENV/bin/python" -c 'import importlib.util,sys
missing=[m for m in ("cloakbrowser",) if importlib.util.find_spec(m) is None]
sys.exit(1 if missing else 0)' || {
    if [ -f "$SCRIPTS_DST/requirements.txt" ]; then
      echo "== 依赖缺失，按 requirements.txt 补装"
      "$VENV/bin/pip" install -q -r "$SCRIPTS_DST/requirements.txt"
    fi
  }
fi

systemctl restart "$SERVICE"

# 校验：服务 active 且监听地址能响应，失败则回滚
ok=0
for i in $(seq 1 15); do
  sleep 2
  systemctl is-active --quiet "$SERVICE" || continue
  if curl -sf -m 3 "http://${ADDR}/" >/dev/null 2>&1; then
    ok=1; break
  fi
done

if [ "$ok" != "1" ]; then
  echo "!! 校验失败"
  if [ -n "$BACKUP" ]; then
    echo "!! 回滚到 $BACKUP"
    cp -a "$BACKUP" "$REMOTE_BIN"
    systemctl restart "$SERVICE" || true
  fi
  exit 1
fi

rm -rf "$STAGE"
echo "== 部署完成：$(md5sum "$REMOTE_BIN" | awk '{print $1}') $REMOTE_BIN"
REMOTE
}

FAILED=()
for HOST in "$@"; do
  echo "==== 部署到 $HOST ===="
  if stage_files "$HOST" && remote_install "$HOST"; then
    echo "==== $HOST 部署成功 ===="
  else
    echo "==== $HOST 部署失败 ====" >&2
    FAILED+=("$HOST")
  fi
done

if [ "${#FAILED[@]}" -gt 0 ]; then
  echo "失败的主机: ${FAILED[*]}" >&2
  exit 1
fi
echo "全部部署完成"
