// Sentinel VM 实验台：移植官方 SDK 的 dx 解释器，用真实抓到的程序 + 我们自己控制的假窗口/事件，
// 观察 session-observer 收集器记录了什么、so 快照输出长什么样。
const fs = require("fs");

const atobN = (s) => Buffer.from(s, "base64").toString("latin1");
const btoaN = (s) => Buffer.from(String(s), "latin1").toString("base64");
const xorLog = [];
function rt(t, n) {
  let r = "";
  for (let o = 0; o < t.length; o++) r += String.fromCharCode(t.charCodeAt(o) ^ n.charCodeAt(o % n.length));
  if (t.length > 80) xorLog.push({ inLen: t.length, keyLen: n.length, key: n.slice(0, 40), inHead: t.slice(0, 200), outHead: r.slice(0, 200) });
  return r;
}

function createVM(win, doc, hooks = {}) {
  const St = new Map();
  let Ct = 0;
  let At = Promise.resolve();
  const J = 0, G = 1, W = 2, z = 3, H = 4, V = 5, B = 6, Z = 24, K = 7, Q = 8, Y = 9, X = 10, tt = 11, nt = 12, et = 13,
    rtn = 14, ot = 15, it = 16, ct = 17, st = 18, ut = 19, at = 23, ft = 20, lt = 21, dt = 22, ht = 25, pt = 26, mt = 27,
    gt = 28, wt = 29, yt = 30, vt = 33, bt = 34, kt = 35;
  const Ot = (fn) => { const e = At.then(fn, fn); At = e.then(() => {}, () => {}); return e; };
  async function _t() {
    while (St.get(Y).length > 0) {
      const [n, ...e] = St.get(Y).shift();
      const f = St.get(n);
      if (typeof f !== "function") throw new Error("op register " + n + " is not a function (" + typeof f + ")");
      const r = f(...e);
      if (r && typeof r.then === "function") await r;
      Ct++;
    }
  }
  const Nt = (t) => Ot(() => jt(t));
  function jt(t, n) {
    return new Promise((resolve, reject) => {
      if (n !== undefined) {
        St.clear();
        St.set(J, Nt);
        St.set(G, (n2, e) => St.set(n2, rt("" + St.get(n2), "" + St.get(e))));
        St.set(W, (n2, e) => St.set(n2, e));
        St.set(V, (n2, e) => { const o = St.get(n2); Array.isArray(o) ? o.push(St.get(e)) : St.set(n2, o + St.get(e)); });
        St.set(mt, (n2, e) => { const o = St.get(n2); Array.isArray(o) ? o.splice(o.indexOf(St.get(e)), 1) : St.set(n2, o - St.get(e)); });
        St.set(wt, (n2, e, r) => St.set(n2, St.get(e) < St.get(r)));
        St.set(vt, (n2, e, r) => { St.set(n2, Number(St.get(e)) * Number(St.get(r))); });
        St.set(kt, (n2, e, r) => { const i = Number(St.get(e)), c = Number(St.get(r)); St.set(n2, c === 0 ? 0 : i / c); });
        St.set(B, (n2, e, r) => { const o = St.get(e), k = St.get(r); if (hooks.onGet) hooks.onGet(o, k); St.set(n2, o[k]); });
        St.set(K, (n2, ...e) => {
          const f = St.get(n2);
          if (typeof f !== "function" && hooks.onBad) hooks.onBad("call-not-function", { reg: n2, type: typeof f, val: String(f).slice(0, 60), idx: Ct });
          return f(...e.map((x) => St.get(x)));
        });
        St.set(ct, (n2, e, ...r) => {
          try {
            const t2 = St.get(e)(...r.map((x) => St.get(x)));
            if (t2 && typeof t2.then === "function") return t2.then((v) => { St.set(n2, v); }).catch((v) => { St.set(n2, "" + v); });
            St.set(n2, t2);
          } catch (t2) { St.set(n2, "" + t2); }
        });
        St.set(et, (n2, e, ...r) => { try { St.get(e)(...r); } catch (t2) { St.set(n2, "" + t2); } });
        St.set(Q, (n2, e) => St.set(n2, St.get(e)));
        St.set(X, win);
        St.set(tt, (n2, e) => St.set(n2, (Array.from(doc.scripts || []).map((x) => x?.src?.match(St.get(e))).filter((x) => x?.length)[0] ?? [])[0] ?? null));
        St.set(nt, (n2) => St.set(n2, St));
        St.set(rtn, (n2, e) => St.set(n2, JSON.parse("" + St.get(e))));
        St.set(ot, (n2, e) => St.set(n2, JSON.stringify(St.get(e))));
        St.set(st, (n2) => St.set(n2, atobN("" + St.get(n2))));
        St.set(ut, (n2) => St.set(n2, btoaN("" + St.get(n2))));
        St.set(ft, (n2, e, r, ...o) => (St.get(n2) === St.get(e) ? St.get(r)(...o) : null));
        St.set(lt, (n2, e, r, o, ...i) => (Math.abs(St.get(n2) - St.get(e)) > St.get(r) ? St.get(o)(...i) : null));
        St.set(at, (n2, e, ...r) => (void 0 !== St.get(n2) ? St.get(e)(...r) : null));
        St.set(Z, (n2, e, r) => {
          const o = St.get(e), k = St.get(r);
          if ((o === undefined || o === null || o[k] === undefined) && hooks.onBad) {
            hooks.onBad("bind-missing", { key: String(k), target: o === undefined ? "undefined" : o === null ? "null" : Object.prototype.toString.call(o), idx: Ct });
          }
          St.set(n2, o[k].bind(o));
        });
        St.set(bt, (n2, e) => { try { const t2 = St.get(e); return Promise.resolve(t2).then((t3) => { St.set(n2, t3); }); } catch { return; } });
        St.set(dt, (n2, e) => {
          const o = [...St.get(Y)];
          St.set(Y, [...e]);
          return _t().catch((t2) => { St.set(n2, "" + t2); }).finally(() => { St.set(Y, o); });
        });
        St.set(gt, () => {}); St.set(pt, () => {}); St.set(ht, () => {});
        Ct = 0;
        St.set(it, n);
      }
      let done = false;
      const timer = setTimeout(() => { if (!done) { done = true; reject(new Error("vm_timeout")); } }, 20000);
      const s = (v) => { if (!done) { done = true; clearTimeout(timer); resolve(v); } };
      St.set(z, (v) => { s(btoaN("" + v)); });
      St.set(H, (v) => { if (!done) { done = true; clearTimeout(timer); reject(new Error("vm_reject:" + btoaN("" + v))); } });
      St.set(yt, (t2, n2, e, r) => {
        const c = Array.isArray(r);
        const sArr = c ? e : [];
        const u = (c ? r : e) || [];
        St.set(t2, (...args) => {
          const saved = [...St.get(Y)];
          if (c) for (let i = 0; i < sArr.length; i++) St.set(sArr[i], args[i]);
          St.set(Y, [...u]);
          return _t().then(() => St.get(n2)).catch((x) => "" + x).finally(() => { St.set(Y, saved); });
        });
      });
      try {
        St.set(Y, JSON.parse(rt(atobN(t), "" + St.get(it))));
        _t().catch((t2) => { s(btoaN(Ct + ": " + t2)); });
      } catch (t2) { s(btoaN(Ct + ": " + t2)); }
    });
  }
  return { run: (prog, key) => Ot(() => jt(prog, key)), St };
}

module.exports = { createVM, atobN, btoaN, rt, xorLog };
