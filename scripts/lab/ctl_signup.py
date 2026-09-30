"""在对照浏览器里走完整的真实注册流程，并启动页内存活轮询。
用法（先启动 ctl_browser.py，环境变量 CTL_PORT/CTL_TAG 要一致）：
    python scripts/lab/ctl_signup.py <label> [domain] [--nospa] [--noevents]
--nospa     回调后不让 chatgpt.com SPA 继续水合（尽快跳到 session JSON）
--noevents  不产生任何真实鼠标/键盘事件（脚本填表+脚本点击）
"""
import json, os, random, re, sys, time, urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import srv  # noqa: E402

PORT = os.environ.get("CTL_PORT", "8765")
label = sys.argv[1] if len(sys.argv) > 1 else "B"
domain = sys.argv[2] if len(sys.argv) > 2 and not sys.argv[2].startswith("--") else "hotmail.com"
NOSPA = "--nospa" in sys.argv
NOEVENTS = "--noevents" in sys.argv


def ctl(cmd, **kw):
    body = dict(cmd=cmd, **kw)
    req = urllib.request.Request("http://127.0.0.1:%s/" % PORT, data=json.dumps(body).encode(), method="POST")
    r = urllib.request.urlopen(req, timeout=kw.get("timeout", 120) + 10)
    return json.loads(r.read().decode())


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


def remote(cli, cmd, timeout=200):
    return srv.run(cli, cmd, timeout=timeout)


def idle(sec):
    if NOEVENTS:
        time.sleep(sec)
    else:
        ctl("idle", seconds=sec)


def fill(selector, text):
    if NOEVENTS:
        return ctl("jsfill", selector=selector, text=text)
    ctl("click", selector=selector, wait=1)
    return ctl("type", text=text)


def click(selector=None, text=None, wait=4):
    if NOEVENTS:
        r = ctl("jsclick", **({"selector": selector} if selector else {"text": text}))
        time.sleep(wait)
        return {"r": r, "url": ctl("eval", expr="location.href")}
    return ctl("click", **({"selector": selector} if selector else {"text": text}), wait=wait)


FIRST = ["Daniel", "Olivia", "Marcus", "Hannah", "Ethan", "Chloe", "Lucas", "Grace", "Owen", "Nora", "Caleb", "Ivy", "Julian", "Maya"]
LAST = ["Foster", "Bennett", "Hayes", "Carter", "Brooks", "Reed", "Coleman", "Ward", "Perry", "Long", "Price", "Sanders", "Russell", "Diaz"]
CODE_SEL = "input[name=code], input[autocomplete=one-time-code], input[inputmode=numeric], input[type=text]"

cli = srv.conn()
out = remote(cli, "/opt/chatgpt-register/gptlive2 -mode pick -domain %s -note browser-control-%s" % (domain, label))
m = re.search(r"EMAIL=(\S+)", out)
if not m:
    print(out)
    sys.exit(1)
email = m.group(1)
log("mailbox picked:", email[:2] + "***", "mode:", "noevents" if NOEVENTS else "events", "nospa" if NOSPA else "spa")

info = ctl("info")
log("browser exit:", info.get("ip"), info.get("country"), info.get("tz"), "session", info.get("session"))

ctl("goto", url="https://chatgpt.com/", wait=6)
idle(4)
click(text="Sign up for free", wait=3)
ctl("wait", selector="input[type=email], input[name=email]", timeout=15)
fill("input[type=email], input[name=email], input[placeholder*=mail]", email)
time.sleep(0.8)
t_submit = int(time.time())
r = click(text="Continue", wait=4)
log("after continue:", r.get("url"))
w = ctl("wait", selector="input[name=code], input[autocomplete=one-time-code], input[inputmode=numeric]", timeout=30)
if not w.get("found"):
    log("otp page not reached"); sys.exit(4)
log("otp page reached; waiting for mail")
time.sleep(12)  # 等页面水合 + Sentinel iframe 初始化，和真人看邮件的间隔一致
out = remote(cli, "/opt/chatgpt-register/gptlive2 -mode otp -email %s -since %d -timeout 3m" % (email, t_submit - 60))
m = re.search(r"OTP=(\d{6})", out)
if not m:
    log("otp fetch failed:", re.sub(r"\S+@\S+", "***", out)[-300:])
    sys.exit(2)
otp = m.group(1)
log("otp received")
idle(random.randint(2, 5))
for attempt in range(3):
    fill(CODE_SEL, otp)
    time.sleep(0.7)
    ln = ctl("eval", expr="(document.querySelector('%s')||{value:''}).value.length" % CODE_SEL)
    if ln == 6:
        break
    log("otp field not filled (len=%s), retry" % ln)
idle(2)
r = click(selector="button[type=submit]", wait=6)
log("after otp submit:", r.get("url"))
w = ctl("wait", selector="input[name=age]", timeout=30)
if not w.get("found"):
    log("about-you not reached, url=", ctl("eval", expr="location.href")); sys.exit(5)
time.sleep(3)  # 等 React 水合完，否则输入会被重置
name = random.choice(FIRST) + " " + random.choice(LAST)
age = str(random.randint(22, 40))
for attempt in range(3):
    idle(2)
    fill("input[name=name]", name)
    time.sleep(0.6)
    if not NOEVENTS:
        ctl("press", key="Tab", wait=0.5)
        ctl("type", text=age)
    else:
        fill("input[name=age]", age)
    time.sleep(0.8)
    ok = ctl("eval", expr="(()=>{const n=document.querySelector('input[name=name]'),a=document.querySelector('input[name=age]');return !!(n&&a&&n.value.length>3&&a.value.length>=2)})()")
    if ok is True:
        break
    log("about-you fields empty, retry", attempt + 1)
    ctl("eval", expr="location.reload()")
    time.sleep(6)
idle(2)
if NOSPA:
    ctl("cdp", method="Network.setBlockedURLs", params={"urls": ["https://chatgpt.com/"]})
    log("nospa: SPA landing blocked")
r = click(selector="button[type=submit]", wait=8)
log("after about-you:", r.get("url"))
if NOSPA:
    time.sleep(3)
    ctl("cdp", method="Network.setBlockedURLs", params={"urls": []})
    ctl("goto", url="https://chatgpt.com/api/auth/session", wait=3)
    log("nospa: navigated to session json:", ctl("eval", expr="location.href"))
for _ in range(20):
    u = ctl("eval", expr="location.href")
    if isinstance(u, str) and u.startswith("https://chatgpt.com"):
        break
    time.sleep(1)
time.sleep(3)
s = ctl("session")
log("session:", s)
c = ctl("cookies")
log("cookies:", c.get("count"))
if not (isinstance(s, dict) and s.get("has_at")):
    log("no AT, abort")
    sys.exit(3)
p = ctl("probe")
log("first probe:", p)
ctl("probeloop", every=60, total=3000)
log("probeloop started; label=%s" % label)
cli.close()
