"""发命令给 ctl_browser.py：python scripts/lab/ctl.py cmd [k=v ...]（值按 JSON 解析，失败当字符串）"""
import json, os, sys, urllib.request

PORT = os.environ.get("CTL_PORT", "8765")
cmd = sys.argv[1]
body = {"cmd": cmd}
for kv in sys.argv[2:]:
    k, _, v = kv.partition("=")
    if k in ("text", "selector", "expr", "url", "name", "contains", "key", "method"):
        body[k] = v
        continue
    try:
        body[k] = json.loads(v)
    except Exception:
        body[k] = v
req = urllib.request.Request("http://127.0.0.1:%s/" % PORT, data=json.dumps(body).encode(), method="POST")
resp = urllib.request.urlopen(req, timeout=body.get("timeout", 120) + 10)
out = resp.read().decode("utf-8", "replace")
print(out[:6000])
