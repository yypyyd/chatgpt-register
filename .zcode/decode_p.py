import json
p=None
d=None
for line in open("/tmp/oai-probe/requests.jsonl", errors="ignore"):
    r=json.loads(line)
    u=str(r.get("url") or "")
    if r.get("kind")=="request" and "create_account" in u:
        d=json.loads((r.get("headers") or {}).get("openai-sentinel-token") or "{}")
        p=d.get("p")
        break
print("keys", list(d))
print("id", d.get("id"))
print("flow", d.get("flow"))
print("P")
print(p)
print("T")
print((d.get("t") or "")[:200], "...")
print("C")
print((d.get("c") or "")[:200], "...")
