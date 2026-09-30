import json, os, re, sys
p = os.path.join(os.environ["TEMP"], "ctl_net.jsonl")
recs = [json.loads(l) for l in open(p, encoding="utf-8")]
kw = sys.argv[1]
limit = int(sys.argv[2]) if len(sys.argv) > 2 else 3000
show_h = len(sys.argv) > 3 and sys.argv[3] == "h"
reqs = {r["id"]: r for r in recs if r["k"] == "req"}
resp = {r["id"]: r for r in recs if r["k"] == "resp"}
for r in recs:
    if r["k"] == "body" and kw in r["url"]:
        rq = reqs.get(r["id"], {})
        rs = resp.get(r["id"], {})
        print("=" * 100)
        print(rq.get("m"), re.sub(r"(code|state)=[^&]+", r"\1=***", r["url"])[:200], "->", rs.get("status"))
        if show_h:
            print("REQ HEADERS:", json.dumps(rq.get("h"), ensure_ascii=False)[:1800])
            print("RESP HEADERS:", json.dumps(rs.get("h"), ensure_ascii=False)[:1800])
        if rq.get("body"):
            print("REQ BODY:", rq["body"][:limit])
        b = r["body"]
        b = re.sub(r'"(accessToken|access_token|id_token|refresh_token)"\s*:\s*"[^"]{20,}"', r'"\1":"<redacted>"', b)
        print("BODY:", b[:limit])
