"""用真实 Chrome + tls.peet.ws 回显，测量每类请求在 HTTP/2 线上的头顺序与值。
模板与真实抓包（X7）里 CDP 看到的“页面可见头集合”一致。输出 scripts/header_orders.json。
"""
import json, os
from playwright.sync_api import sync_playwright

TEMPLATES = {
    "xhr_get": ("GET", {}, None),
    "xhr_post_form": ("POST", {"content-type": "application/x-www-form-urlencoded"}, "callbackUrl=x&csrfToken=y&json=true"),
    "xhr_post_json_sentinel": ("POST", {
        "x-datadog-parent-id": "1", "openai-sentinel-token": "{}", "x-access-flow-invocation-id": "a", "x-datadog-trace-id": "2",
        "traceparent": "00-0-1-01", "accept": "application/json", "content-type": "application/json", "tracestate": "dd=s:1;o:rum",
        "x-datadog-origin": "rum", "x-openai-document-navigation-id": "n", "openai-sentinel-so-token": "{}", "x-datadog-sampling-priority": "1",
    }, '{"code":"123456"}'),
    "xhr_post_json": ("POST", {
        "x-datadog-parent-id": "1", "x-access-flow-invocation-id": "a", "x-datadog-trace-id": "2",
        "traceparent": "00-0-1-01", "accept": "application/json", "content-type": "application/json", "tracestate": "dd=s:1;o:rum",
        "x-datadog-origin": "rum", "x-openai-document-navigation-id": "n", "x-datadog-sampling-priority": "1",
    }, "{}"),
}


def frames(txt):
    d = json.loads(txt)
    for f in d.get("http2", {}).get("sent_frames") or []:
        if f.get("frame_type") == "HEADERS":
            out = []
            for h in f.get("headers", []):
                k, v = h.split(": ", 1) if ": " in h else (h, "")
                out.append([k, v])
            return out
    return []


with sync_playwright() as p:
    b = p.chromium.launch(channel="chrome", headless=False)
    pg = b.new_page()
    res = {}
    # 导航：地址栏直输 / 同源带 Referer / 跨站带 Referer
    pg.goto("https://tls.peet.ws/api/all", wait_until="domcontentloaded")
    res["nav_typed"] = frames(pg.inner_text("body"))
    pg.goto("https://tls.peet.ws/", wait_until="domcontentloaded")
    pg.evaluate("() => { location.href = '/api/all'; }")
    pg.wait_for_url("**/api/all")
    pg.wait_for_load_state("domcontentloaded")
    res["nav_ref"] = frames(pg.inner_text("body"))
    pg.goto("https://tls.peet.ws/", wait_until="domcontentloaded")
    for name, (method, headers, body) in TEMPLATES.items():
        js = """async ([method, headers, body]) => {
          const init = {method, credentials: 'include'};
          if (Object.keys(headers).length) init.headers = headers;
          if (body !== null) init.body = body;
          const r = await fetch('/api/all', init); return await r.text(); }"""
        res[name] = frames(pg.evaluate(js, [method, headers, body]))
    b.close()

path = os.path.join(os.path.dirname(__file__), "header_orders.json")
json.dump(res, open(path, "w", encoding="utf-8"), ensure_ascii=False, indent=1)
for k, v in res.items():
    print("==", k)
    print("  ", [x[0] for x in v])
