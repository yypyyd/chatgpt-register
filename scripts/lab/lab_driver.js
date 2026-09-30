// 端到端实验台：子进程启动生产用的 sentinel_node.js（官方 SDK + Node 假浏览器），
// 用抓包得到的“真实 dx / collector / snapshot 程序”按当次请求里的 p 重新加密后充当 sentinel/req 响应，
// 复现线上运行，但不需要网络。用法: node lab_driver.js <dx_pairs.json> <index> [--interact]
const { spawn } = require("child_process");
const fs = require("fs");
const path = require("path");
const readline = require("readline");

const pairsFile = process.argv[2];
const idx = Number(process.argv[3] || 0);
const withInteract = process.argv.includes("--interact");
const pairs = JSON.parse(fs.readFileSync(pairsFile, "utf8"));
const pair = pairs[idx];
const sdkPath = process.env.SDK_FILE || path.join(process.env.USERPROFILE || "", ".cursor/projects/e-CODE-Register-chatgpt-register/agent-tools/379c16de-2ee9-4783-b16c-34bd1925dc84.txt");
let sdk = fs.readFileSync(sdkPath, "utf8");
// 诊断补丁：dx VM 的 bind 操作缺成员时，通过 fetch 上报缺的是哪个对象的哪个属性。
const bindNeedle = "bn[t(4)](n,bn[t(10)](e)[bn[t(10)](r)][t(28)](bn.get(e)))";
const bindPatch = "bn[t(4)](n,(()=>{const o=bn.get(e),k=bn.get(r);if(o==null||o[k]===undefined){window.fetch('https://lab.invalid/bindmiss?k='+encodeURIComponent(String(k))+'&o='+encodeURIComponent(Object.prototype.toString.call(o))+'&keys='+encodeURIComponent(o&&typeof o==='object'?Object.getOwnPropertyNames(o).join(','):'')+'&kn='+kn)}return o[k].bind(o)})())";
if (process.argv.includes("--diag")) {
  if (!sdk.includes(bindNeedle)) console.log("!! bind needle not found");
  sdk = sdk.replace(bindNeedle, bindPatch);
}

const b64d = (s) => Buffer.from(s, "base64").toString("latin1");
const b64e = (s) => Buffer.from(s, "latin1").toString("base64");
const xor = (t, k) => { let r = ""; for (let i = 0; i < t.length; i++) r += String.fromCharCode(t.charCodeAt(i) ^ k.charCodeAt(i % k.length)); return r; };
const reenc = (dx, pReal, pNew) => (dx ? b64e(xor(xor(b64d(dx), pReal), pNew)) : dx);

const capture = process.argv.includes("--capture");
let errBuf = "";
const child = spawn(process.execPath, [path.join(__dirname, "..", "sentinel_node.js")], { stdio: ["pipe", "pipe", capture ? "pipe" : "inherit"] });
if (capture) child.stderr.on("data", (d) => { errBuf += d.toString("utf8"); });
const rl = readline.createInterface({ input: child.stdout });
const waiters = [];
rl.on("line", (line) => {
  let m; try { m = JSON.parse(line); } catch { return; }
  if (m.type === "fetch") return onFetch(m);
  if (m.type === "hello") return;
  const w = waiters.shift();
  if (w) w(m); else console.log("unexpected msg", m.type);
});
function send(o) { child.stdin.write(JSON.stringify(o) + "\n"); }
function next() { return new Promise((r) => waiters.push(r)); }

function onFetch(m) {
  if (/lab\.invalid/.test(m.url)) {
    console.log("DIAG", decodeURIComponent(m.url.replace("https://lab.invalid/", "")));
    send({ type: "fetch_ok", id: m.id, status: 204, headers: {}, body: "" });
    return;
  }
  let body = {};
  try { body = JSON.parse(m.body || "{}"); } catch {}
  if (/sentinel\/req/.test(m.url) && body.p) {
    const resp = {
      persona: "chatgpt-noauth", token: pair.token || "gAAAAAB_mock_token", expire_after: 540, expire_at: Math.floor(Date.now() / 1000) + 540,
      turnstile: { required: true, dx: reenc(pair.dx, pair.p, body.p) },
      proofofwork: pair.pow || { required: false },
      so: { required: true, collector_dx: reenc(pair.collector_dx, pair.p, body.p), snapshot_dx: reenc(pair.snapshot_dx, pair.p, body.p) },
    };
    send({ type: "fetch_ok", id: m.id, status: 200, headers: { "content-type": "application/json" }, body: JSON.stringify(resp) });
    return;
  }
  send({ type: "fetch_ok", id: m.id, status: 204, headers: {}, body: "" });
}

(async () => {
  const href = pair.flow === "oauth_create_account" ? "https://auth.openai.com/about-you" : "https://auth.openai.com/email-verification";
  const snap = { href, scripts: [], dataBuild: "", loadedAgoMs: 12000, searchKeys: "", cookies: "oai-did=" + pair.id, iframeCookies: "oai-did=" + pair.id, taskbar: 40, toolbar: 88 };
  send(Object.assign({
    type: "init", sdk, sdkURL: "https://sentinel.openai.com/sentinel/20260810913b/sdk.js",
    ua: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.7680.164 Safari/537.36",
    languages: ["en-US", "en"], deviceID: pair.id, screenWidth: 1920, screenHeight: 1080, chromeMajor: "146", chromeFull: "146.0.7680.164",
    timezone: "America/New_York", locale: "en-US",
  }, snap));
  let r = await next();
  console.log("init ->", r.type);
  send(Object.assign({ type: "page", flow: pair.flow }, snap));
  r = await next();
  console.log("page ->", r.type, r.error || "");
  const msg = Object.assign({ type: "mint", flow: pair.flow }, snap);
  if (withInteract) msg.interact = pair.flow === "oauth_create_account" ? { kind: "profile", name: "Daniel Foster", age: "27" } : { kind: "otp", text: "483920" };
  send(msg);
  r = await next();
  console.log("mint ->", r.type, r.error || "");
  if (r.type === "ok") {
    const tok = JSON.parse(r.token);
    const rawT = b64d(tok.t || "");
    console.log("p_len", (tok.p || "").length, "t_raw_len", rawT.length, "so_len", (r.so || "").length);
    console.log("t head:", JSON.stringify(rawT.slice(0, 100)));
    fs.writeFileSync(path.join(process.env.TEMP || ".", "lab_out.json"), JSON.stringify({ flow: pair.flow, token: r.token, so: r.so }));
  }
  if (capture) {
    const font = (errBuf.match(/set fontFamily = "([^"]*)"/) || [])[1];
    const size = (errBuf.match(/set fontSize = "([^"]*)"/) || [])[1];
    const text = (errBuf.match(/set innerText = ("(?:[^"\\]|\\.)*")/) || [])[1];
    const rec = { i: idx, flow: pair.flow, font, size, text: text ? JSON.parse(text) : null };
    fs.appendFileSync(process.env.CAPTURE_OUT || "capture_out.jsonl", JSON.stringify(rec) + "\n", "utf8");
  }
  child.kill();
  process.exit(0);
})();
