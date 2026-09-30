"""对照实验：真实 Windows Chrome + 同一个 BestGo 出口池 + 真实(受信任)鼠标键盘事件。

守护进程：启动本地代理桥、拉起 Chrome、通过最小 CDP 控制（不 Runtime.enable），
并在 127.0.0.1:8765 提供 JSON 控制接口。用 scripts/lab/ctl.py 发命令。
"""
import asyncio, base64, json, math, os, random, re, subprocess, sys, threading, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

import websockets

TMP = os.environ["TEMP"]
TAG = os.environ.get("CTL_TAG", "")
PROXY_FILE = os.path.join(TMP, "bestgo_proxy.txt")
CHROME = r"C:\Program Files\Google\Chrome\Application\chrome.exe"
BRIDGE_PORT = int(os.environ.get("CTL_BRIDGE_PORT", "18080"))
CDP_PORT = int(os.environ.get("CTL_CDP_PORT", "9333"))
CTL_PORT = int(os.environ.get("CTL_PORT", "8765"))
RUN = time.strftime("%Y%m%d_%H%M%S")
PROFILE = os.path.join(TMP, "ctl_profile_" + TAG + RUN)
NETLOG = os.path.join(TMP, "ctl_net%s.jsonl" % TAG)
AT_FILE = os.path.join(TMP, "ctl_at%s.txt" % TAG)
COOKIES_FILE = os.path.join(TMP, "ctl_cookies%s.json" % TAG)
PROBE_LOG = os.path.join(TMP, "ctl_probe%s.log" % TAG)
SHOT_DIR = os.path.join(TMP, "ctl_shots%s" % TAG)
os.makedirs(SHOT_DIR, exist_ok=True)
BLOCK = [x for x in os.environ.get("CTL_BLOCK", "").split(",") if x]

loop = asyncio.new_event_loop()


def load_proxy():
    raw = open(PROXY_FILE).read().strip()
    tag = os.environ.get("CTL_SESSION")
    u = urlparse(raw)
    user, pw = u.username, u.password
    if tag:
        if re.search(r"-session-[^-]*", user, re.I):
            user = re.sub(r"(-session-)[^-]*", r"\g<1>" + tag, user, flags=re.I)
        else:
            user += "-session-" + tag
    return u.hostname, u.port, user, pw


UP_HOST, UP_PORT, UP_USER, UP_PASS = load_proxy()
SESSION_TOKEN = (re.search(r"-session-([^-]*)", UP_USER, re.I) or [None, ""])[1]
AUTH = "Basic " + base64.b64encode(f"{UP_USER}:{UP_PASS}".encode()).decode()


# ---------------------------------------------------------------- proxy bridge
async def pipe(r, w):
    try:
        while True:
            d = await r.read(65536)
            if not d:
                break
            w.write(d)
            await w.drain()
    except Exception:
        pass
    finally:
        try:
            w.close()
        except Exception:
            pass


async def handle_client(cr, cw):
    try:
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = await cr.read(4096)
            if not chunk:
                cw.close()
                return
            head += chunk
        header_blob, _, rest = head.partition(b"\r\n\r\n")
        lines = header_blob.split(b"\r\n")
        method, target, ver = lines[0].decode().split(" ", 2)
        ur, uw = await asyncio.open_connection(UP_HOST, UP_PORT)
        if method.upper() == "CONNECT":
            req = f"CONNECT {target} HTTP/1.1\r\nHost: {target}\r\nProxy-Authorization: {AUTH}\r\nProxy-Connection: keep-alive\r\n\r\n"
            uw.write(req.encode())
            await uw.drain()
            resp = b""
            while b"\r\n\r\n" not in resp:
                c = await ur.read(4096)
                if not c:
                    break
                resp += c
            if not resp.startswith(b"HTTP/1.1 200") and not resp.startswith(b"HTTP/1.0 200"):
                cw.write(resp or b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
                await cw.drain()
                cw.close()
                uw.close()
                return
            extra = resp.partition(b"\r\n\r\n")[2]
            cw.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            if extra:
                cw.write(extra)
            await cw.drain()
            if rest:
                uw.write(rest)
                await uw.drain()
        else:
            new_lines = [lines[0]] + [l for l in lines[1:] if not l.lower().startswith(b"proxy-authorization")]
            new_lines.append(f"Proxy-Authorization: {AUTH}".encode())
            uw.write(b"\r\n".join(new_lines) + b"\r\n\r\n" + rest)
            await uw.drain()
        await asyncio.gather(pipe(cr, uw), pipe(ur, cw))
    except Exception:
        try:
            cw.close()
        except Exception:
            pass


# ------------------------------------------------------------------------ CDP
class CDP:
    def __init__(self, ws):
        self.ws = ws
        self.n = 0
        self.pending = {}
        self.handlers = {}
        self.mouse = (random.randint(300, 700), random.randint(200, 500))
        self.net_ids = {}

    async def reader(self):
        async for msg in self.ws:
            d = json.loads(msg)
            if "id" in d and d["id"] in self.pending:
                fut = self.pending.pop(d["id"])
                if not fut.done():
                    if "error" in d:
                        fut.set_exception(RuntimeError(json.dumps(d["error"])))
                    else:
                        fut.set_result(d.get("result", {}))
            elif "method" in d:
                h = self.handlers.get(d["method"])
                if h:
                    try:
                        r = h(d.get("params", {}))
                        if asyncio.iscoroutine(r):
                            asyncio.ensure_future(r)
                    except Exception as e:
                        print("handler err", e, file=sys.stderr)

    async def send(self, method, params=None, timeout=60):
        self.n += 1
        i = self.n
        fut = loop.create_future()
        self.pending[i] = fut
        await self.ws.send(json.dumps({"id": i, "method": method, "params": params or {}}))
        return await asyncio.wait_for(fut, timeout)

    async def ev(self, expr, await_promise=True):
        r = await self.send("Runtime.evaluate", {"expression": expr, "returnByValue": True,
                                                  "awaitPromise": await_promise, "userGesture": True})
        if "exceptionDetails" in r:
            return {"__error__": r["exceptionDetails"].get("text"), "detail": str(r["exceptionDetails"])[:400]}
        return r.get("result", {}).get("value")

    # --- human-like input
    async def move_to(self, x, y):
        x0, y0 = self.mouse
        dist = math.hypot(x - x0, y - y0)
        steps = max(12, min(40, int(dist / 18)))
        cx1, cy1 = x0 + (x - x0) * 0.3 + random.randint(-60, 60), y0 + (y - y0) * 0.1 + random.randint(-60, 60)
        cx2, cy2 = x0 + (x - x0) * 0.75 + random.randint(-40, 40), y0 + (y - y0) * 0.9 + random.randint(-40, 40)
        for s in range(1, steps + 1):
            t = s / steps
            t = t * t * (3 - 2 * t)
            bx = (1 - t) ** 3 * x0 + 3 * (1 - t) ** 2 * t * cx1 + 3 * (1 - t) * t ** 2 * cx2 + t ** 3 * x
            by = (1 - t) ** 3 * y0 + 3 * (1 - t) ** 2 * t * cy1 + 3 * (1 - t) * t ** 2 * cy2 + t ** 3 * y
            await self.send("Input.dispatchMouseEvent", {"type": "mouseMoved", "x": bx, "y": by, "buttons": 0, "pointerType": "mouse"})
            await asyncio.sleep(random.uniform(0.006, 0.02))
        self.mouse = (x, y)

    async def click_xy(self, x, y):
        x += random.uniform(-3, 3)
        y += random.uniform(-2, 2)
        await self.move_to(x, y)
        await asyncio.sleep(random.uniform(0.08, 0.25))
        await self.send("Input.dispatchMouseEvent", {"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1, "pointerType": "mouse"})
        await asyncio.sleep(random.uniform(0.05, 0.13))
        await self.send("Input.dispatchMouseEvent", {"type": "mouseReleased", "x": x, "y": y, "button": "left", "buttons": 0, "clickCount": 1, "pointerType": "mouse"})

    SHIFTED = set('~!@#$%^&*()_+{}|:"<>?ABCDEFGHIJKLMNOPQRSTUVWXYZ')

    async def type_text(self, text, fast=False):
        for ch in text:
            shift = ch in self.SHIFTED
            mod = 8 if shift else 0
            if shift:
                await self.send("Input.dispatchKeyEvent", {"type": "rawKeyDown", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16, "modifiers": 8})
                await asyncio.sleep(random.uniform(0.02, 0.06))
            vk = ord(ch.upper()) if ch.isalnum() else 0
            p = {"key": ch, "text": ch, "unmodifiedText": ch, "modifiers": mod}
            if vk:
                p["windowsVirtualKeyCode"] = vk
                p["code"] = ("Key" + ch.upper()) if ch.isalpha() else ("Digit" + ch)
            await self.send("Input.dispatchKeyEvent", dict(p, type="keyDown"))
            await asyncio.sleep(random.uniform(0.03, 0.09))
            await self.send("Input.dispatchKeyEvent", dict(p, type="keyUp", text=""))
            if shift:
                await asyncio.sleep(random.uniform(0.01, 0.04))
                await self.send("Input.dispatchKeyEvent", {"type": "keyUp", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16})
            base = random.uniform(0.05, 0.11) if fast else random.uniform(0.08, 0.2)
            if random.random() < 0.08:
                base += random.uniform(0.15, 0.4)
            await asyncio.sleep(base)

    async def press(self, key):
        keys = {"Enter": (13, "Enter", "\r"), "Tab": (9, "Tab", ""), "Backspace": (8, "Backspace", ""), "Escape": (27, "Escape", "")}
        vk, code, text = keys[key]
        p = {"key": key, "code": code, "windowsVirtualKeyCode": vk}
        await self.send("Input.dispatchKeyEvent", dict(p, type="keyDown", **({"text": text} if text else {})))
        await asyncio.sleep(random.uniform(0.04, 0.09))
        await self.send("Input.dispatchKeyEvent", dict(p, type="keyUp"))

    async def scroll(self, dy):
        x, y = self.mouse
        await self.send("Input.dispatchMouseEvent", {"type": "mouseWheel", "x": x, "y": y, "deltaX": 0, "deltaY": dy})


FIND_JS = r"""
(function(sel, txt, nth){
  function vis(e){ const r=e.getBoundingClientRect(); const s=getComputedStyle(e);
    return r.width>2 && r.height>2 && s.visibility!=='hidden' && s.display!=='none' && r.bottom>0 && r.right>0; }
  let els=[];
  if(sel){ els=[...document.querySelectorAll(sel)].filter(vis); }
  else if(txt){
    const t=txt.toLowerCase();
    els=[...document.querySelectorAll('button,a,[role=button],input[type=submit],[role=link],label,span,div')]
      .filter(e=>vis(e)&&(e.innerText||e.value||'').trim().toLowerCase()===t);
    if(!els.length) els=[...document.querySelectorAll('button,a,[role=button],input[type=submit]')]
      .filter(e=>vis(e)&&(e.innerText||e.value||'').toLowerCase().includes(t));
  }
  const e=els[nth||0]; if(!e) return null;
  e.scrollIntoView({block:'center',inline:'center'});
  const r=e.getBoundingClientRect();
  return {x:r.left+r.width/2, y:r.top+r.height/2, w:r.width, h:r.height, tag:e.tagName, text:(e.innerText||e.value||'').slice(0,40), n:els.length};
})(%s,%s,%s)
"""


async def find(cdp, sel=None, txt=None, nth=0):
    return await cdp.ev(FIND_JS % (json.dumps(sel), json.dumps(txt), nth))


JSFILL = r"""(function(sel,val){
  const el=[...document.querySelectorAll(sel)].find(e=>{const r=e.getBoundingClientRect();return r.width>2&&r.height>2});
  if(!el) return 'not found';
  el.focus();
  const proto=el.tagName==='TEXTAREA'?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto,'value').set.call(el,val);
  el.dispatchEvent(new Event('input',{bubbles:true}));
  el.dispatchEvent(new Event('change',{bubbles:true}));
  return 'ok:'+el.value.length;
})(%s,%s)"""

JSCLICK = r"""(function(sel,txt){
  function vis(e){const r=e.getBoundingClientRect();return r.width>2&&r.height>2}
  let els=[];
  if(sel) els=[...document.querySelectorAll(sel)].filter(vis);
  else { const t=txt.toLowerCase(); els=[...document.querySelectorAll('button,a,[role=button]')].filter(e=>vis(e)&&(e.innerText||'').trim().toLowerCase()===t); }
  if(!els.length) return 'not found';
  els[0].click(); return 'ok:'+(els[0].innerText||'').slice(0,20);
})(%s,%s)"""


cdp_holder = {}
net_lock = threading.Lock()
BODY_PATTERNS = ("sentinel/req", "email-otp", "create_account", "api/auth/session", "authorize/continue",
                 "/api/accounts/", "sentinel/sdk", "/backend-api/me", "callback/openai", "user/register")


def netlog(rec):
    rec["ts"] = time.time()
    with net_lock:
        with open(NETLOG, "a", encoding="utf-8") as f:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def attach_net(cdp):
    def on_req(p):
        r = p.get("request", {})
        cdp.net_ids[p["requestId"]] = r.get("url", "")
        netlog({"k": "req", "id": p["requestId"], "m": r.get("method"), "url": r.get("url"), "h": r.get("headers"),
                "body": (r.get("postData") or "")[:6000], "type": p.get("type")})

    def on_resp(p):
        r = p.get("response", {})
        netlog({"k": "resp", "id": p["requestId"], "url": r.get("url"), "status": r.get("status"),
                "h": r.get("headers"), "mime": r.get("mimeType")})

    async def on_fin(p):
        url = cdp.net_ids.get(p["requestId"], "")
        if any(x in url for x in BODY_PATTERNS):
            try:
                b = await cdp.send("Network.getResponseBody", {"requestId": p["requestId"]}, timeout=10)
                netlog({"k": "body", "id": p["requestId"], "url": url, "body": (b.get("body") or "")[:600000]})
            except Exception as e:
                netlog({"k": "bodyerr", "id": p["requestId"], "url": url, "err": str(e)[:200]})

    cdp.handlers["Network.requestWillBeSent"] = on_req
    cdp.handlers["Network.responseReceived"] = on_resp
    cdp.handlers["Network.loadingFinished"] = on_fin


async def ensure_cdp():
    if "cdp" in cdp_holder:
        return cdp_holder["cdp"]
    for _ in range(60):
        try:
            tabs = json.load(urllib.request.urlopen(f"http://127.0.0.1:{CDP_PORT}/json/list", timeout=2))
            pages = [t for t in tabs if t.get("type") == "page"]
            if pages:
                ws = await websockets.connect(pages[0]["webSocketDebuggerUrl"], max_size=64 * 1024 * 1024, ping_interval=None)
                cdp = CDP(ws)
                asyncio.ensure_future(cdp.reader())
                attach_net(cdp)
                await cdp.send("Page.enable")
                await cdp.send("Network.enable")
                if BLOCK:
                    await cdp.send("Network.setBlockedURLs", {"urls": BLOCK})
                await cdp.send("Emulation.setFocusEmulationEnabled", {"enabled": True})
                await cdp.send("Page.bringToFront")
                cdp_holder["cdp"] = cdp
                return cdp
        except Exception:
            pass
        await asyncio.sleep(0.5)
    raise RuntimeError("cannot attach to chrome")


async def set_timezone(cdp):
    last = ""
    for _ in range(4):
        try:
            req = urllib.request.build_opener(urllib.request.ProxyHandler({"https": f"http://127.0.0.1:{BRIDGE_PORT}"}))
            info = await loop.run_in_executor(None, lambda: json.load(req.open("https://ipwho.is/", timeout=25)))
            tz = (info.get("timezone") or {}).get("id")
            cdp_holder["ip"] = info.get("ip")
            cdp_holder["country"] = info.get("country_code")
            cdp_holder["tz"] = tz
            if tz:
                await cdp.send("Emulation.setTimezoneOverride", {"timezoneId": tz})
            return info.get("ip"), info.get("country_code"), tz
        except Exception as e:
            last = str(e)
            await asyncio.sleep(2)
    return None, None, last


# ----------------------------------------------------------------- commands
async def do_cmd(c):
    cdp = await ensure_cdp()
    cmd = c.get("cmd")
    if cmd == "info":
        return {"session": SESSION_TOKEN, "ip": cdp_holder.get("ip"), "country": cdp_holder.get("country"), "tz": cdp_holder.get("tz"),
                "url": await cdp.ev("location.href"), "title": await cdp.ev("document.title")}
    if cmd == "goto":
        await cdp.send("Page.navigate", {"url": c["url"]})
        await asyncio.sleep(c.get("wait", 3))
        return {"url": await cdp.ev("location.href")}
    if cmd == "eval":
        return await cdp.ev(c["expr"])
    if cmd == "text":
        return await cdp.ev("({url:location.href,title:document.title,text:document.body?document.body.innerText.slice(0,%d):''})" % c.get("n", 1500))
    if cmd == "inputs":
        return await cdp.ev("""[...document.querySelectorAll('input,textarea,button,select')].map(e=>({tag:e.tagName,type:e.type,name:e.name,id:e.id,ph:e.placeholder,ac:e.autocomplete,txt:(e.innerText||e.value||'').slice(0,30),ar:e.getAttribute('aria-label')})).slice(0,40)""")
    if cmd == "shot":
        r = await cdp.send("Page.captureScreenshot", {"format": "png"})
        path = os.path.join(SHOT_DIR, c.get("name", "s") + ".png")
        with open(path, "wb") as f:
            f.write(base64.b64decode(r["data"]))
        return {"path": path}
    if cmd == "click":
        pos = await find(cdp, c.get("selector"), c.get("text"), c.get("nth", 0))
        if not pos or "__error__" in pos:
            return {"error": "not found", "detail": pos}
        await asyncio.sleep(random.uniform(0.2, 0.6))
        await cdp.click_xy(pos["x"], pos["y"])
        await asyncio.sleep(c.get("wait", 1.5))
        return {"clicked": pos, "url": await cdp.ev("location.href")}
    if cmd == "type":
        await cdp.type_text(c["text"], c.get("fast", False))
        return {"typed": len(c["text"])}
    if cmd == "press":
        await cdp.press(c["key"])
        await asyncio.sleep(c.get("wait", 1.0))
        return {"pressed": c["key"], "url": await cdp.ev("location.href")}
    if cmd == "idle":
        # 拟人空闲：随机移动/滚动
        t_end = time.time() + c.get("seconds", 5)
        while time.time() < t_end:
            await cdp.move_to(random.randint(200, 1100), random.randint(150, 700))
            await asyncio.sleep(random.uniform(0.3, 1.2))
            if random.random() < 0.2:
                await cdp.scroll(random.choice([-120, 120]))
        return {"idle": True}
    if cmd == "wait":
        t_end = time.time() + c.get("timeout", 20)
        while time.time() < t_end:
            js = "!!document.querySelector(%s)" % json.dumps(c["selector"]) if c.get("selector") else \
                 "document.body && document.body.innerText.includes(%s)" % json.dumps(c["contains"])
            if await cdp.ev(js):
                return {"found": True}
            await asyncio.sleep(0.5)
        return {"found": False}
    if cmd == "cookies":
        r = await cdp.send("Network.getAllCookies")
        out = []
        for ck in r.get("cookies", []):
            d = ck.get("domain", "")
            if "openai.com" in d or "chatgpt.com" in d:
                out.append({"name": ck["name"], "value": ck["value"], "domain": d, "path": ck.get("path", "/"),
                            "expires": ck.get("expires", 0) if ck.get("expires", 0) > 0 else 0,
                            "httpOnly": ck.get("httpOnly", False), "secure": ck.get("secure", False),
                            "session": ck.get("session", False), "sameSite": ck.get("sameSite", "")})
        json.dump(out, open(COOKIES_FILE, "w"))
        return {"saved": COOKIES_FILE, "count": len(out), "names": sorted(set(x["name"] for x in out))}
    if cmd == "session":
        js = """fetch('/api/auth/session',{credentials:'include'}).then(async r=>{const j=await r.json().catch(()=>({}));return {status:r.status,has_at:!!j.accessToken,at:j.accessToken||'',email:(j.user&&j.user.email)||''}})"""
        r = await cdp.ev(js)
        if isinstance(r, dict) and r.get("at"):
            open(AT_FILE, "w").write(r["at"])
            r = dict(r, at="<saved %d chars>" % len(r["at"]))
            r["email"] = (r.get("email") or "")[:2] + "***"
        return r
    if cmd == "probe":
        return await inpage_probe(cdp)
    if cmd == "probeloop":
        asyncio.ensure_future(probe_loop(cdp, c.get("every", 60), c.get("total", 2400)))
        return {"started": True}
    if cmd == "jsfill":
        return await cdp.ev(JSFILL % (json.dumps(c["selector"]), json.dumps(c["text"])))
    if cmd == "jsclick":
        return await cdp.ev(JSCLICK % (json.dumps(c.get("selector")), json.dumps(c.get("text"))))
    if cmd == "tz":
        return await set_timezone(cdp)
    if cmd == "cdp":
        return await cdp.send(c["method"], c.get("params") or {})
    if cmd == "quit":
        threading.Thread(target=lambda: (time.sleep(0.5), os._exit(0))).start()
        return {"bye": True}
    return {"error": "unknown cmd"}


async def inpage_probe(cdp):
    if not os.path.exists(AT_FILE):
        return {"error": "no at"}
    at = open(AT_FILE).read().strip()
    js = """(async()=>{
      const at=%s; const did=(document.cookie.match(/oai-did=([^;]+)/)||[])[1]||'';
      const H={'authorization':'Bearer '+at,'oai-language':'en-US','oai-device-id':did};
      const out={};
      for (const [k,u,m,b] of [['me','/backend-api/me','GET',null],['init','/backend-api/conversation/init','POST','{"gizmo_id":null,"requested_default_model":null,"conversation_id":null,"timezone_offset_min":240}']]){
        try{ const r=await fetch(u,{method:m,credentials:'include',headers:b?Object.assign({'content-type':'application/json'},H):H,body:b}); 
             let code='ok'; if(!r.ok){ const j=await r.json().catch(()=>({})); code=(j.error&&(j.error.code||j.error.message))||(j.detail&&(j.detail.code||String(j.detail).slice(0,40)))||('http_'+r.status); }
             out[k]=r.status+'/'+String(code).slice(0,40);
        }catch(e){ out[k]='err:'+e.message; }
      }
      return out; })()""" % json.dumps(at)
    return await cdp.ev(js)


def jwt_iat(at):
    try:
        p = at.split(".")[1]
        p += "=" * (-len(p) % 4)
        return json.loads(base64.urlsafe_b64decode(p)).get("iat", 0)
    except Exception:
        return 0


async def probe_loop(cdp, every, total):
    at = open(AT_FILE).read().strip()
    iat = jwt_iat(at) or time.time()
    n = 0
    while time.time() - iat < total:
        n += 1
        try:
            r = await inpage_probe(cdp)
        except Exception as e:
            r = {"error": str(e)[:100]}
        line = "+%.1fmin poll#%d %s" % ((time.time() - iat) / 60, n, json.dumps(r))
        with open(PROBE_LOG, "a") as f:
            f.write(line + "\n")
        if isinstance(r, dict) and "revoked" in json.dumps(r):
            with open(PROBE_LOG, "a") as f:
                f.write("REVOKED at +%.1fmin\n" % ((time.time() - iat) / 60))
            return
        await asyncio.sleep(every)
    with open(PROBE_LOG, "a") as f:
        f.write("SURVIVED %.1fmin\n" % ((time.time() - iat) / 60))


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        fut = asyncio.run_coroutine_threadsafe(do_cmd(body), loop)
        try:
            res = fut.result(timeout=body.get("timeout", 120))
        except Exception as e:
            res = {"error": "%s: %s" % (type(e).__name__, e)}
        out = json.dumps(res, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json; charset=utf-8")
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)


def main():
    threading.Thread(target=loop.run_forever, daemon=True).start()
    asyncio.run_coroutine_threadsafe(asyncio.start_server(handle_client, "127.0.0.1", BRIDGE_PORT), loop).result()
    print("bridge up on", BRIDGE_PORT, "session", SESSION_TOKEN, flush=True)
    args = [CHROME, f"--user-data-dir={PROFILE}", f"--remote-debugging-port={CDP_PORT}",
            f"--proxy-server=http://127.0.0.1:{BRIDGE_PORT}", "--no-first-run", "--no-default-browser-check",
            "--lang=en-US", "--accept-lang=en-US,en", "--window-size=1366,900",
            "--window-position=" + os.environ.get("CTL_POS", "40,10"),
            "--force-webrtc-ip-handling-policy=disable_non_proxied_udp", "about:blank"]
    subprocess.Popen(args)
    cdp = asyncio.run_coroutine_threadsafe(ensure_cdp(), loop).result(timeout=60)
    ip, cc, tz = asyncio.run_coroutine_threadsafe(set_timezone(cdp), loop).result(timeout=60)
    print("chrome up; exit ip=%s country=%s tz=%s" % (ip, cc, tz), flush=True)
    srv = ThreadingHTTPServer(("127.0.0.1", CTL_PORT), H)
    print("ctl on", CTL_PORT, flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()

