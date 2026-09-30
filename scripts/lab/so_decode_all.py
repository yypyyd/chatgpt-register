"""解码所有真实抓包里的 openai-sentinel-so-token。
so = base64( XOR( JSON{regId: base64( XOR(value, K) )}, K ) )，K 是形如 "dd.dd" 的 5 字符 key，暴力恢复。
"""
import base64, itertools, json, os, sys

TMP = os.environ["TEMP"]


def b64(s):
    return base64.b64decode(s + "=" * (-len(s) % 4))


def xor(b, key):
    k = key.encode()
    return bytes(c ^ k[i % len(k)] for i, c in enumerate(b))


def recover(so):
    raw = b64(so)
    for a in range(0, 100):
        for b in range(0, 100):
            key = "%02d.%02d" % (a, b)
            try:
                txt = xor(raw, key).decode("latin1")
                if txt.startswith('{"') and txt.endswith('}'):
                    return key, json.loads(txt)
            except Exception:
                pass
    # 允许一位小数/三位
    for key in ("%d.%02d" % (a, b) for a in range(0, 10) for b in range(0, 100)):
        try:
            txt = xor(raw, key).decode("latin1")
            if txt.startswith('{"') and txt.endswith('}'):
                return key, json.loads(txt)
        except Exception:
            pass
    return None, None


def collect(tag):
    p = os.path.join(TMP, "ctl_net%s.jsonl" % tag)
    if not os.path.exists(p):
        return {}
    out = {}
    for line in open(p, encoding="utf-8"):
        r = json.loads(line)
        if r["k"] == "req" and r["m"] == "POST" and ("email-otp/validate" in r["url"] or "create_account" in r["url"]):
            h = r["h"]
            so = h.get("openai-sentinel-so-token")
            if so:
                out["validate" if "validate" in r["url"] else "create"] = json.loads(so)["so"]
    return out


tags = [("B1", ""), ("X4", "_x4"), ("X5", "_x5"), ("X6", "_x6"), ("X7", "_x7")]
table = {}
for name, tag in tags:
    for kind, so in collect(tag).items():
        key, obj = recover(so)
        if not obj:
            print(name, kind, "decode failed")
            continue
        vals = {rid: xor(b64(v), key).decode("latin1") for rid, v in obj.items()}
        table[(name, kind)] = (key, vals)

# 以 X7 validate 的字段顺序为基准（不同 flow 的寄存器 id 不同，这里只展示各自的值列表）
for kind in ("validate", "create"):
    print("\n=== %s ===" % kind)
    rows = [(n, table[(n, kind)]) for n, _ in tags if (n, kind) in table]
    for n, (key, vals) in rows:
        print("%-3s key=%s n=%d" % (n, key, len(vals)))
        print("     ", " | ".join(("%s" % v)[:14] for v in vals.values()))
