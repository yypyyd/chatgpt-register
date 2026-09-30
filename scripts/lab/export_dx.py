"""把抓包里的 sentinel/req 请求(p)与响应(so.collector_dx/snapshot_dx/turnstile.dx)成对导出成 JSON，供 VM 实验台使用。"""
import json, os, sys

tag = sys.argv[1] if len(sys.argv) > 1 else "_x7"
out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(os.environ["TEMP"], "dx_pairs%s.json" % tag)
p = os.path.join(os.environ["TEMP"], "ctl_net%s.jsonl" % tag)
recs = [json.loads(l) for l in open(p, encoding="utf-8")]
reqs = {r["id"]: r for r in recs if r["k"] == "req"}
pairs = []
for b in recs:
    if b["k"] == "body" and "sentinel/req" in b["url"]:
        rq = reqs.get(b["id"])
        if not rq:
            continue
        try:
            preq = json.loads(rq["body"])
            resp = json.loads(b["body"])
        except Exception as e:
            print("skip", e)
            continue
        so = resp.get("so") or {}
        pairs.append({
            "ts": rq["ts"], "flow": preq.get("flow"), "p": preq["p"], "id": preq.get("id"),
            "token": resp.get("token"), "collector_dx": so.get("collector_dx"), "snapshot_dx": so.get("snapshot_dx"),
            "dx": (resp.get("turnstile") or {}).get("dx"), "pow": resp.get("proofofwork"),
        })
json.dump(pairs, open(out, "w"))
print("exported", len(pairs), "pairs ->", out)
for i, pr in enumerate(pairs):
    print(i, pr["flow"], "p_len", len(pr["p"]), "collector", len(pr["collector_dx"] or ""), "snapshot", len(pr["snapshot_dx"] or ""), "dx", len(pr["dx"] or ""))
