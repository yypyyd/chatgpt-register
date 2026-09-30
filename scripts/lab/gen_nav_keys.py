import json, os

keys = json.load(open(os.path.join(os.environ["TEMP"], "nav_keys.json"), encoding="utf-8"))
rows = []
for name, kind, s in keys:
    if kind == "obj":
        cls = s[len("[object "):-1] if s.startswith("[object ") else ""
        rows.append([name, "obj", cls])
    else:
        rows.append([name, kind, ""])
js = "// 自动生成：scripts/genexport_nav_keys.py（真实 Chrome 154 的 Navigator.prototype 可枚举成员，顺序一致）。\nmodule.exports = " + json.dumps(rows, ensure_ascii=False, separators=(",", ":")) + ";\n"
dst = os.path.join(os.path.dirname(__file__), "..", "nav_keys.js")
open(dst, "w", encoding="utf-8").write(js)
print("wrote", dst, len(rows), "keys")
