// 用法: node vm_run.js <dx_pairs.json> <index> [scenario]
// 在假窗口里运行真实的 collector_dx，派发事件，再运行 snapshot_dx，打印 so 输出与观察到的窗口读写。
const fs = require("fs");
const { createVM, atobN, btoaN, rt } = require("./vm_lab.js");

const pairs = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const pair = pairs[Number(process.argv[3] || 0)];
const scenario = process.argv[4] || "none";

const listeners = {};
const winLog = [];
const store = {};
const base = {
  performance: { now: () => performance.now(), timeOrigin: performance.timeOrigin },
  Date, Math, Object, Reflect, Number, WeakSet, JSON, String, Array, Promise, Map, Set, Symbol,
  navigator: { webdriver: false, userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.7680.164 Safari/537.36" },
  addEventListener(type, fn, opts) {
    (listeners[type] || (listeners[type] = [])).push({ fn, opts });
    winLog.push("addEventListener(" + type + "," + JSON.stringify(opts) + ")");
  },
  removeEventListener(type, fn) {
    listeners[type] = (listeners[type] || []).filter((l) => l.fn !== fn);
    winLog.push("removeEventListener(" + type + ")");
  },
};
const win = new Proxy(base, {
  get(t, k) {
    if (typeof k === "symbol") return t[k];
    if (!(k in t)) winLog.push("GET(undefined) win." + String(k));
    return t[k];
  },
  set(t, k, v) { t[k] = v; winLog.push("SET win." + String(k) + "=" + (typeof v === "object" ? JSON.stringify(v) : String(v)).slice(0, 80)); return true; },
  has(t, k) { return k in t; },
});
base.window = win;
const doc = { scripts: [], documentElement: {}, hasFocus: () => true, visibilityState: "visible" };

const vm = createVM(win, doc, {
  onGet(o, k) { if (o === win || o === base.navigator) winLog.push("VMGET " + (o === win ? "win." : "navigator.") + String(k)); },
});

const evReads = {};
function dispatch(type, props) {
  const raw = Object.assign({ type, isTrusted: true, timeStamp: performance.now(), bubbles: true, cancelable: true }, props || {});
  const ev = new Proxy(raw, {
    get(t, k) {
      if (typeof k !== "symbol") { (evReads[type] || (evReads[type] = new Set())).add(String(k)); }
      const v = t[k];
      if (k === "target" && v && typeof v === "object") {
        return new Proxy(v, { get(tt, kk) { if (typeof kk !== "symbol") (evReads[type + ".target"] || (evReads[type + ".target"] = new Set())).add(String(kk)); return tt[kk]; } });
      }
      return v;
    },
  });
  for (const l of listeners[type] || []) l.fn(ev);
}

(async () => {
  console.log("flow:", pair.flow);
  await vm.run(pair.collector_dx, pair.p);
  console.log("collector done; listeners:", Object.keys(listeners).join(","));
  if (process.argv.includes("--log")) console.log(winLog.join("\n"));
  const target = { nodeName: "INPUT", type: "text", disabled: false, readOnly: false, isContentEditable: false, value: "" };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  if (scenario === "human") {
    // 指针移动 -> 点击输入框 -> 输入 6 位 -> 指针移动 -> 点击提交
    let x = 400, y = 300;
    for (let i = 0; i < 30; i++) { x += 12 + Math.random() * 6; y += 4 + Math.random() * 3; dispatch("pointermove", { clientX: x, clientY: y, screenX: x, screenY: y + 80, pointerType: "mouse" }); await sleep(12); }
    dispatch("click", { clientX: x, clientY: y, target, button: 0 });
    await sleep(300);
    for (const ch of "483920") {
      dispatch("keydown", { key: ch, code: "Digit" + ch, ctrlKey: false, metaKey: false, altKey: false, target });
      target.value += ch;
      dispatch("input", { target, inputType: "insertText", data: ch });
      await sleep(90 + Math.random() * 90);
    }
    for (let i = 0; i < 25; i++) { x += 8 + Math.random() * 5; y += 10 + Math.random() * 4; dispatch("pointermove", { clientX: x, clientY: y, screenX: x, screenY: y + 80, pointerType: "mouse" }); await sleep(12); }
    dispatch("click", { clientX: x, clientY: y, target: { nodeName: "BUTTON" }, button: 0 });
  } else if (scenario === "untrusted") {
    for (const ch of "483920") { dispatch("keydown", { key: ch, isTrusted: false, target }); dispatch("input", { target, isTrusted: false }); }
  }
  for (const k of Object.keys(evReads)) console.log("event reads", k, "->", [...evReads[k]].join(","));
  const so = await vm.run(pair.snapshot_dx);
  console.log("so length:", so.length);
  const counters = Object.keys(base).filter((k) => k.startsWith("__oai_so_") && typeof base[k] !== "function");
  console.log("counters:", counters.map((k) => k.replace("__oai_so_", "") + "=" + JSON.stringify(base[k])).join(" "));
  console.log("so head:", so.slice(0, 80));
  fs.writeFileSync(process.env.TEMP + "/vm_so_" + scenario + ".txt", so);
  const St = vm.St;
  console.log("register16(key)=", String(St.get(16)).slice(0, 60));
  const { xorLog } = require("./vm_lab.js");
  console.log("xor calls with long input:", xorLog.length);
  for (const x of xorLog.slice(-6)) console.log(JSON.stringify(x).slice(0, 700));
  // 直接解 so：假设 so = btoa(xor(plain, keyStr))，尝试用 register16 与 token 作为 key
  const raw = atobN(so);
  console.log("so raw len", raw.length);
})().catch((e) => { console.error("ERR", e && e.stack || e); if (process.argv.includes("--log")) console.log(winLog.slice(-40).join("\n")); process.exit(1); });
