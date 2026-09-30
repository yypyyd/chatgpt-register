import json, os, re, sys, time
p = os.path.join(os.environ["TEMP"], "ctl_net.jsonl")
recs = [json.loads(l) for l in open(p, encoding="utf-8")]
t0 = recs[0]["ts"]
mode = sys.argv[1] if len(sys.argv) > 1 else "flow"
since_kw = sys.argv[2] if len(sys.argv) > 2 else ""
skip_static = re.compile(r"\.(js|css|png|svg|woff2?|ico|jpg|webp|gif|map)(\?|$)|/cdn/assets/|oaistatic|fonts\.")
reqs = [r for r in recs if r["k"] == "req"]
resp = {r["id"]: r for r in recs if r["k"] == "resp"}
bodies = {r["id"]: r for r in recs if r["k"] == "body"}
started = not since_kw
for r in reqs:
    u = r["url"]
    if skip_static.search(u):
        continue
    if since_kw and since_kw in u:
        started = True
    if not started:
        continue
    rs = resp.get(r["id"], {})
    short = re.sub(r"\?.*", "?..", u)[:110]
    print("%7.1fs %-6s %s -> %s" % (r["ts"] - t0, r["m"], short, rs.get("status")))
