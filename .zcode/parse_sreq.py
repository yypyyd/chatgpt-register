import json
n=0
for line in open("/tmp/oai-probe/requests.jsonl", errors="ignore"):
    r=json.loads(line)
    u=str(r.get("url") or "")
    if "sentinel/req" not in u:
        continue
    n+=1
    print("====", n, r.get("kind"), r.get("method"), r.get("status"))
    body=r.get("body") or ""
    print("body", body[:800])
    if r.get("kind")=="request":
        h=r.get("headers") or {}
        for k in ("content-type","origin","referer","user-agent"):
            for hk,hv in h.items():
                if hk.lower()==k:
                    print("H", k, hv[:120])
print("total", n)
