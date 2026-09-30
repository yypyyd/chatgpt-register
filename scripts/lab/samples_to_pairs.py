import json, os, sys

src = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.environ["TEMP"], "sentinel_req_samples.jsonl")
dst = sys.argv[2] if len(sys.argv) > 2 else os.path.join(os.environ["TEMP"], "dx_pairs_samples.json")
pairs = []
for line in open(src, encoding="utf-8"):
    r = json.loads(line)
    try:
        req = json.loads(r["req"])
        resp = json.loads(r["resp"])
    except Exception:
        continue
    so = resp.get("so") or {}
    ts = resp.get("turnstile") or {}
    if not ts.get("dx"):
        continue
    pairs.append({"flow": req.get("flow"), "p": req["p"], "id": req.get("id"), "token": resp.get("token"),
                  "dx": ts["dx"], "pow": resp.get("proofofwork"),
                  "collector_dx": so.get("collector_dx"), "snapshot_dx": so.get("snapshot_dx")})
json.dump(pairs, open(dst, "w"))
print("pairs:", len(pairs), "->", dst)
