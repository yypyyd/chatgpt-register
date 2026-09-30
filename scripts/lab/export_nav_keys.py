"""导出真实 Chrome 154 里 Navigator.prototype 的可枚举成员及其 toString 形态，供假 navigator 使用。"""
import json, os
from playwright.sync_api import sync_playwright

JS = """() => {
  const proto = Object.getPrototypeOf(navigator);
  const out = [];
  for (const k of Object.keys(proto)) {
    let kind = 'unknown', str = '';
    try {
      const v = navigator[k];
      if (typeof v === 'function') { kind = 'fn'; str = v.toString(); }
      else if (v === null) { kind = 'null'; str = 'null'; }
      else if (typeof v === 'object') { kind = 'obj'; str = Object.prototype.toString.call(v); try { str = v.toString(); } catch (e) {} }
      else { kind = typeof v; str = String(v); }
    } catch (e) { kind = 'throws'; str = String(e).slice(0, 60); }
    out.push([k, kind, str]);
  }
  return out;
}"""
with sync_playwright() as p:
    b = p.chromium.launch(channel="chrome", headless=False)
    pg = b.new_page(no_viewport=True)
    pg.goto("https://example.com/")
    keys = pg.evaluate(JS)
    b.close()
json.dump(keys, open(os.path.join(os.environ["TEMP"], "nav_keys.json"), "w", encoding="utf-8"), ensure_ascii=False)
print(len(keys), "keys")
for k in keys:
    print(k)
