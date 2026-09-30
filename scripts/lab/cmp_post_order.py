"""在服务器上用协议 tls-client 发各类 POST 到 tls.peet.ws，与真实 Chrome 测得的顺序逐项比对。"""
import json, os, sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import srv

real = json.load(open(os.path.join(os.path.dirname(__file__), "header_orders.json"), encoding="utf-8"))
mapping = {"form": "xhr_post_form", "json": "xhr_post_json", "json_sentinel": "xhr_post_json_sentinel", "min": "xhr_post_min"}
real["xhr_post_min"] = real.get("xhr_post_form")  # min 与 form 的集合不同，下面单独说明，仅比较顺序骨架
cli = srv.conn()
for kind in ("form", "json", "json_sentinel", "min"):
    out = srv.run(cli, "/opt/chatgpt-register/gptlive9 -mode fetchpost -url https://tls.peet.ws/api/all -note %s -timeout 90s 2>&1 | head -c 100000" % kind, timeout=120)
    try:
        d = json.loads(out[out.find("{"):out.rfind("}") + 1])
    except Exception as e:
        print(kind, "parse fail", out[:200])
        continue
    ours = None
    for f in d["http2"]["sent_frames"]:
        if f.get("frame_type") == "HEADERS":
            ours = [(h.split(":", 1)[0] if not h.startswith(":") else ":" + h[1:].split(":", 1)[0]) for h in f["headers"]]
            break
    ref = [x[0] for x in real[mapping[kind]]]
    same = ours == ref
    print("== %s : %s" % (kind, "IDENTICAL to real Chrome" if same else "DIFFERENT"))
    if not same:
        print("   ours:", ours)
        print("   real:", ref)
cli.close()
