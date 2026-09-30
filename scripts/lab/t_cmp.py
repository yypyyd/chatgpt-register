"""对比 lab_out.json（我们 Node 假环境的 t）与真实浏览器同一程序的 t。用法: python _t_cmp.py [real_tag=_x7] [real_index=0]"""
import importlib.util, json, os, sys

spec = importlib.util.spec_from_file_location("pt", os.path.join(os.path.dirname(__file__), "pt_decode.py"))
pt = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pt)

tag = sys.argv[1] if len(sys.argv) > 1 else "_x7"
idx = int(sys.argv[2]) if len(sys.argv) > 2 else 0
ours = json.load(open(os.path.join(os.environ["TEMP"], "lab_out.json")))
ours_tok = json.loads(ours["token"])
real = pt.real_tokens(tag)[idx]

ok, ov = pt.decode_map(ours_tok["t"])
rk, rv = pt.decode_map(real["token"]["t"])
print("ours key=%s fields=%s | real key=%s fields=%s" % (ok, len(ov) if ov else None, rk, len(rv) if rv else None))
n = max(len(ov or []), len(rv or []))
for i in range(n):
    o = ov[i][1] if ov and i < len(ov) else "<none>"
    r = rv[i][1] if rv and i < len(rv) else "<none>"
    same = "  " if o == r else "≠ "
    print("%s[%2d] ours=%-70s" % (same, i, json.dumps(o, ensure_ascii=False)[:70]))
    print("     real=%-70s" % json.dumps(r, ensure_ascii=False)[:70])
