import json, base64
for line in open("/tmp/oai-probe/requests.jsonl", errors="ignore"):
    r = json.loads(line)
    u = str(r.get("url") or "")
    if r.get("kind") == "request" and "create_account" in u:
        h = r.get("headers") or {}
        tok = h.get("openai-sentinel-token") or ""
        so = h.get("openai-sentinel-so-token") or ""
        d = json.loads(tok)
        print("sentinel keys", list(d))
        for k in d:
            v = d[k]
            sv = str(v)
            print(k, type(v).__name__, sv[:120], "len", len(sv))
        soj = json.loads(so) if so.startswith("{") else {}
        print("so keys", list(soj))
        for k in soj:
            print("so", k, str(soj[k])[:120])
        print("nav", h.get("x-openai-document-navigation-id"))
        print("flowinv", h.get("x-access-flow-invocation-id"))
        p = d.get("p") or ""
        print("p[:12]", p[:12])
        try:
            pad = p + "=" * ((4 - len(p) % 4) % 4)
            b = base64.b64decode(pad)
            print("b64", len(b), b[:100])
        except Exception as e:
            print("b64 err", e)
        break
print("==== sentinel urls ====")
for line in open("/tmp/oai-probe/requests.jsonl", errors="ignore"):
    r = json.loads(line)
    u = str(r.get("url") or "")
    if "sentinel" in u:
        print(r.get("kind"), r.get("method"), r.get("status"), u[:180], (r.get("body") or "")[:180])
