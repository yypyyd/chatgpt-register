"""用本机真实 Chrome 生成 dx 文本盒子指纹用的字体度量表 -> scripts/font_metrics.js

真实认证页给测量 div 继承了 letter-spacing:-0.01em（用抓包里 Arial/10px=17.4875、Trebuchet MS/17px=30.6625
逐项核对，完全一致）。这里在相同 CSS 下、按 (DPR, 字体, 字号) 分别实测 52 个字母的单字簇宽度和行高，
运行时把 3 个字簇的宽度相加即可（组合符号零宽）。
"""
import json, os, random, unicodedata
from playwright.sync_api import sync_playwright

FONTS = ["Arial", "Helvetica", "Times New Roman", "Palatino", "Georgia", "Garamond", "Impact", "Trebuchet MS", "Verdana",
         "Courier New", "Lucida Console", "Lucida Sans Unicode", "Comic Sans MS", "Tahoma"]
LETTERS = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
SIZES = list(range(10, 21))
DPRS = [1, 1.25, 1.5]
MARKS = [chr(c) for c in range(0x300, 0x330)]
random.seed(11)

CSS = "visibility:hidden;position:fixed;letter-spacing:-0.01em;"
JS_ADV = """([font,sizes])=>sizes.map(s=>[...'%s'].map(ch=>{const d=document.createElement('div');d.style.cssText='%s';d.style.fontFamily=font;d.style.fontSize=s+'px';d.innerText=ch;document.body.appendChild(d);const w=d.getBoundingClientRect().width;d.remove();return w}))""" % (LETTERS, CSS)
JS_DIRECT = """(items)=>items.map(([f,s,t])=>{const d=document.createElement('div');d.style.cssText='%s';d.style.fontFamily=f;d.style.fontSize=s+'px';d.innerText=t;document.body.appendChild(d);const r=d.getBoundingClientRect();d.remove();return [r.width,r.height]})""" % CSS


def zalgo():
    out = ""
    for _ in range(3):
        out += random.choice(LETTERS)
        for _ in range(random.randint(2, 4)):
            out += random.choice(MARKS)
    return out


def cluster_base(text):
    """按 NFD 拆出每个字簇的基础字母（组合符号零宽）。"""
    bases = []
    for ch in unicodedata.normalize("NFD", text):
        if 0x300 <= ord(ch) <= 0x36f:
            continue
        bases.append(ch)
    return bases


out = {"fonts": FONTS, "sizes": SIZES, "letters": LETTERS, "adv": {}, "lh": {}}
errs = []
with sync_playwright() as p:
    for dpr in DPRS:
        b = p.chromium.launch(channel="chrome", headless=True, args=["--force-device-scale-factor=%s" % dpr])
        pg = b.new_page(no_viewport=True)
        pg.set_content("<html><body></body></html>")
        out["adv"][str(dpr)] = {}
        out["lh"][str(dpr)] = {}
        for f in FONTS:
            adv = pg.evaluate(JS_ADV, [f, SIZES])
            out["adv"][str(dpr)][f] = [[round(x, 4) for x in row] for row in adv]
            hs = pg.evaluate(JS_DIRECT, [[f, s, "Z\u0327\u0306Ḙ\u032f\u031f\u0309\u0303z\u0319\u0309\u0313"] for s in SIZES])
            out["lh"][str(dpr)][f] = [round(h[1], 4) for h in hs]
        # 用随机 zalgo 串验证“字簇宽度相加”模型的误差
        for f in FONTS[:8]:
            for s in (10, 13, 17, 20):
                texts = [zalgo() for _ in range(10)]
                res = pg.evaluate(JS_DIRECT, [[f, s, t] for t in texts])
                for t, r in zip(texts, res):
                    si = SIZES.index(s)
                    model = sum(out["adv"][str(dpr)][f][si][LETTERS.find(ch)] if LETTERS.find(ch) >= 0 else 0 for ch in cluster_base(t))
                    errs.append((dpr, abs(model - r[0])))
        b.close()

for dpr in DPRS:
    e = [x for d, x in errs if d == dpr]
    print("DPR %s: n=%d mean_abs_err=%.4f max_abs_err=%.4f" % (dpr, len(e), sum(e) / len(e), max(e)))

js = "// 自动生成：scripts/gen_font_metrics.py（本机 Chrome 154 实测，含真实页面的 letter-spacing:-0.01em）。\nmodule.exports = " + json.dumps(out, ensure_ascii=False, separators=(",", ":")) + ";\n"
dst = os.path.join(os.path.dirname(__file__), "..", "font_metrics.js")
open(dst, "w", encoding="utf-8").write(js)
print("wrote", dst, len(js), "bytes")

