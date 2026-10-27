"use strict";

const vm = require("node:vm");
const readline = require("node:readline");
const { makeLabWindow, runInSandbox } = require("./sentinel_node.js");

let win;
let chain = Promise.resolve();
function reply(value) {
  process.stdout.write(JSON.stringify(value) + "\n");
}
async function handle(input) {
  if (input.type === "init") {
    win = makeLabWindow({
      href: input.url, ua: input.userAgent, cookies: input.cookies,
      languages: ["en-US", "en"], timezone: "America/New_York",
      chromeMajor: "131", chromeFull: "131.0.0.0",
    });
    const create = win.document.createElement.bind(win.document);
    win.document.createElement = (tag, ...args) => {
      const element = create(tag, ...args);
      if (String(tag).toLowerCase() === "canvas") element.getContext = () => null;
      if (String(tag).toLowerCase() === "iframe") {
        element.contentWindow = vm.runInNewContext("globalThis");
        element.contentWindow.document = win.document;
        element.contentDocument = win.document;
      }
      return element;
    };
    win.document.documentElement.hasAttribute = () => false;
    win.document.documentElement.attributes = {};
    // This worker only computes SDK tokens. Network and account credentials
    // remain in the Go registration client.
    win.fetch = () => Promise.reject(new Error("network unavailable in SDK worker"));
    const exports = {};
    win.TURBOPACK = { push(chunk) {
      if (typeof chunk[2] !== "function") throw new Error("unsupported SDK bundle");
      chunk[2]({ i: () => ({}), e: exports, g: win }, { exports }, exports);
    } };
    runInSandbox(input.source, win, input.sdkURL);
    if (typeof exports.configure !== "function") throw new Error("missing Castle SDK");
    win.__castle = exports;
    win.__pk = input.pk;
    win.__sdk = await runInSandbox("__castle.configure({pk:__pk})", win, input.sdkURL);
    reply({ type: "ready" });
    return;
  }
  if (input.type !== "mint" || !win?.__sdk) throw new Error("SDK not initialized");
  const token = await runInSandbox("__sdk.createRequestToken()", win, "castle-request.js");
  if (typeof token !== "string" || token.length < 100) throw new Error("empty Castle token");
  reply({ type: "token", token, cookies: win.document.cookie });
}
const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
lines.on("line", line => {
  chain = chain.then(() => handle(JSON.parse(line))).catch(() => reply({ type: "error", error: "Castle SDK computation failed" }));
});
lines.on("close", () => chain.finally(() => process.stdout.write("", () => process.exit(0))));
