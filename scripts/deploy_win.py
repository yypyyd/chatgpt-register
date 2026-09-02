#!/usr/bin/env python3
"""Windows 下的一键部署：等价于 scripts/deploy.sh 的升级路径（paramiko 实现）。

用法：python scripts/deploy_win.py <host> <user> <password>
上传本地 chatgpt-register-linux 与 scripts/，备份旧二进制后替换、重启服务并校验，
校验失败自动回滚。远端必须已完成首次部署（service 文件已存在）。
"""
import hashlib
import os
import sys
import time

import paramiko

SERVICE = "chatgpt-register"
REMOTE_BIN = "/usr/local/bin/chatgpt-register"
SCRIPTS_DST = "/usr/local/share/chatgpt-register/scripts"
STAGE = "/tmp/cgr-deploy"

REMOTE_INSTALL = r"""
set -euo pipefail
STAGE=/tmp/cgr-deploy
SERVICE=__SERVICE__
REMOTE_BIN=__REMOTE_BIN__
SCRIPTS_DST=__SCRIPTS_DST__
BIN_MD5=__BIN_MD5__
TS=$(date -u +%Y%m%d%H%M%S)

[ "$(md5sum "$STAGE/bin" | awk '{print $1}')" = "$BIN_MD5" ] || { echo "!! 上传的二进制校验失败"; exit 1; }

UNIT="/etc/systemd/system/${SERVICE}.service"
[ -f "$UNIT" ] || { echo "!! 远端没有 $UNIT，请先用 deploy.sh 做首次部署"; exit 1; }
ADDR="$(sed -n 's/^Environment=ADDR=//p' "$UNIT" | head -1)"
[ -n "$ADDR" ] || ADDR=127.0.0.1:9000

BACKUP=""
if [ -f "$REMOTE_BIN" ]; then
  BACKUP="${REMOTE_BIN}.bak.${TS}"
  cp -a "$REMOTE_BIN" "$BACKUP"
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

systemctl restart "$SERVICE"

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
  systemctl status "$SERVICE" --no-pager -l | tail -20 || true
  if [ -n "$BACKUP" ]; then
    echo "!! 回滚到 $BACKUP"
    cp -a "$BACKUP" "$REMOTE_BIN"
    systemctl restart "$SERVICE" || true
  fi
  exit 1
fi

rm -rf "$STAGE"
echo "== 部署完成：$(md5sum "$REMOTE_BIN" | awk '{print $1}') $REMOTE_BIN (ADDR=$ADDR)"
"""


def md5_file(path: str) -> str:
    h = hashlib.md5()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def run(cli: paramiko.SSHClient, cmd: str, timeout: int = 300) -> int:
    _, out, err = cli.exec_command(cmd, timeout=timeout)
    stdout = out.read().decode("utf-8", "replace")
    stderr = err.read().decode("utf-8", "replace")
    rc = out.channel.recv_exit_status()
    if stdout:
        sys.stdout.write(stdout)
    if stderr:
        sys.stderr.write(stderr)
    return rc


def main() -> int:
    if len(sys.argv) != 4:
        print("用法: deploy_win.py <host> <user> <password>", file=sys.stderr)
        return 2
    host, user, password = sys.argv[1], sys.argv[2], sys.argv[3]

    repo = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    bin_path = os.path.join(repo, "chatgpt-register-linux")
    scripts_dir = os.path.join(repo, "scripts")
    if not os.path.isfile(bin_path):
        print(f"找不到 {bin_path}，先交叉编译: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o chatgpt-register-linux .",
              file=sys.stderr)
        return 2

    bin_md5 = md5_file(bin_path)
    print(f"== 二进制 {bin_path} ({bin_md5}, {os.path.getsize(bin_path) / 1e6:.1f} MB)")

    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    print(f"== 连接 {user}@{host} ...")
    cli.connect(host, 22, user, password, timeout=30, banner_timeout=90,
                auth_timeout=120, look_for_keys=False, allow_agent=False)
    try:
        run(cli, f"rm -rf {STAGE} && mkdir -p {STAGE}/scripts")

        sftp = cli.open_sftp()
        print("== 上传二进制（56MB，可能需要几分钟）...")
        start = time.time()
        sftp.put(bin_path, f"{STAGE}/bin")
        print(f"== 二进制上传完成，用时 {time.time() - start:.0f}s")
        for name in os.listdir(scripts_dir):
            src = os.path.join(scripts_dir, name)
            if os.path.isfile(src):
                sftp.put(src, f"{STAGE}/scripts/{name}")
        print("== scripts/ 上传完成")
        sftp.close()

        script = (REMOTE_INSTALL
                  .replace("__SERVICE__", SERVICE)
                  .replace("__REMOTE_BIN__", REMOTE_BIN)
                  .replace("__SCRIPTS_DST__", SCRIPTS_DST)
                  .replace("__BIN_MD5__", bin_md5))
        print("== 远端安装/重启/校验 ...")
        rc = run(cli, f"bash -s <<'REMOTE'\n{script}\nREMOTE", timeout=600)
        if rc != 0:
            print(f"!! 远端安装失败 rc={rc}", file=sys.stderr)
            return 1
        print("== 全部完成")
        return 0
    finally:
        cli.close()


if __name__ == "__main__":
    sys.exit(main())
