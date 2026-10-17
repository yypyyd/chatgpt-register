const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'static/settings.html'), 'utf8');
const script = fs.readFileSync(path.join(root, 'static/proxy-settings.js'), 'utf8');
const channelNames = ['chatgpt', 'grok', 'adobe', 'leonardo'];

class Element {
  constructor() {
    this.value = ''; this.checked = false; this.children = []; this.style = {}; this.attributes = {};
    this.listeners = {}; this.textContent = ''; this.className = '';
    this.classList = { add: value => { this.className += ' ' + value; } };
  }
  setAttribute(key, value) { this.attributes[key] = value; }
  replaceChildren(...nodes) { this.children = nodes; }
  append(...nodes) { this.children.push(...nodes); }
  addEventListener(name, listener) { this.listeners[name] = listener; }
  dispatchEvent(event) {
    if (this.onchange && event.type === 'change') this.onchange(event);
    this.listeners[event.type]?.(event);
  }
}

function setup() {
  const elements = Object.fromEntries([...html.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()]));
  const channels = channelNames.map(channel => ({
    channel, label: channel === 'chatgpt' ? 'GPT' : channel,
    min_session_minutes: channel === 'chatgpt' ? 9 : 6,
    config: { enabled: false, source: 'api', api_url: 'https://extract.example/' + channel + '?sessTime=30', list: '' },
    status: { available: 0, last_fetch: null, last_error: '' },
  }));
  const requests = [], notices = [];
  const context = vm.createContext({
    URL, Date, Event: class { constructor(type) { this.type = type; } },
    document: { getElementById: id => elements[id], createElement: () => new Element() },
    api: async (url, options) => {
      requests.push({ url, ...options });
      if (options?.method === 'PUT') return { ok: true, json: async () => ({ ok: true }) };
      return { ok: true, json: async () => ({ channels: structuredClone(channels) }) };
    },
    toast: (...args) => notices.push(args),
  });
  vm.runInContext(script, context, { filename: 'proxy-settings.js' });
  elements['set-proxy-source'].onchange = () => context.proxySourceChanged();
  return { context, elements, requests, notices, row: channel => vm.runInContext(`proxySettings.rows.find(row => row.channel === '${channel}')`, context) };
}

test('tabs preserve separate saved values and unsaved drafts', async () => {
  const { context, elements, row } = setup();
  await context.loadProxySettings();
  assert.equal(elements['proxy-channels'].children.length, channelNames.length);
  elements['set-proxy-api'].value = 'https://draft.example/gpt?sessTime=10';
  context.updateProxyHint();
  elements['proxy-channels'].children[1].onclick();
  assert.match(elements['set-proxy-api'].value, /extract.example\/grok/);
  assert.equal(row('grok').dirty, false);
  elements['proxy-channels'].children[0].onclick();
  assert.match(elements['set-proxy-api'].value, /draft.example\/gpt/);
  assert.equal(row('chatgpt').dirty, true);
});

test('Leonardo has its own tab, switch and unsaved API draft', async () => {
  const { context, elements, row } = setup();
  await context.loadProxySettings();
  elements['proxy-channels'].children[channelNames.indexOf('leonardo')].onclick();
  elements['set-proxy-enabled'].checked = true;
  elements['set-proxy-api'].value = 'https://draft.example/leonardo?sessTime=10';
  context.proxyDraftChanged();
  elements['proxy-channels'].children[0].onclick();
  assert.equal(elements['set-proxy-enabled'].checked, false);
  assert.match(elements['set-proxy-api'].value, /extract.example\/chatgpt/);
  assert.equal(row('leonardo').config.enabled, true);
  assert.equal(row('leonardo').dirty, true);
  assert.equal(row('chatgpt').dirty, false);
});

test('saving Grok does not save or overwrite the GPT draft', async () => {
  const { context, elements, requests, row } = setup();
  await context.loadProxySettings();
  elements['set-proxy-api'].value = 'https://draft.example/gpt';
  context.updateProxyHint();
  elements['proxy-channels'].children[1].onclick();
  elements['set-proxy-enabled'].checked = true;
  context.proxyDraftChanged();
  await context.saveProxy();
  const save = requests.find(request => request.method === 'PUT');
  assert.equal(save.url, '/api/proxy/channels/grok');
  assert.equal(JSON.parse(save.body).enabled, true);
  assert.match(JSON.parse(save.body).api_url, /extract.example\/grok/);
  assert.equal(row('chatgpt').dirty, true);
  assert.equal(row('grok').dirty, false);
});

test('proxy example uses the selected channel and sufficient session time', async () => {
  const { context, elements, row } = setup();
  await context.loadProxySettings();
  elements['proxy-channels'].children[3].onclick();
  context.useProxyExample();
  const url = new URL(elements['set-proxy-api'].value);
  assert.equal(url.searchParams.get('region'), 'US');
  assert.equal(url.searchParams.get('count'), '1');
  assert.ok(Number(url.searchParams.get('sessTime')) >= 6);
  assert.match(row('chatgpt').config.api_url, /extract.example\/chatgpt/);
});

test('a test completing after changing tabs stays attached to its original channel', async () => {
  const { context, elements, row } = setup();
  await context.loadProxySettings();
  let finish;
  context.api = () => new Promise(resolve => { finish = resolve; });
  const pending = context.testProxy();
  elements['proxy-channels'].children[3].onclick();
  finish({ ok: true, json: async () => ({ ok: true, count: 1, ip: '203.0.113.24', ms: 20 }) });
  await pending;
  assert.match(row('chatgpt').testResult.text, /203.0.113.24/);
  assert.equal(row('leonardo').testResult, undefined);
  assert.equal(elements['proxy-test-result'].style.display, 'none');
});

test('a rejected save leaves the draft dirty and reports the server error', async () => {
  const { context, elements, row, notices } = setup();
  await context.loadProxySettings();
  elements['set-proxy-api'].value = 'bad-url';
  context.updateProxyHint();
  context.api = async () => ({ ok: false, json: async () => ({ error: '提取 API 地址无效' }) });
  await context.saveProxy();
  assert.equal(row('chatgpt').dirty, true);
  assert.match(row('chatgpt').saved, /extract.example/);
  assert.deepEqual(notices.at(-1), ['提取 API 地址无效', true]);
  assert.equal(elements['proxy-save-btn'].disabled, false);
});

test('list source and short rotation warning remain scoped to the selected tab', async () => {
  const { context, elements } = setup();
  await context.loadProxySettings();
  elements['set-proxy-source'].value = 'list';
  context.proxySourceChanged();
  assert.equal(elements['proxy-api-fields'].style.display, 'none');
  assert.equal(elements['proxy-list-fields'].style.display, '');
  assert.equal(elements['proxy-test-btn'].textContent, '测试第一个代理');
  elements['proxy-channels'].children[3].onclick();
  elements['set-proxy-api'].value = 'https://extract.example/gen?sessTime=5';
  context.proxyDraftChanged();
  assert.match(elements['proxy-window-hint'].className, /proxy-hint-warning/);
  elements['proxy-channels'].children[0].onclick();
  assert.equal(elements['set-proxy-source'].value, 'list');
  assert.equal(elements['proxy-test-btn'].textContent, '测试第一个代理');
});
