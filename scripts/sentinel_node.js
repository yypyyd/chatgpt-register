#!/usr/bin/env node
"use strict";

const vm = require("vm");
const readline = require("readline");
const { webcrypto } = require("crypto");
const { TextEncoder, TextDecoder } = require("util");
const { performance: nodePerf } = require("perf_hooks");

const HostEvent = typeof Event === "function" ? Event : class Event {
  constructor(type, init) {
    this.type = type;
    Object.assign(this, init || {});
  }
};
const HostMessageEvent = typeof MessageEvent === "function" ? MessageEvent : class MessageEvent extends HostEvent {};
const HostAbortController = typeof AbortController === "function" ? AbortController : class AbortController {
  constructor() {
    this.signal = { aborted: false, addEventListener() {} };
  }
  abort() {
    this.signal.aborted = true;
  }
};

const nativeFns = new WeakSet();
const nativeNames = new WeakMap();
const scriptBags = new WeakMap();
const dataBuildSetters = new WeakMap();
const timeOrigins = new WeakMap();
const historyLens = new WeakMap();
const RealFunction = Function;
let activeCtx = null;

function nativeCode(name) {
  return "function " + (name || "") + "() { [native code] }";
}

function asNative(fn, name) {
  if (typeof fn !== "function") return fn;
  nativeFns.add(fn);
  nativeNames.set(fn, name || fn.name || "");
  try {
    Object.defineProperty(fn, "name", { value: name || fn.name || "", configurable: true });
  } catch (e) {}
  return fn;
}

const hostFnToString = Function.prototype.toString;
Function.prototype.toString = function () {
  if (nativeFns.has(this)) {
    return nativeCode(nativeNames.get(this) || this.name || "");
  }
  return hostFnToString.call(this);
};
asNative(Function.prototype.toString, "toString");

let currentTZ = process.env.TZ || "";

function activeTimeZone() {
  return currentTZ || (env && env.timezone) || process.env.TZ || "";
}

// Intl.DateTimeFormat 构造很贵（首次尤其慢），按时区缓存；否则 PoW 的 elapsed 会比真实 Chrome 慢一个数量级。
const dtfCache = new Map();
function cachedDTF(key, make) {
  let f = dtfCache.get(key);
  if (!f) { f = make(); dtfCache.set(key, f); }
  return f;
}

function chromeDateParts(d) {
  const tz = activeTimeZone();
  const opts = {
    weekday: "short",
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hourCycle: "h23",
    hour12: false,
    timeZoneName: "long",
  };
  if (tz) opts.timeZone = tz;
  const bag = {};
  for (const p of cachedDTF("main:" + tz, () => new Intl.DateTimeFormat("en-US", opts)).formatToParts(d)) {
    if (p.type !== "literal") bag[p.type] = p.value;
  }
  let offset = "GMT+0000";
  try {
    const offOpts = { timeZoneName: "longOffset", hour: "numeric" };
    if (tz) offOpts.timeZone = tz;
    const off = cachedDTF("off:" + tz, () => new Intl.DateTimeFormat("en-US", offOpts)).formatToParts(d).find((p) => p.type === "timeZoneName");
    const m = String(off && off.value || "").match(/GMT([+-])(\d{1,2})(?::?(\d{2}))?/i);
    if (m) {
      offset = "GMT" + m[1] + String(m[2]).padStart(2, "0") + String(m[3] || "00");
    }
  } catch (e) {}
  let tzName = bag.timeZoneName || "GMT";
  if (/^GMT/i.test(tzName) || /[\u4e00-\u9fff]/.test(tzName)) {
    tzName = tzName.indexOf("-") >= 0 || tzName.indexOf("+") >= 0 ? tzName : "GMT";
  }
  const hour = String(bag.hour || "00").padStart(2, "0");
  return {
    weekday: bag.weekday || "Thu",
    month: bag.month || "Jan",
    day: String(parseInt(bag.day || "1", 10)).padStart(2, "0"),
    year: bag.year || "1970",
    hour,
    minute: String(bag.minute || "00").padStart(2, "0"),
    second: String(bag.second || "00").padStart(2, "0"),
    offset,
    tzName,
  };
}

function chromeDateToString(d) {
  const p = chromeDateParts(d instanceof Date ? d : new Date());
  const name = /^GMT/i.test(p.tzName) ? p.offset : p.tzName;
  return p.weekday + " " + p.month + " " + p.day + " " + p.year + " " +
    p.hour + ":" + p.minute + ":" + p.second + " " + p.offset + " (" + name + ")";
}

function chromeDateToDateString(d) {
  const p = chromeDateParts(d instanceof Date ? d : new Date());
  return p.weekday + " " + p.month + " " + p.day + " " + p.year;
}

function chromeDateToTimeString(d) {
  const p = chromeDateParts(d instanceof Date ? d : new Date());
  const name = /^GMT/i.test(p.tzName) ? p.offset : p.tzName;
  return p.hour + ":" + p.minute + ":" + p.second + " " + p.offset + " (" + name + ")";
}

Date.prototype.toString = function () { return chromeDateToString(this); };
Date.prototype.toDateString = function () { return chromeDateToDateString(this); };
Date.prototype.toTimeString = function () { return chromeDateToTimeString(this); };
asNative(Date.prototype.toString, "toString");
asNative(Date.prototype.toDateString, "toDateString");
asNative(Date.prototype.toTimeString, "toTimeString");

Error.prepareStackTrace = function (err) {
  const name = (err && err.name) || "Error";
  const msg = err && err.message != null ? String(err.message) : "";
  const head = msg ? name + ": " + msg : name;
  const file = sdkURL || "https://chatgpt.com/sentinel/sdk.js";
  return head + "\n    at " + file + ":1:1";
};

function runInSandbox(code, ctx, filename) {
  const prev = activeCtx;
  activeCtx = ctx;
  try {
    if (!vm.isContext(ctx)) vm.createContext(ctx);
    return vm.runInContext(code, ctx, { filename: filename || sdkURL || "sdk.js" });
  } finally {
    activeCtx = prev;
  }
}

function SafeFunction(...args) {
  const body = args.length ? String(args[args.length - 1]) : "";
  const params = args.slice(0, -1).map(String);
  const ctx = activeCtx || parentCtx;
  if (!ctx) {
    return asNative(function () { return undefined; }, "anonymous");
  }
  return runInSandbox("(function(" + params.join(",") + "){\n" + body + "\n})", ctx, sdkURL);
}
asNative(SafeFunction, "Function");
SafeFunction.prototype = RealFunction.prototype;
try {
  Object.defineProperty(RealFunction.prototype, "constructor", {
    value: SafeFunction,
    writable: true,
    configurable: true,
  });
} catch (e) {}

const CHROME_WINDOW_NAMES = [
  "name", "status", "closed", "length", "opener", "origin", "history", "localStorage", "sessionStorage",
  "indexedDB", "isSecureContext", "crossOriginIsolated", "originAgentCluster", "scheduler", "trustedTypes",
  "cookieStore", "navigation", "visualViewport", "speechSynthesis", "caches", "customElements",
  "locationbar", "menubar", "personalbar", "scrollbars", "statusbar", "toolbar", "external",
  "scrollX", "scrollY", "pageXOffset", "pageYOffset", "screenX", "screenY", "screenLeft", "screenTop",
  "styleMedia", "alert", "confirm", "prompt", "print", "focus", "blur", "close", "stop", "open",
  "getComputedStyle", "getSelection", "matchMedia", "moveTo", "moveBy", "resizeTo", "resizeBy",
  "scroll", "scrollTo", "scrollBy", "requestAnimationFrame", "cancelAnimationFrame",
  "structuredClone", "reportError", "queueMicrotask", "DOMParser", "XMLSerializer",
  "MutationObserver", "ResizeObserver", "IntersectionObserver", "FileReader", "Blob", "File",
  "FormData", "Headers", "Request", "Response", "CustomEvent", "PointerEvent", "MouseEvent",
  "KeyboardEvent", "ErrorEvent", "PromiseRejectionEvent", "HTMLElement", "HTMLIFrameElement",
  "HTMLScriptElement", "HTMLCanvasElement", "Node", "Document", "Element", "WebSocket",
  "EventSource", "Worker", "BroadcastChannel", "MessageChannel", "MessagePort", "Notification",
  "Image", "Audio", "Option", "CSS", "PluginArray", "MimeTypeArray", "Navigator", "Screen",
  "History", "Location", "Storage", "PerformanceObserver",
];
const CHROME_WINDOW_EVENTS = [
  "onabort", "onafterprint", "onbeforeprint", "onbeforeunload", "onblur", "oncancel", "oncanplay",
  "oncanplaythrough", "onchange", "onclick", "onclose", "oncontextmenu", "oncuechange", "ondblclick",
  "ondrag", "ondragend", "ondragenter", "ondragleave", "ondragover", "ondragstart", "ondrop",
  "ondurationchange", "onemptied", "onended", "onerror", "onfocus", "onformdata", "onhashchange",
  "oninput", "oninvalid", "onkeydown", "onkeypress", "onkeyup", "onload", "onloadeddata",
  "onloadedmetadata", "onloadstart", "onmessage", "onmessageerror", "onmousedown", "onmouseenter",
  "onmouseleave", "onmousemove", "onmouseout", "onmouseover", "onmouseup", "onmousewheel",
  "onoffline", "ononline", "onpagehide", "onpageshow", "onpause", "onplay", "onplaying", "onpopstate",
  "onprogress", "onratechange", "onrejectionhandled", "onreset", "onresize", "onscroll",
  "onsecuritypolicyviolation", "onseeked", "onseeking", "onselect", "onslotchange", "onstalled",
  "onstorage", "onsubmit", "onsuspend", "ontimeupdate", "ontoggle", "onunhandledrejection",
  "onunload", "onvolumechange", "onwaiting", "onwheel", "onauxclick", "ongotpointercapture",
  "onlostpointercapture", "onpointerdown", "onpointermove", "onpointerup", "onpointercancel",
  "onpointerover", "onpointerout", "onpointerenter", "onpointerleave",
];

let sdkSource = "";
let sdkURL = "https://chatgpt.com/sentinel/sdk.js";
let env = {};
let parentCtx = null;
let mintBusy = Promise.resolve();

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

function log(msg) {
  process.stderr.write(String(msg) + "\n");
}

function asHeaders(raw) {
  if (!raw) return {};
  if (typeof raw.forEach === "function") {
    const out = {};
    raw.forEach((v, k) => {
      out[k] = v;
    });
    return out;
  }
  if (Array.isArray(raw)) {
    const out = {};
    for (const pair of raw) {
      if (pair && pair.length >= 2) out[pair[0]] = pair[1];
    }
    return out;
  }
  return { ...raw };
}

let fetchSeq = 1;
const fetchWait = new Map();

function hookedFetch(url, init) {
  const id = fetchSeq++;
  const method = String((init && init.method) || "GET").toUpperCase();
  const headers = asHeaders(init && init.headers);
  const body = init && init.body != null ? String(init.body) : "";
  return new Promise((resolve, reject) => {
    fetchWait.set(id, { resolve, reject });
    send({ type: "fetch", id, url: String(url), method, headers, body });
  });
}
asNative(hookedFetch, "fetch");

function finishFetch(msg) {
  const pending = fetchWait.get(msg.id);
  if (!pending) return;
  fetchWait.delete(msg.id);
  if (msg.error) {
    pending.reject(new Error(msg.error));
    return;
  }
  const status = Number(msg.status) || 0;
  const body = msg.body == null ? "" : String(msg.body);
  pending.resolve({
    ok: status >= 200 && status < 300,
    status,
    headers: { get: (k) => (msg.headers && msg.headers[k]) || (msg.headers && msg.headers[String(k).toLowerCase()]) || null },
    json: async () => (body ? JSON.parse(body) : {}),
    text: async () => body,
  });
}

function makeLocation(href) {
  const u = new URL(href);
  return {
    href: u.href,
    origin: u.origin,
    pathname: u.pathname,
    search: u.search,
    hostname: u.hostname,
    host: u.host,
    protocol: u.protocol,
    hash: u.hash,
    toString() {
      return this.href;
    },
  };
}

function hidden(obj, key, value, writable) {
  Object.defineProperty(obj, key, { value, writable: writable !== false, enumerable: false, configurable: true });
}

function expose(obj, key, value, writable) {
  Object.defineProperty(obj, key, { value, writable: writable !== false, enumerable: true, configurable: true });
}

const HIDDEN_WINDOW_BUILTINS = [
  "globalThis", "fetch", "Function", "eval", "Promise", "Map", "WeakMap", "Set", "Array", "Object",
  "JSON", "Math", "Date", "Number", "String", "Boolean", "Error", "TypeError", "Uint8Array",
  "Uint8ClampedArray", "ArrayBuffer", "TextEncoder", "TextDecoder", "URL", "URLSearchParams",
  "atob", "btoa", "setTimeout", "clearTimeout", "setInterval", "clearInterval", "Intl",
  "AbortController", "AbortSignal", "Event", "MessageEvent", "DOMException", "crypto",
  "performance", "addEventListener", "removeEventListener", "dispatchEvent", "postMessage",
  "requestIdleCallback", "cancelIdleCallback", "queueMicrotask", "structuredClone", "reportError",
  "AudioContext", "webkitAudioContext", "OfflineAudioContext", "OffscreenCanvas",
  "WebGLRenderingContext", "WebGL2RenderingContext", "SVGElement",
];

function makeMimeType(type, suffixes, description, plugin) {
  return { type, suffixes, description, enabledPlugin: plugin };
}

function makePlugin(name, filename, description, mimes) {
  const plugin = { name, filename, description, length: mimes.length };
  mimes.forEach((m, i) => {
    plugin[i] = m;
    m.enabledPlugin = plugin;
  });
  plugin.item = asNative(function item(i) { return plugin[i] || null; }, "item");
  plugin.namedItem = asNative(function namedItem(n) {
    return mimes.find((m) => m.type === n) || null;
  }, "namedItem");
  return plugin;
}

function chromePdfPlugins() {
  const mimeDefs = [
    ["application/pdf", "pdf", "Portable Document Format"],
    ["text/pdf", "pdf", "Portable Document Format"],
  ];
  const names = [
    "PDF Viewer",
    "Chrome PDF Viewer",
    "Chromium PDF Viewer",
    "Microsoft Edge PDF Viewer",
    "WebKit built-in PDF",
  ];
  return names.map((name) => {
    const mimes = mimeDefs.map((d) => makeMimeType(d[0], d[1], d[2], null));
    return makePlugin(name, "internal-pdf-viewer", "Portable Document Format", mimes);
  });
}

function makePluginArray() {
  const list = chromePdfPlugins();
  const arr = { length: list.length };
  list.forEach((p, i) => { arr[i] = p; });
  arr.item = asNative(function item(i) { return arr[i] || null; }, "item");
  arr.namedItem = asNative(function namedItem(n) {
    return list.find((p) => p.name === n) || null;
  }, "namedItem");
  arr.refresh = asNative(function refresh() {}, "refresh");
  return arr;
}

function makeMimeTypeArray(plugins) {
  const first = plugins && plugins[0];
  const list = first ? [first[0], first[1]].filter(Boolean) : [];
  const arr = { length: list.length };
  list.forEach((m, i) => { arr[i] = m; arr[m.type] = m; });
  arr.item = asNative(function item(i) { return arr[i] || null; }, "item");
  arr.namedItem = asNative(function namedItem(n) { return arr[n] || null; }, "namedItem");
  return arr;
}

function AudioContextStub() {
  this.sampleRate = 48000;
  this.state = "running";
  this.baseLatency = 0.01;
  this.outputLatency = 0.02;
  this.currentTime = 0;
  this.destination = { maxChannelCount: 2, channelCount: 2 };
  this.createOscillator = asNative(function createOscillator() {
    return { type: "sine", frequency: { value: 440 }, connect() {}, start() {}, stop() {}, disconnect() {} };
  }, "createOscillator");
  this.createAnalyser = asNative(function createAnalyser() {
    return {
      fftSize: 2048, frequencyBinCount: 1024, connect() {}, disconnect() {},
      getFloatFrequencyData(a) { if (a && a.fill) a.fill(-120); },
      getByteFrequencyData(a) { if (a && a.fill) a.fill(0); },
    };
  }, "createAnalyser");
  this.createGain = asNative(function createGain() {
    return { gain: { value: 1 }, connect() {}, disconnect() {} };
  }, "createGain");
  this.close = asNative(function close() { this.state = "closed"; return Promise.resolve(); }, "close");
}
asNative(AudioContextStub, "AudioContext");

// Chrome 的 GREASE 品牌算法（品牌名/版本/顺序都由主版本号决定），与真实 Chrome 154 逐项核对一致。
function greaseBrands(majorStr, fullVersion) {
  const major = parseInt(majorStr, 10) || 146;
  const chars = [" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"];
  const brand = "Not" + chars[major % chars.length] + "A" + chars[(major + 1) % chars.length] + "Brand";
  const gv = ["8", "99", "24"][major % 3];
  const orders = [[0, 1, 2], [0, 2, 1], [1, 0, 2], [1, 2, 0], [2, 0, 1], [2, 1, 0]];
  const o = orders[major % 6];
  const out = [null, null, null];
  out[o[0]] = { brand, version: fullVersion ? gv + ".0.0.0" : gv };
  out[o[1]] = { brand: "Chromium", version: fullVersion ? fullVersion : String(major) };
  out[o[2]] = { brand: "Google Chrome", version: fullVersion ? fullVersion : String(major) };
  return out;
}

function makeNavigator(opts) {
  const langs = Array.isArray(opts.languages) && opts.languages.length ? opts.languages : ["en-US", "en"];
  const major = String(opts.chromeMajor || "146");
  const full = String(opts.chromeFull || major + ".0.0.0");
  const brands = greaseBrands(major, null);
  const fullVersionList = greaseBrands(major, full);
  const uaData = {
    brands,
    mobile: false,
    platform: "Windows",
    getHighEntropyValues: asNative(function getHighEntropyValues(hints) {
      const out = { brands, mobile: false, platform: "Windows" };
      const want = Array.isArray(hints) ? hints : [];
      const extra = {
        architecture: "x86",
        bitness: "64",
        model: "",
        platformVersion: "15.0.0",
        uaFullVersion: full,
        fullVersionList,
        wow64: false,
      };
      for (const h of want) {
        if (h in extra) out[h] = extra[h];
      }
      if (!want.length) Object.assign(out, extra);
      return Promise.resolve(out);
    }, "getHighEntropyValues"),
    toJSON: asNative(function toJSON() {
      return { brands, mobile: false, platform: "Windows" };
    }, "toJSON"),
  };
  function Navigator() {}
  asNative(Navigator, "Navigator");
  const nav = Object.create(Navigator.prototype);
  Object.defineProperty(Navigator.prototype, Symbol.toStringTag, { value: "Navigator", configurable: true });
  const plugins = makePluginArray();
  const fields = {
    userAgent: opts.ua,
    platform: "Win32",
    language: langs[0],
    languages: langs.slice(),
    hardwareConcurrency: Number(opts.hwc) > 0 ? Number(opts.hwc) : 8,
    deviceMemory: Number(opts.mem) > 0 ? Number(opts.mem) : 8,
    maxTouchPoints: Number(opts.touch) > 0 ? Number(opts.touch) : 0,
    webdriver: false,
    vendor: "Google Inc.",
    vendorSub: "",
    product: "Gecko",
    productSub: "20030107",
    appCodeName: "Mozilla",
    appName: "Netscape",
    appVersion: String(opts.ua || "").replace(/^Mozilla\//, ""),
    cookieEnabled: true,
    onLine: true,
    pdfViewerEnabled: true,
    userAgentData: uaData,
    plugins,
    mimeTypes: makeMimeTypeArray(plugins),
    connection: { effectiveType: "4g", rtt: 50, downlink: 10, saveData: false },
    locks: { request() { return Promise.resolve(); }, query() { return Promise.resolve({ held: [], pending: [] }); } },
    permissions: { query() { return Promise.resolve({ state: "prompt" }); } },
    clipboard: { writeText() { return Promise.resolve(); }, readText() { return Promise.resolve(""); } },
    storage: { estimate() { return Promise.resolve({ quota: 1 << 30, usage: 1 << 20 }); } },
    wakeLock: { request() { return Promise.resolve({ released: false, release() { return Promise.resolve(); } }); } },
    hid: { getDevices() { return Promise.resolve([]); } },
    usb: { getDevices() { return Promise.resolve([]); } },
    serial: { getPorts() { return Promise.resolve([]); } },
    gpu: undefined,
    doNotTrack: null,
  };
  // 真实 Chrome 里 Navigator.prototype 有 83 个可枚举成员（属性是访问器、方法是函数），
  // 实例自身没有任何自有属性。官方 SDK/dx 会 Object.keys(Object.getPrototypeOf(navigator)) 随机取一个
  // 再读 navigator[key].toString()，所以每个成员都要存在且 toString 形态与真实一致。
  const objTag = (cls, extra) => {
    const o = Object.assign({}, extra || {});
    Object.defineProperty(o, Symbol.toStringTag, { value: cls, configurable: true });
    return o;
  };
  const cache = Object.create(null);
  let navKeys = [];
  try { navKeys = require("./nav_keys.js"); } catch (e) { navKeys = []; }
  const known = new Set(navKeys.map((r) => r[0]));
  for (const [name, kind, cls] of navKeys) {
    if (kind === "fn") {
      const fn = asNative(function () { return undefined; }, name);
      Object.defineProperty(Navigator.prototype, name, { value: fn, writable: true, enumerable: true, configurable: true });
      continue;
    }
    let getter;
    if (Object.prototype.hasOwnProperty.call(fields, name) && fields[name] !== undefined) {
      const v = fields[name];
      if (name === "plugins") Object.defineProperty(v, Symbol.toStringTag, { value: "PluginArray", configurable: true });
      if (name === "mimeTypes") Object.defineProperty(v, Symbol.toStringTag, { value: "MimeTypeArray", configurable: true });
      if (name === "userAgentData") Object.defineProperty(v, Symbol.toStringTag, { value: "NavigatorUAData", configurable: true });
      getter = () => v;
    } else if (kind === "null") {
      getter = () => null;
    } else if (kind === "obj") {
      getter = () => (cache[name] || (cache[name] = objTag(cls)));
    } else if (kind === "number") {
      getter = () => 0;
    } else if (kind === "boolean") {
      getter = () => false;
    } else {
      getter = () => "";
    }
    asNative(getter, "get " + name);
    Object.defineProperty(Navigator.prototype, name, { get: getter, enumerable: true, configurable: true });
  }
  // 兜底：模块缺失时仍保留原先的字段（非枚举）。
  for (const [k, v] of Object.entries(fields)) {
    if (known.has(k)) continue;
    Object.defineProperty(Navigator.prototype, k, { get: () => v, enumerable: false, configurable: true });
  }
  return nav;
}

// 真实 Web Storage 语义：键就是自有可枚举属性（Object.keys(localStorage) 只列已存的键），
// length/key/getItem/setItem/removeItem/clear 在原型上且不可枚举。
function makeStorage(initial) {
  function Storage() {}
  asNative(Storage, "Storage");
  const proto = Storage.prototype;
  Object.defineProperty(proto, Symbol.toStringTag, { value: "Storage", configurable: true });
  const store = Object.create(proto);
  const reserved = new Set(["length", "key", "getItem", "setItem", "removeItem", "clear"]);
  const def = (name, fn) => Object.defineProperty(proto, name, { value: asNative(fn, name), writable: true, enumerable: false, configurable: true });
  Object.defineProperty(proto, "length", {
    get: asNative(function () { return Object.keys(store).length; }, "get length"),
    enumerable: false, configurable: true,
  });
  def("key", function key(i) { return Object.keys(store)[Number(i)] || null; });
  def("getItem", function getItem(k) {
    k = String(k);
    return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null;
  });
  def("setItem", function setItem(k, v) {
    k = String(k);
    if (reserved.has(k)) return;
    Object.defineProperty(store, k, { value: String(v), writable: true, enumerable: true, configurable: true });
  });
  def("removeItem", function removeItem(k) { delete store[String(k)]; });
  def("clear", function clear() { for (const k of Object.keys(store)) delete store[k]; });
  for (const [k, v] of Object.entries(initial || {})) store.setItem(k, v);
  return store;
}

// 官方认证页（Remix）里 Statsig 写进 localStorage 的键。后缀是公开 SDK key 的哈希，所有用户相同；
// 资料页多一条 16 位十六进制的用户级缓存键。抓包逐项核对。
function statsigStorage(pageHref) {
  const now = Date.now();
  const sid = "444584300";
  const items = {
    "statsig.cached.evaluations.3341434239": JSON.stringify({ source: "Network", data: "{}", receivedAt: now - 9000, stableID: webcrypto.randomUUID(), fullUserHash: null }),
    "statsig.session_id.444584300": JSON.stringify({ sessionID: webcrypto.randomUUID(), startTime: now - 12000, lastUpdate: now - 1500 }),
    "statsig.last_modified_time.evaluations": JSON.stringify({ [String(3341434239)]: now - 9000 }),
    "statsig.stable_id.444584300": JSON.stringify(webcrypto.randomUUID()),
  };
  if (/about-you/.test(String(pageHref || ""))) {
    const hex = Array.from({ length: 16 }, () => "0123456789abcdef"[Math.floor(Math.random() * 16)]).join("");
    const withExtra = {};
    const keys = Object.keys(items);
    withExtra[keys[0]] = items[keys[0]];
    withExtra[hex] = JSON.stringify({ v: 1 });
    for (const k of keys.slice(1)) withExtra[k] = items[k];
    return withExtra;
  }
  return items;
}

function webglStub() {
  return {
    getParameter(p) {
      if (p === 37445) return "Google Inc. (NVIDIA)";
      if (p === 37446) return "ANGLE (NVIDIA, NVIDIA GeForce GTX 1660 Direct3D11 vs_5_0 ps_5_0, D3D11)";
      return null;
    },
    getExtension() { return null; },
    getSupportedExtensions() { return ["WEBGL_debug_renderer_info"]; },
    createBuffer() { return {}; },
    bindBuffer() {},
    bufferData() {},
  };
}

function seedCookieMap(raw, deviceID) {
  const map = {};
  if (deviceID) map["oai-did"] = deviceID;
  for (const part of String(raw || "").split(";")) {
    const s = part.trim();
    const i = s.indexOf("=");
    if (i > 0) map[s.slice(0, i).trim()] = s.slice(i + 1).trim();
  }
  return map;
}

function applyCookies(doc, raw) {
  if (!doc || raw == null || raw === "") return;
  for (const part of String(raw).split(";")) {
    const s = part.trim();
    if (s) doc.cookie = s;
  }
}

function makeDocument(opts) {
  const cookieMap = seedCookieMap(opts.cookies, opts.deviceID || "");
  const scripts = Array.isArray(opts.scripts) && opts.scripts.length
    ? opts.scripts.map((src) => ({ src }))
    : [{ src: opts.sdkURL }];
  let dataBuild = opts.dataBuild || "";
  const loc = opts.location;
  const langs = opts.languages || ["en-US"];
  const bodyChildren = [];

  function createElement(tag) {
    tag = String(tag || "").toLowerCase();
    if (tag === "iframe") return opts.createIframe ? opts.createIframe() : { style: {}, src: "", contentWindow: null, addEventListener() {} };
    if (tag === "script") return { src: "", async: true, defer: true, type: "", onload: null, onerror: null };
    if (tag === "canvas") {
      return {
        width: 300,
        height: 150,
        style: {},
        getContext(type) {
          if (String(type).toLowerCase().includes("webgl")) return webglStub();
          return {
            fillRect() {},
            fillText() {},
            measureText() { return { width: 0 }; },
            getImageData() { return { data: new Uint8ClampedArray(4) }; },
          };
        },
        toDataURL() { return "data:image/png;base64,iVBORw0KGgo="; },
      };
    }
    const el = makeGenericElement(tag, opts, () => (loc && loc.href) || "");
    if (process.env.SENTINEL_TRACE_ELEMENTS) {
      const tr = (name, target) => new Proxy(target, {
        get(t, k) {
          if (typeof k !== "symbol") log("[el " + name + "] get " + String(k) + " -> " + typeof t[k]);
          return t[k];
        },
        set(t, k, v) {
          log("[el " + name + "] set " + String(k) + " = " + JSON.stringify(v).slice(0, 80));
          t[k] = v;
          return true;
        },
      });
      el.style = tr(tag + ".style", el.style);
      return tr(tag, el);
    }
    return el;
  }

  const documentElement = {
    lang: langs[0] || "en",
    getAttribute(name) {
      if (name === "data-build") return dataBuild || null;
      if (name === "lang") return langs[0] || "en";
      return null;
    },
    setAttribute() {},
  };

  const doc = {};
  const defs = {
    nodeType: 9,
    readyState: "complete",
    visibilityState: "visible",
    hidden: false,
    title: opts.title || "",
    referrer: opts.referrer || "",
    documentElement,
    currentScript: { src: opts.sdkURL },
    body: (() => {
      const b = makeGenericElement("body", opts, () => (loc && loc.href) || "");
      b.children = bodyChildren;
      b.appendChild = asNative(function appendChild(el) {
        if (el) { bodyChildren.push(el); el.parentNode = b; }
        return el;
      }, "appendChild");
      b.removeChild = asNative(function removeChild(el) {
        const i = bodyChildren.indexOf(el);
        if (i >= 0) bodyChildren.splice(i, 1);
        if (el) el.parentNode = null;
        return el;
      }, "removeChild");
      return b;
    })(),
    head: { appendChild() {} },
    createElement,
    createElementNS(_ns, tag) { return createElement(tag); },
    getElementById() { return null; },
    getElementsByTagName(tag) {
      if (String(tag).toLowerCase() === "script") return scripts;
      return [];
    },
    getElementsByClassName() { return []; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
    hasFocus() { return true; },
  };
  asNative(createElement, "createElement");
  asNative(defs.createElementNS, "createElementNS");
  asNative(defs.getElementById, "getElementById");
  asNative(defs.getElementsByTagName, "getElementsByTagName");
  asNative(defs.getElementsByClassName, "getElementsByClassName");
  asNative(defs.querySelector, "querySelector");
  asNative(defs.querySelectorAll, "querySelectorAll");
  asNative(defs.addEventListener, "addEventListener");
  asNative(defs.removeEventListener, "removeEventListener");
  asNative(defs.hasFocus, "hasFocus");
  for (const [k, v] of Object.entries(defs)) hidden(doc, k, v);
  Object.defineProperty(doc, "scripts", { get() { return scripts; }, enumerable: false, configurable: true });
  // 真实 Chrome：Object.keys(document) === ["location"]；React 水合后再多一个 _reactListening<随机> 键。
  Object.defineProperty(doc, "location", { get() { return loc; }, enumerable: true, configurable: true });
  Object.defineProperty(doc, "_reactListening" + Math.random().toString(36).slice(2, 13), { value: true, writable: true, enumerable: true, configurable: true });
  Object.defineProperty(doc, "URL", { get() { return loc.href; }, enumerable: false, configurable: true });
  Object.defineProperty(doc, "documentURI", { get() { return loc.href; }, enumerable: false, configurable: true });
  Object.defineProperty(doc, "domain", { get() { return loc.hostname; }, enumerable: false, configurable: true });
  Object.defineProperty(doc, "cookie", {
    enumerable: false,
    configurable: true,
    get() {
      return Object.keys(cookieMap)
        .filter((k) => cookieMap[k])
        .map((k) => k + "=" + cookieMap[k])
        .join("; ");
    },
    set(v) {
      const s = String(v || "").split(";")[0];
      const i = s.indexOf("=");
      if (i > 0) cookieMap[s.slice(0, i).trim()] = s.slice(i + 1).trim();
    },
  });
  scriptBags.set(doc, scripts);
  dataBuildSetters.set(doc, (v) => { dataBuild = v == null ? "" : String(v); });
  return doc;
}

function installChromeSurface(win, popts) {
  const noop = asNative(function () {}, "");
  const stubs = {
    name: "",
    status: "",
    closed: false,
    opener: null,
    history: { length: 1, state: null, pushState: noop, replaceState: noop, go: noop, back: noop, forward: noop },
    localStorage: makeStorage(statsigStorage(popts && popts.href)),
    sessionStorage: makeStorage(),
    indexedDB: { open() { return { result: null, addEventListener() {} }; } },
    isSecureContext: true,
    crossOriginIsolated: false,
    originAgentCluster: true,
    scheduler: { postTask(cb) { return Promise.resolve().then(cb); } },
    trustedTypes: { createPolicy() { return { createHTML: (s) => s, createScript: (s) => s, createScriptURL: (s) => s }; } },
    cookieStore: { getAll() { return Promise.resolve([]); }, get() { return Promise.resolve(null); } },
    navigation: { currentEntry: { url: win.location && win.location.href }, addEventListener: noop },
    visualViewport: { width: win.innerWidth, height: win.innerHeight, scale: 1, offsetLeft: 0, offsetTop: 0, addEventListener: noop },
    speechSynthesis: { getVoices() { return []; }, speaking: false, pending: false, paused: false },
    caches: { open() { return Promise.resolve({ match() { return Promise.resolve(undefined); } }); }, keys() { return Promise.resolve([]); } },
    customElements: { define: noop, get() { return undefined; } },
    locationbar: { visible: true },
    menubar: { visible: true },
    personalbar: { visible: true },
    scrollbars: { visible: true },
    statusbar: { visible: true },
    toolbar: { visible: true },
    external: { AddSearchProvider: noop },
    styleMedia: { type: "screen" },
    scrollX: 0,
    scrollY: 0,
    pageXOffset: 0,
    pageYOffset: 0,
    screenX: 0,
    screenY: 0,
    screenLeft: 0,
    screenTop: 0,
    alert: noop,
    confirm() { return true; },
    prompt() { return ""; },
    print: noop,
    focus: noop,
    blur: noop,
    close: noop,
    stop: noop,
    open() { return null; },
    getComputedStyle() { return { getPropertyValue() { return ""; } }; },
    getSelection() { return { toString() { return ""; }, rangeCount: 0 }; },
    matchMedia(q) {
      const qs = String(q || "");
      const matches = /prefers-color-scheme:\s*light/i.test(qs) || /prefers-reduced-motion:\s*no-preference/i.test(qs);
      return { matches, media: qs, addEventListener: noop, addListener: noop, removeListener: noop, onchange: null };
    },
    moveTo: noop,
    moveBy: noop,
    resizeTo: noop,
    resizeBy: noop,
    scroll: noop,
    scrollTo: noop,
    scrollBy: noop,
    requestAnimationFrame(cb) { return setTimeout(() => cb(Date.now()), 16); },
    cancelAnimationFrame(id) { clearTimeout(id); },
    structuredClone(v) { return JSON.parse(JSON.stringify(v)); },
    reportError: noop,
    queueMicrotask,
    DOMParser: class { parseFromString() { return { body: {}, documentElement: {} }; } },
    MutationObserver: class { observe() {} disconnect() {} takeRecords() { return []; } },
    ResizeObserver: class { observe() {} disconnect() {} unobserve() {} },
    IntersectionObserver: class { observe() {} disconnect() {} unobserve() {} },
    Notification: { permission: "default", requestPermission() { return Promise.resolve("default"); } },
  };
  for (const name of CHROME_WINDOW_NAMES) {
    if (Object.prototype.hasOwnProperty.call(win, name)) continue;
    expose(win, name, Object.prototype.hasOwnProperty.call(stubs, name) ? stubs[name] : noop);
  }
  for (const name of CHROME_WINDOW_EVENTS) {
    if (!Object.prototype.hasOwnProperty.call(win, name)) expose(win, name, null);
  }
}

// ---------------------------------------------------------------------------
// 元素盒模型（文本测量指纹）
//
// dx 程序会建一个隐藏的 position:fixed div，设置 fontFamily / fontSize / 一段 zalgo 文字，
// 再读 getBoundingClientRect() —— 这是设备“文字渲染”指纹。官方认证页给该 div 继承了
// letter-spacing:-0.01em（用真实抓包核对，宽度逐位一致）。宽度用本机 Chrome 实测的字号级字簇宽度相加，
// 高度用实测行高；坐标按 float32 取整（与真实数据的小数位一致）。
// ---------------------------------------------------------------------------
let FONT_MODEL = null;
try { FONT_MODEL = require("./font_metrics.js"); } catch (e) { FONT_MODEL = null; }

function fontKeyFor(family) {
  const first = String(family || "").split(",")[0].replace(/["']/g, "").trim().toLowerCase();
  if (FONT_MODEL) {
    const hit = FONT_MODEL.fonts.find((f) => f.toLowerCase() === first);
    if (hit) return hit;
  }
  return "Times New Roman"; // 未安装的字体，Chrome 回落到默认衬线体
}

function measureTextBox(win, style, text, pageHref) {
  const dpr = Number(win.devicePixelRatio) > 0 ? Number(win.devicePixelRatio) : 1;
  const size = parseFloat(style.fontSize) || 16;
  const font = fontKeyFor(style.fontFamily);
  const chars = Array.from(String(text || "").normalize("NFD")).filter((ch) => {
    const cp = ch.codePointAt(0);
    return !(cp >= 0x300 && cp <= 0x36f);
  });
  let width = 0;
  let height = size * 1.15;
  if (FONT_MODEL) {
    const dk = FONT_MODEL.adv[String(dpr)] ? String(dpr) : "1";
    const si = clampN(Math.round(size) - FONT_MODEL.sizes[0], 0, FONT_MODEL.sizes.length - 1);
    const scale = size / FONT_MODEL.sizes[si];
    const table = FONT_MODEL.adv[dk][font] && FONT_MODEL.adv[dk][font][si];
    for (const ch of chars) {
      const idx = FONT_MODEL.letters.indexOf(ch);
      width += (table && idx >= 0 ? table[idx] : size * 0.55) * scale;
    }
    const lh = FONT_MODEL.lh[dk][font] && FONT_MODEL.lh[dk][font][si];
    if (lh) height = lh * scale;
  } else {
    width = chars.length * size * 0.6;
  }
  const q = 1 / (64 * dpr);
  width = Math.round(width / q) * q;
  const H = Number(win.innerHeight) || 800;
  const frac = /about-you/.test(pageHref) ? 0.66 : 0.818; // 固定定位元素的静态位置≈页面内容底部
  const y = Math.round((H * frac) / 0.05) * 0.05;
  const fw = Math.fround(width), fh = Math.fround(height), fy = Math.fround(y);
  return { x: 0, y: fy, width: fw, height: fh, top: fy, right: fw, bottom: fy + fh, left: 0 };
}

function makeGenericElement(tag, opts, hrefFn) {
  const attrs = Object.create(null);
  const winRef = () => opts.__win;
  const el = {
    style: {},
    tagName: String(tag).toUpperCase(),
    nodeName: String(tag).toUpperCase(),
    nodeType: 1,
    children: [],
    childNodes: [],
    parentNode: null,
    innerText: "",
    textContent: "",
    appendChild(c) { if (c) { this.children.push(c); c.parentNode = this; } return c; },
    removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); return c; },
    remove() { if (this.parentNode && this.parentNode.removeChild) this.parentNode.removeChild(this); this.parentNode = null; },
    setAttribute(k, v) { attrs[k] = String(v); },
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null; },
    hasAttribute(k) { return Object.prototype.hasOwnProperty.call(attrs, k); },
    removeAttribute(k) { delete attrs[k]; },
    addEventListener() {},
    removeEventListener() {},
    getBoundingClientRect() {
      const w = winRef();
      if (w) return measureTextBox(w, this.style, this.innerText || this.textContent, hrefFn());
      return { x: 0, y: 0, width: 0, height: 0, top: 0, right: 0, bottom: 0, left: 0 };
    },
    getClientRects() { return [this.getBoundingClientRect()]; },
  };
  for (const name of ["getBoundingClientRect", "getClientRects", "appendChild", "removeChild", "remove", "setAttribute",
    "getAttribute", "hasAttribute", "removeAttribute", "addEventListener", "removeEventListener"]) {
    asNative(el[name], name);
  }
  Object.defineProperty(el, "offsetWidth", { get() { return Math.round(this.getBoundingClientRect().width); }, enumerable: false, configurable: true });
  Object.defineProperty(el, "offsetHeight", { get() { return Math.round(this.getBoundingClientRect().height); }, enumerable: false, configurable: true });
  Object.defineProperty(el, "clientWidth", { get() { return Math.round(this.getBoundingClientRect().width); }, enumerable: false, configurable: true });
  Object.defineProperty(el, "clientHeight", { get() { return Math.round(this.getBoundingClientRect().height); }, enumerable: false, configurable: true });
  return el;
}

function makeWindow(opts) {
  const langs = Array.isArray(opts.languages) && opts.languages.length ? opts.languages : ["en-US", "en"];
  const win = {};
  const loc = makeLocation(opts.href);
  // 真实 Chrome 的 performance.timeOrigin / now() 是 0.1ms 粒度的小数（例如 1790678347582.8 / 30792.5），不是整数毫秒。
  const timeOrigin = Math.round((nodePerf.timeOrigin + nodePerf.now() - Math.max(0, Number(opts.loadedAgoMs) || 0)) * 10) / 10;
  timeOrigins.set(win, timeOrigin);

  const doc = makeDocument({
    ...opts,
    location: loc,
    languages: langs,
    __win: win,
  });
  const nav = makeNavigator({ ...opts, languages: langs });
  const listeners = Object.create(null);

  function on(type, fn) {
    (listeners[type] || (listeners[type] = [])).push(fn);
  }
  function off(type, fn) {
    const list = listeners[type];
    if (!list) return;
    const i = list.indexOf(fn);
    if (i >= 0) list.splice(i, 1);
  }
  function dispatch(type, event) {
    for (const fn of (listeners[type] || []).slice()) {
      try { fn(event); } catch (e) {}
    }
  }

  const w = opts.screenWidth || 1920;
  const h = opts.screenHeight || 1080;
  const taskbar = Number(opts.taskbar) > 0 ? Number(opts.taskbar) : 40;
  const toolbar = Number(opts.toolbar) > 0 ? Number(opts.toolbar) : 88;
  const availH = Math.max(600, h - taskbar);
  const innerH = Math.max(500, availH - toolbar);
  const chromeObj = {
    app: {
      isInstalled: false,
      getDetails: asNative(function getDetails() { return null; }, "getDetails"),
      getIsInstalled: asNative(function getIsInstalled() { return false; }, "getIsInstalled"),
      runningState: asNative(function runningState() { return "cannot_run"; }, "runningState"),
    },
    csi: asNative(function csi() { return { startE: Date.now(), onloadT: Date.now(), pageT: 12, tran: 15 }; }, "csi"),
    loadTimes: asNative(function loadTimes() { return {}; }, "loadTimes"),
  };
  const perf = {
    now: asNative(function now() {
      return Math.round((nodePerf.timeOrigin + nodePerf.now()) * 10) / 10 - (timeOrigins.get(win) || timeOrigin);
    }, "now"),
    memory: { jsHeapSizeLimit: 4294705152, totalJSHeapSize: 40000000, usedJSHeapSize: 18000000 },
  };
  Object.defineProperty(perf, "timeOrigin", {
    get() { return timeOrigins.get(win) || timeOrigin; },
    enumerable: true,
    configurable: true,
  });
  const enumerableCore = {
    window: win,
    self: win,
    document: doc,
    name: "",
    location: loc,
    customElements: { define() {}, get() { return undefined; } },
    history: { get length() { return historyLens.get(win) || 3; }, state: null, pushState() {}, replaceState() {}, go() {}, back() {}, forward() {} },
    navigator: nav,
    clientInformation: nav,
    frames: win,
    length: 0,
    opener: null,
    parent: opts.parent || win,
    top: opts.top || win,
    origin: loc.origin,
    originAgentCluster: true,
    screen: { width: w, height: h, availWidth: w, availHeight: availH, colorDepth: 24, pixelDepth: 24, availLeft: 0, availTop: 0 },
    innerWidth: w,
    innerHeight: innerH,
    outerWidth: w,
    outerHeight: availH,
    devicePixelRatio: Number(opts.dpr) > 0 ? Number(opts.dpr) : 1,
    chrome: chromeObj,
    console: { log() {}, warn() {}, error() {}, info() {}, debug() {}, dir() {} },
    event: undefined,
  };
  for (const [k, v] of Object.entries(enumerableCore)) expose(win, k, v);
  Object.defineProperty(win, "frameElement", {
    get() { return typeof opts.getFrameElement === "function" ? opts.getFrameElement() : (opts.frameElement || null); },
    enumerable: true,
    configurable: true,
  });
  hidden(win, "globalThis", win);
  hidden(win, "crypto", webcrypto);
  hidden(win, "performance", perf);
  hidden(win, "fetch", hookedFetch);
  hidden(win, "Function", SafeFunction);
  hidden(win, "eval", asNative(function (code) {
    return runInSandbox(String(code), win, sdkURL);
  }, "eval"));
  hidden(win, "URL", URL);
  hidden(win, "URLSearchParams", URLSearchParams);
  hidden(win, "Promise", Promise);
  hidden(win, "Map", Map);
  hidden(win, "WeakMap", WeakMap);
  hidden(win, "Set", Set);
  hidden(win, "Array", Array);
  hidden(win, "Object", Object);
  hidden(win, "JSON", JSON);
  hidden(win, "Math", Math);
  hidden(win, "Date", Date);
  hidden(win, "Number", Number);
  hidden(win, "String", String);
  hidden(win, "Boolean", Boolean);
  hidden(win, "Error", Error);
  hidden(win, "TypeError", TypeError);
  hidden(win, "Uint8Array", Uint8Array);
  hidden(win, "Uint8ClampedArray", Uint8ClampedArray);
  hidden(win, "ArrayBuffer", ArrayBuffer);
  hidden(win, "TextEncoder", TextEncoder);
  hidden(win, "TextDecoder", TextDecoder);
  hidden(win, "atob", atob);
  hidden(win, "btoa", btoa);
  hidden(win, "setTimeout", setTimeout);
  hidden(win, "clearTimeout", clearTimeout);
  hidden(win, "setInterval", setInterval);
  hidden(win, "clearInterval", clearInterval);
  hidden(win, "Intl", Intl);
  hidden(win, "AbortController", HostAbortController);
  hidden(win, "AbortSignal", typeof AbortSignal === "function" ? AbortSignal : class {});
  hidden(win, "Event", HostEvent);
  hidden(win, "MessageEvent", HostMessageEvent);
  hidden(win, "DOMException", typeof DOMException === "function" ? DOMException : Error);
  hidden(win, "AudioContext", AudioContextStub);
  hidden(win, "webkitAudioContext", AudioContextStub);
  hidden(win, "OfflineAudioContext", AudioContextStub);
  hidden(win, "requestIdleCallback", function (cb) {
    return setTimeout(() => cb({ timeRemaining: () => 10, didTimeout: false }), 0);
  });
  hidden(win, "cancelIdleCallback", function (id) { clearTimeout(id); });
  hidden(win, "addEventListener", on);
  hidden(win, "removeEventListener", off);
  hidden(win, "dispatchEvent", function (ev) {
    if (ev && ev.type) dispatch(ev.type, ev);
    return true;
  });
  hidden(win, "postMessage", function (data) {
    const target = opts.messageTarget;
    if (!target) return;
    queueMicrotask(() => target.dispatchEvent({ type: "message", data, origin: loc.origin, source: win }));
  });
  installChromeSurface(win, opts);
  expose(win, "window", win);
  hidden(win, "globalThis", win);
  expose(win, "frames", win);
  for (const k of HIDDEN_WINDOW_BUILTINS) {
    if (!Object.prototype.hasOwnProperty.call(win, k)) continue;
    const desc = Object.getOwnPropertyDescriptor(win, k);
    if (desc && desc.enumerable && "value" in desc) hidden(win, k, desc.value);
  }
  asNative(win.addEventListener, "addEventListener");
  asNative(win.removeEventListener, "removeEventListener");
  asNative(win.dispatchEvent, "dispatchEvent");
  asNative(win.postMessage, "postMessage");
  asNative(win.requestIdleCallback, "requestIdleCallback");
  asNative(win.cancelIdleCallback, "cancelIdleCallback");
  asNative(win.atob, "atob");
  asNative(win.btoa, "btoa");
  asNative(win.setTimeout, "setTimeout");
  asNative(win.clearTimeout, "clearTimeout");
  asNative(win.setInterval, "setInterval");
  asNative(win.clearInterval, "clearInterval");
  return win;
}

function scriptList(srcs, fallback) {
  const out = [];
  const seen = new Set();
  for (const src of srcs || []) {
    const s = String(src || "").trim();
    if (!s || seen.has(s)) continue;
    seen.add(s);
    out.push(s);
  }
  if (fallback && !seen.has(fallback)) out.push(fallback);
  return out;
}

function syncPageState(win, href) {
  // Remix 是 SPA：进入资料页是 pushState，history.length 递增；Statsig 也会多写一条用户级缓存键。
  historyLens.set(win, /about-you/.test(String(href || "")) ? 4 : 3);
  const ls = win.localStorage;
  if (ls && /about-you/.test(String(href || "")) && !Object.keys(ls).some((k) => /^[0-9a-f]{16}$/.test(k))) {
    const old = {};
    for (const k of Object.keys(ls)) old[k] = ls.getItem(k);
    for (const k of Object.keys(old)) ls.removeItem(k);
    const hex = Array.from({ length: 16 }, () => "0123456789abcdef"[Math.floor(Math.random() * 16)]).join("");
    const ordered = Object.keys(old);
    for (let i = 0; i < ordered.length; i++) {
      ls.setItem(ordered[i], old[ordered[i]]);
      if (i === 0) ls.setItem(hex, JSON.stringify({ v: 1 }));
    }
  }
}

function applyPage(win, msg) {
  if (!win) return;
  if (msg.href) syncPageState(win, msg.href);
  if (msg.href) {
    const loc = makeLocation(msg.href);
    Object.assign(win.location, loc);
    win.origin = loc.origin;
  }
  const scripts = win.document && scriptBags.get(win.document);
  if (scripts && Array.isArray(msg.scripts)) {
    const next = scriptList(msg.scripts, sdkURL);
    scripts.length = 0;
    for (const src of next) scripts.push({ src });
  }
  const setBuild = win.document && dataBuildSetters.get(win.document);
  if (typeof setBuild === "function" && msg.dataBuild != null) {
    setBuild(msg.dataBuild);
  }
  const ago = Number(msg.loadedAgoMs);
  if (Number.isFinite(ago) && ago >= 0) {
    timeOrigins.set(win, Math.round((nodePerf.timeOrigin + nodePerf.now() - ago) * 10) / 10);
  }
  if (msg.cookies != null) applyCookies(win.document, msg.cookies);
  if (env.deviceID && win.document) win.document.cookie = "oai-did=" + env.deviceID;
}

function buildRuntime(init) {
  env = init;
  if (init.timezone) {
    currentTZ = String(init.timezone);
    process.env.TZ = currentTZ;
  }
  sdkSource = String(init.sdk || "");
  sdkURL = String(init.sdkURL || sdkURL);
  if (!sdkSource) throw new Error("sdk_empty");

  const shared = {
    ua: init.ua,
    languages: init.languages,
    deviceID: init.deviceID,
    sdkURL,
    screenWidth: init.screenWidth,
    screenHeight: init.screenHeight,
    taskbar: init.taskbar,
    toolbar: init.toolbar,
    chromeMajor: init.chromeMajor,
    chromeFull: init.chromeFull,
    timezone: init.timezone,
    locale: init.locale,
    dataBuild: init.dataBuild || "",
    scripts: scriptList(init.scripts, sdkURL),
    loadedAgoMs: init.loadedAgoMs,
  };
  const m = String(sdkURL).match(/\/sentinel\/([^/]+)\/sdk\.js/);
  // iframe 与 /req 跟随 SDK 所在源（真实认证页里是 sentinel.openai.com）。
  let sdkOrigin = "https://chatgpt.com";
  try { sdkOrigin = new URL(sdkURL).origin; } catch (e) {}
  const iframeHref = sdkOrigin + "/backend-api/sentinel/frame.html" + (m ? "?sv=" + encodeURIComponent(m[1]) : "");

  let iframeWin = null;
  let iframeEl = null;
  const parentWin = makeWindow({
    ...shared,
    href: init.href || "https://auth.openai.com/email-verification",
    cookies: init.cookies,
    frameElement: null,
    createIframe() {
      const elListeners = Object.create(null);
      const el = {
        style: { display: "none" },
        addEventListener(type, fn) {
          (elListeners[type] || (elListeners[type] = [])).push(fn);
        },
      };
      iframeEl = el;
      Object.defineProperty(el, "contentWindow", {
        get() { return iframeWin; },
        enumerable: true,
      });
      Object.defineProperty(el, "contentDocument", {
        get() { return iframeWin && iframeWin.document; },
        enumerable: true,
      });
      Object.defineProperty(el, "src", {
        set() {
          runInSandbox(sdkSource, iframeWin, sdkURL);
          expose(parentWin, "0", iframeWin);
          expose(parentWin, "length", 1);
          queueMicrotask(() => {
            for (const fn of elListeners.load || []) fn();
          });
        },
        get() {
          return iframeHref;
        },
      });
      return el;
    },
  });
  iframeWin = makeWindow({
    ...shared,
    href: iframeHref,
    scripts: [sdkURL],
    dataBuild: "",
    loadedAgoMs: 0,
    cookies: init.iframeCookies,
    top: parentWin,
    parent: parentWin,
    getFrameElement() { return iframeEl; },
  });
  parentWin.postMessage = function (data) {
    queueMicrotask(() => {
      parentWin.dispatchEvent({ type: "message", data, origin: iframeWin.location.origin, source: iframeWin });
    });
  };
  iframeWin.postMessage = function (data) {
    queueMicrotask(() => {
      iframeWin.dispatchEvent({ type: "message", data, origin: parentWin.location.origin, source: parentWin });
    });
  };

  applyPage(parentWin, init);
  runInSandbox(sdkSource, parentWin, sdkURL);
  parentCtx = parentWin;
}

// ---------------------------------------------------------------------------
// 拟人输入合成
//
// 官方 SDK 的 Session Observer(so) 收集器在 window 上以捕获模式监听
// keydown / pointermove / click / scroll / paste / input / wheel，并统计交互量、
// isTrusted、按键与指针几何。零交互的会话(so 快照全是 "0")会在数分钟后被异步风控
// 撤销 access token；真人会话不会。这里把真人在验证码页/资料页上的操作（移动鼠标 →
// 点输入框 → 逐键输入 → 移到按钮 → 点击）按真实节奏合成给收集器。
// 收集器只读 type / isTrusted / clientX / clientY / key / ctrlKey / metaKey / altKey，
// 事件对象都带 isTrusted:true。
// ---------------------------------------------------------------------------
const rndf = (a, b) => a + Math.random() * (b - a);
const rndi = (a, b) => Math.floor(rndf(a, b + 1));
const sleepMs = (ms) => new Promise((r) => setTimeout(r, ms));
const clampN = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const pointer = { x: 0, y: 0, ready: false };
const SHIFT_CHARS = new Set('~!@#$%^&*()_+{}|:"<>?ABCDEFGHIJKLMNOPQRSTUVWXYZ');
let idleCtl = null;

function emitEvent(win, type, props) {
  const ev = Object.assign({
    type, isTrusted: true, bubbles: true, cancelable: true, composed: true, defaultPrevented: false,
    eventPhase: 1, timeStamp: win.performance.now(),
    preventDefault() {}, stopPropagation() {}, stopImmediatePropagation() {},
  }, props || {});
  win.dispatchEvent(ev);
}

function ensurePointer(win) {
  if (!pointer.ready) {
    pointer.x = rndf(0.35, 0.65) * win.innerWidth;
    pointer.y = rndf(0.25, 0.55) * win.innerHeight;
    pointer.ready = true;
  }
}

function emitPointerMove(win, x, y, dx, dy) {
  emitEvent(win, "pointermove", {
    clientX: x, clientY: y, pageX: x, pageY: y, screenX: x, screenY: y + (Number(win.outerHeight) - Number(win.innerHeight)),
    movementX: dx, movementY: dy, pointerId: 1, pointerType: "mouse", isPrimary: true, buttons: 0, button: -1,
  });
}

async function moveMouseTo(win, tx, ty) {
  ensurePointer(win);
  const x0 = pointer.x, y0 = pointer.y;
  const dist = Math.hypot(tx - x0, ty - y0);
  if (dist < 2) return;
  const steps = clampN(Math.round(dist / 16), 10, 45);
  const c1x = x0 + (tx - x0) * 0.3 + rndf(-60, 60), c1y = y0 + (ty - y0) * 0.1 + rndf(-60, 60);
  const c2x = x0 + (tx - x0) * 0.75 + rndf(-40, 40), c2y = y0 + (ty - y0) * 0.9 + rndf(-40, 40);
  let px = x0, py = y0;
  for (let s = 1; s <= steps; s++) {
    let t = s / steps;
    t = t * t * (3 - 2 * t);
    const u = 1 - t;
    const bx = u * u * u * x0 + 3 * u * u * t * c1x + 3 * u * t * t * c2x + t * t * t * tx;
    const by = u * u * u * y0 + 3 * u * u * t * c1y + 3 * u * t * t * c2y + t * t * t * ty;
    emitPointerMove(win, bx, by, bx - px, by - py);
    px = bx; py = by;
    await sleepMs(rndf(7, 22));
  }
  pointer.x = tx;
  pointer.y = ty;
}

async function clickAt(win, x, y, target) {
  const tx = x + rndf(-3, 3), ty = y + rndf(-2, 2);
  await moveMouseTo(win, tx, ty);
  await sleepMs(rndf(80, 250));
  emitEvent(win, "pointerdown", { clientX: tx, clientY: ty, button: 0, buttons: 1, pointerId: 1, pointerType: "mouse", target });
  await sleepMs(rndf(50, 130));
  emitEvent(win, "pointerup", { clientX: tx, clientY: ty, button: 0, buttons: 0, pointerId: 1, pointerType: "mouse", target });
  emitEvent(win, "click", { clientX: tx, clientY: ty, pageX: tx, pageY: ty, button: 0, detail: 1, pointerType: "mouse", target });
}

function keyCodeFor(ch) {
  if (/[a-z]/i.test(ch)) return "Key" + ch.toUpperCase();
  if (/[0-9]/.test(ch)) return "Digit" + ch;
  if (ch === " ") return "Space";
  return "";
}

function fakeField(name, type) {
  return {
    nodeName: "INPUT", tagName: "INPUT", type, name, id: "_r_i_-" + name, value: "",
    disabled: false, readOnly: false, isContentEditable: false, autocomplete: type === "number" ? "off" : "on",
  };
}

async function typeInto(win, target, text, opts) {
  const o = opts || {};
  const chars = Array.from(String(text || ""));
  for (let i = 0; i < chars.length; i++) {
    const ch = chars[i];
    // 偶发笔误：多敲一个相邻字母再退格
    if (o.typos && /[a-z]/.test(ch) && i > 1 && Math.random() < 0.06) {
      const wrong = String.fromCharCode(clampN(ch.charCodeAt(0) + (Math.random() < 0.5 ? 1 : -1), 97, 122));
      emitEvent(win, "keydown", { key: wrong, code: keyCodeFor(wrong), shiftKey: false, ctrlKey: false, metaKey: false, altKey: false, repeat: false, target });
      target.value += wrong;
      await sleepMs(rndf(1, 4));
      emitEvent(win, "input", { inputType: "insertText", data: wrong, target });
      await sleepMs(rndf(140, 320));
      emitEvent(win, "keydown", { key: "Backspace", code: "Backspace", shiftKey: false, ctrlKey: false, metaKey: false, altKey: false, repeat: false, target });
      target.value = target.value.slice(0, -1);
      await sleepMs(rndf(1, 4));
      emitEvent(win, "input", { inputType: "deleteContentBackward", data: null, target });
      await sleepMs(rndf(90, 220));
    }
    const shift = SHIFT_CHARS.has(ch);
    if (shift) {
      emitEvent(win, "keydown", { key: "Shift", code: "ShiftLeft", shiftKey: true, ctrlKey: false, metaKey: false, altKey: false, repeat: false, target });
      await sleepMs(rndf(25, 70));
    }
    emitEvent(win, "keydown", { key: ch, code: keyCodeFor(ch), shiftKey: shift, ctrlKey: false, metaKey: false, altKey: false, repeat: false, target });
    target.value += ch;
    await sleepMs(rndf(1, 4));
    emitEvent(win, "input", { inputType: "insertText", data: ch, target });
    await sleepMs(rndf(70, 190) + (Math.random() < 0.08 ? rndf(150, 400) : 0));
  }
}

async function runInteraction(win, it) {
  const W = Number(win.innerWidth) || 1366;
  const cx = W / 2;
  const button = { nodeName: "BUTTON", tagName: "BUTTON", type: "submit", disabled: false, isContentEditable: false, innerText: "Continue" };
  if (it.kind === "otp") {
    const field = fakeField("code", "text");
    await clickAt(win, cx + rndf(-60, 60), 300 + rndf(-6, 6), field);
    await sleepMs(rndf(250, 650));
    await typeInto(win, field, it.text || "", {});
    await sleepMs(rndf(300, 800));
    await clickAt(win, cx + rndf(-90, 90), 380 + rndf(-8, 8), button);
  } else if (it.kind === "profile") {
    const nameEl = fakeField("name", "text");
    const ageEl = fakeField("age", "number");
    await clickAt(win, cx + rndf(-60, 60), 280 + rndf(-6, 6), nameEl);
    await sleepMs(rndf(300, 750));
    await typeInto(win, nameEl, it.name || "", { typos: true });
    await sleepMs(rndf(250, 600));
    if (Math.random() < 0.45) {
      emitEvent(win, "keydown", { key: "Tab", code: "Tab", shiftKey: false, ctrlKey: false, metaKey: false, altKey: false, repeat: false, target: nameEl });
      await sleepMs(rndf(150, 350));
    } else {
      await clickAt(win, cx + rndf(-60, 60), 350 + rndf(-6, 6), ageEl);
      await sleepMs(rndf(250, 600));
    }
    await typeInto(win, ageEl, it.age || "", {});
    await sleepMs(rndf(350, 900));
    await clickAt(win, cx + rndf(-90, 90), 482 + rndf(-8, 8), button);
  }
}

// 等待验证码/停留期间：真人偶尔挪一下鼠标。
function startIdle(win) {
  stopIdle();
  const ctl = { running: true, done: null };
  ctl.done = (async () => {
    ensurePointer(win);
    await sleepMs(rndf(600, 1800));
    while (ctl.running) {
      const W = Number(win.innerWidth) || 1366, H = Number(win.innerHeight) || 800;
      const tx = clampN(pointer.x + rndf(-260, 260), 40, W - 40);
      const ty = clampN(pointer.y + rndf(-160, 160), 40, H - 40);
      await moveMouseTo(win, tx, ty);
      const wait = rndf(2500, 7500);
      const t0 = Date.now();
      while (ctl.running && Date.now() - t0 < wait) await sleepMs(120);
    }
  })();
  idleCtl = ctl;
}

async function stopIdle() {
  const ctl = idleCtl;
  idleCtl = null;
  if (!ctl) return;
  ctl.running = false;
  try { await ctl.done; } catch (_) {}
}

// 页面加载：与真实浏览器一致，SDK 在页面就绪时 init(flow)，此刻起 Session Observer 收集器开始观察。
async function pageStart(msg) {
  if (!parentCtx) throw new Error("sdk_not_ready");
  applyPage(parentCtx, msg);
  const sdk = parentCtx.SentinelSDK;
  if (!sdk || typeof sdk.init !== "function") throw new Error("sdk_unavailable");
  await stopIdle();
  const prev = activeCtx;
  activeCtx = parentCtx;
  try {
    await sdk.init(msg.flow);
  } finally {
    activeCtx = prev;
  }
  startIdle(parentCtx);
}

async function mint(msg) {
  if (!parentCtx) throw new Error("sdk_not_ready");
  applyPage(parentCtx, msg);
  const sdk = parentCtx.SentinelSDK;
  if (!sdk || typeof sdk.token !== "function") throw new Error("sdk_unavailable");
  await stopIdle();
  const prev = activeCtx;
  activeCtx = parentCtx;
  try {
    if (msg.interact) {
      await runInteraction(parentCtx, msg.interact);
    }
    const token = String((await sdk.token(msg.flow)) || "");
    if (!token) throw new Error("empty_token");
    let so = "";
    try {
      if (typeof sdk.sessionObserverToken === "function") {
        so = String((await sdk.sessionObserverToken(msg.flow)) || "");
      }
    } catch (_) {}
    return { token, so };
  } finally {
    activeCtx = prev;
  }
}

function runSelftest() {
  sdkURL = "https://chatgpt.com/sentinel/x/sdk.js";
  currentTZ = "America/New_York";
  process.env.TZ = currentTZ;
  env = { timezone: currentTZ };
  const win = makeWindow({
    href: "https://auth.openai.com/email-verification",
    ua: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.7680.164 Safari/537.36",
    languages: ["en-US", "en"],
    sdkURL,
    chromeMajor: "146",
    chromeFull: "146.0.7680.164",
    timezone: currentTZ,
    taskbar: 40,
    toolbar: 88,
  });
  vm.createContext(win);
  const fetchStr = vm.runInContext("Function.prototype.toString.call(fetch)", win);
  if (!String(fetchStr).includes("[native code]")) {
    throw new Error("fetch toString=" + fetchStr);
  }
  let kind;
  try {
    kind = vm.runInContext("(function(){}).constructor('return typeof process')()", win);
  } catch (e) {
    kind = "threw";
  }
  if (kind === "object" || kind === "function") {
    throw new Error("process leaked typeof=" + kind);
  }
  const leaked = Object.getOwnPropertyNames(win).filter((n) => n.startsWith("__"));
  if (leaked.length) {
    throw new Error("window internals " + leaked.join(","));
  }
  const keys = vm.runInContext("Object.keys(window)", win);
  const bad = ["Array", "Promise", "Uint8Array", "Date", "JSON", "Math", "fetch", "Function"].filter((k) => keys.includes(k));
  if (bad.length) {
    throw new Error("enumerable builtins " + bad.join(","));
  }
  if (!keys.includes("document") || !keys.includes("chrome") || !keys.includes("clientInformation")) {
    throw new Error("missing chrome keys " + keys.slice(0, 20).join(","));
  }
  const ds = vm.runInContext("String(new Date())", win);
  if (/[\u4e00-\u9fff]/.test(ds) || !/GMT[+-]\d{4}/.test(ds)) {
    throw new Error("date toString=" + ds);
  }
  if (!/\((Eastern|GMT)/.test(ds) && !/Daylight|Standard/.test(ds)) {
    throw new Error("date tz=" + ds);
  }
  const plug = vm.runInContext("navigator.plugins.length", win);
  if (plug !== 5) {
    throw new Error("plugins=" + plug);
  }
  const hasRuntime = vm.runInContext("'runtime' in chrome", win);
  if (hasRuntime) {
    throw new Error("chrome.runtime should be absent");
  }
  process.stderr.write("selftest_ok\n");
}

// 作为模块被 require（实验台用）时只导出窗口构造器，不启动 stdin 协议。
function makeLabWindow(o) {
  env = o.env || { timezone: o.timezone };
  if (o.sdkURL) sdkURL = o.sdkURL;
  if (o.timezone) {
    currentTZ = String(o.timezone);
    process.env.TZ = currentTZ;
  }
  return makeWindow(o);
}
if (require.main !== module) {
  module.exports = { makeLabWindow, runInSandbox, asNative };
} else {

if (process.argv.includes("--selftest")) {
  runSelftest();
  process.exit(0);
}

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on("line", (line) => {
  line = String(line || "").trim();
  if (!line) return;
  let msg;
  try {
    msg = JSON.parse(line);
  } catch (e) {
    send({ type: "err", error: "bad_json" });
    return;
  }
  if (msg.type === "fetch_ok" || msg.type === "fetch_err") {
    finishFetch(msg);
    return;
  }
  mintBusy = mintBusy.then(async () => {
    try {
      if (msg.type === "init") {
        buildRuntime(msg);
        send({ type: "ready" });
        return;
      }
      if (msg.type === "page") {
        await pageStart(msg);
        send({ type: "page_ok" });
        return;
      }
      if (msg.type === "mint") {
        const out = await mint(msg);
        send({ type: "ok", token: out.token, so: out.so });
        return;
      }
      send({ type: "err", error: "unknown_type" });
    } catch (e) {
      send({ type: "err", error: String(e && e.message ? e.message : e) });
    }
  });
});

log("sentinel_node listening");
send({ type: "hello" });
}
