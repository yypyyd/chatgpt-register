"""解码并对比 Sentinel 令牌里的 p / t / so 明文：真实浏览器 vs 我们的 Node 假环境。
用法: python pt_decode.py real <tag>            # 解码真实抓包（tag: '' _x4 _x5 _x6 _x7）
      python pt_decode.py ours <dump.jsonl>      # 解码 SENTINEL_DEBUG_DUMP 转储
      python pt_decode.py cmp <tag> <dump.jsonl> # 逐字段对比
"""
import base64, json, os, sys

TMP = os.environ["TEMP"]


def b64(s):
    s = s.strip()
    return base64.b64decode(s + "=" * (-len(s) % 4))


def xor(b, key):
    k = key.encode()
    return bytes(c ^ k[i % len(k)] for i, c in enumerate(b))


def recover(data_b64):
    raw = b64(data_b64)
    for a in range(0, 100):
        for b in range(0, 100):
            key = "%d.%02d" % (a, b)
            if raw[0] ^ ord(key[0]) != ord("{") or raw[1] ^ ord(key[1 % len(key)]) != ord('"'):
                continue
            try:
                txt = xor(raw, key).decode("latin1")
                if txt.endswith("}"):
                    return key, json.loads(txt)
            except Exception:
                pass
    return None, None


def decode_map(data_b64):
    key, obj = recover(data_b64)
    if not obj:
        return None, None
    vals = []
    for rid, v in obj.items():
        try:
            vals.append((rid, xor(b64(v), key).decode("latin1")))
        except Exception:
            vals.append((rid, "<undecodable>"))
    return key, vals


def decode_p(p):
    s = p
    if s.startswith("gAAAAAB") or s.startswith("gAAAAAC"):
        s = s[7:]
    if s.endswith("~S"):
        s = s[:-2]
    try:
        return json.loads(b64(s).decode("utf-8", "replace"))
    except Exception as e:
        return "decode failed: %s" % e


def real_tokens(tag):
    p = os.path.join(TMP, "ctl_net%s.jsonl" % tag)
    out = []
    for line in open(p, encoding="utf-8"):
        r = json.loads(line)
        if r["k"] == "req" and r["m"] == "POST" and ("email-otp/validate" in r["url"] or "create_account" in r["url"]):
            h = r["h"]
            tok = json.loads(h["openai-sentinel-token"])
            so = json.loads(h["openai-sentinel-so-token"])["so"] if "openai-sentinel-so-token" in h else ""
            out.append({"flow": tok["flow"], "token": tok, "so": so})
    return out


def ours_tokens(path):
    out = []
    for line in open(path, encoding="utf-8"):
        r = json.loads(line)
        so = r["so"]; so = json.loads(so)["so"] if so.startswith("{") else so; out.append({"flow": r["flow"], "token": json.loads(r["token"]), "so": so})
    return out


def show(name, rec):
    tok = rec["token"]
    print("=== %s flow=%s" % (name, rec["flow"]))
    p = decode_p(tok["p"])
    print("p elements:")
    if isinstance(p, list):
        for i, v in enumerate(p):
            print("  [%2d] %s" % (i, json.dumps(v, ensure_ascii=False)[:110]))
    else:
        print("  ", p)
    for label, val in (("t", tok.get("t")), ("so", rec["so"])):
        if not val:
            print(label, ": <none>")
            continue
        key, vals = decode_map(val)
        if not vals:
            print(label, ": undecodable raw_len=%d" % len(b64(val)))
            continue
        print("%s key=%s fields=%d" % (label, key, len(vals)))
        for i, (rid, v) in enumerate(vals):
            print("  [%2d] %-7s %s" % (i, rid, json.dumps(v, ensure_ascii=False)[:120]))


if __name__ == "__main__":
    mode = sys.argv[1]
    if mode == "real":
        for i, rec in enumerate(real_tokens(sys.argv[2] if len(sys.argv) > 2 else "")):
            show("real#%d" % i, rec)
    elif mode == "ours":
        for i, rec in enumerate(ours_tokens(sys.argv[2])):
            show("ours#%d" % i, rec)

