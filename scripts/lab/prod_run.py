"""在服务器上通过线上服务自身的 HTTP 接口走一遍"生产 1 个账号"的完整路径，用于部署后端到端验证。

用法(在服务器上执行): python3 prod_run.py <admin密码> [count]
只输出脱敏后的进度日志，不输出 token / 密码 / 邮箱明文。
"""
import json
import sys
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:9010"


def call(method, path, body=None, token=None, timeout=30):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"{}")
        except Exception:  # noqa: BLE001
            return e.code, {}


def main() -> None:
    pw = sys.argv[1]
    count = int(sys.argv[2]) if len(sys.argv) > 2 else 1
    code, out = call("POST", "/api/login", {"username": "admin", "password": pw})
    if code != 200 or "token" not in out:
        print("login failed:", code, out.get("error", ""))
        raise SystemExit(2)
    print("login ok (username=%s)" % out.get("username"))
    token = out["token"]

    code, out = call("POST", "/api/produce", {"count": count}, token)
    print("produce start:", code, out)
    if code != 200:
        raise SystemExit(3)

    seen = 0
    deadline = time.time() + 600
    while time.time() < deadline:
        time.sleep(4)
        code, st = call("GET", "/api/produce/status", token=token)
        if code != 200:
            print("status http", code)
            continue
        logs = st.get("logs") or []
        for line in logs[seen:]:
            print("  |", line[:200])
        seen = len(logs)
        print(
            "status: running=%s registered=%s failed=%s running_num=%s msg=%s"
            % (st.get("running"), st.get("registered"), st.get("failed"), st.get("running_num"), st.get("message"))
        )
        if not st.get("running"):
            break
    else:
        print("timeout waiting for production; asking it to stop")
        call("POST", "/api/produce/stop", {}, token)


if __name__ == "__main__":
    main()
