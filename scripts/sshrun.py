#!/usr/bin/env python3
"""密码 SSH 小工具：python sshrun.py "命令"  （服务器信息从环境变量读取）"""
import os
import sys

import paramiko

HOST = os.environ.get("SRV_HOST", "186.241.91.101")
PORT = int(os.environ.get("SRV_PORT", "22"))
USER = os.environ.get("SRV_USER", "root")
PASS = os.environ.get("SRV_PASS", "")


def main() -> int:
    if len(sys.argv) < 2:
        print("用法: sshrun.py <命令>", file=sys.stderr)
        return 2
    cmd = sys.argv[1]
    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    cli.connect(HOST, port=PORT, username=USER, password=PASS, timeout=25,
                look_for_keys=False, allow_agent=False)
    try:
        _, out, err = cli.exec_command(cmd, timeout=120)
        stdout = out.read().decode("utf-8", "replace")
        stderr = err.read().decode("utf-8", "replace")
        rc = out.channel.recv_exit_status()
        if stdout:
            sys.stdout.write(stdout)
        if stderr:
            sys.stderr.write(stderr)
        return rc
    finally:
        cli.close()


if __name__ == "__main__":
    sys.exit(main())
