// Each tab keeps its own draft; saving/testing a tab never submits another one.
const proxySettings = { rows: [], channel: '', loading: true };
const proxyField = id => document.getElementById(id);
const proxyRow = () => proxySettings.rows.find(row => row.channel === proxySettings.channel);

function readProxyDraft() {
  return {
    enabled: proxyField('set-proxy-enabled').checked,
    source: proxyField('set-proxy-source').value,
    api_url: proxyField('set-proxy-api').value.trim(),
    list: proxyField('set-proxy-list').value.trim(),
  };
}

function stashProxyDraft() {
  const row = proxyRow();
  if (!row) return;
  row.config = readProxyDraft();
  row.dirty = JSON.stringify(row.config) !== row.saved;
}

async function loadProxySettings() {
  try {
    const response = await api('/api/proxy/channels');
    const data = await response.json();
    if (!response.ok || !Array.isArray(data.channels)) throw new Error(data.error || '加载渠道代理配置失败');
    proxySettings.rows = data.channels.map(row => ({ ...row, saved: JSON.stringify(row.config), dirty: false }));
    proxySettings.loading = false;
    proxySettings.channel = proxySettings.rows[0]?.channel || '';
    renderProxyTabs();
    renderProxyEditor();
  } catch (error) {
    proxyField('proxy-channels').textContent = '渠道代理配置加载失败，请刷新重试';
    toast(error.message || '加载失败', true);
  }
}

function renderProxyTabs() {
  const container = proxyField('proxy-channels');
  container.replaceChildren();
  for (const row of proxySettings.rows) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'proxy-channel-tab' + (row.channel === proxySettings.channel ? ' active' : '');
    button.setAttribute('role', 'tab');
    button.setAttribute('aria-selected', String(row.channel === proxySettings.channel));
    button.textContent = row.label + (row.dirty ? ' *' : '');
    const badge = document.createElement('span');
    badge.className = 'proxy-channel-badge' + (row.config.enabled ? ' enabled' : '');
    badge.textContent = row.config.enabled ? '已开启' : '直连';
    button.append(badge);
    button.onclick = () => {
      stashProxyDraft();
      proxySettings.channel = row.channel;
      renderProxyTabs();
      renderProxyEditor();
    };
    container.append(button);
  }
}

function renderProxyEditor() {
  const row = proxyRow();
  if (!row) return;
  proxySettings.rendering = true;
  proxyField('proxy-editor').style.display = '';
  proxyField('set-proxy-enabled').checked = row.config.enabled;
  proxyField('set-proxy-source').value = row.config.source;
  proxyField('set-proxy-api').value = row.config.api_url || '';
  proxyField('set-proxy-list').value = row.config.list || '';
  // Keep layout.js's enhanced select label aligned with the native value.
  if (typeof syncSelect === 'function') syncSelect('set-proxy-source');
  proxySettings.rendering = false;
  proxyField('proxy-switch-label').textContent = '启用 ' + row.label + ' 代理';
  renderProxySource();
  renderProxyResult();
  renderProxyButtons();
}

function renderProxySource() {
  const row = proxyRow();
  if (!row) return;
  const isAPI = proxyField('set-proxy-source').value === 'api';
  proxyField('proxy-api-fields').style.display = isAPI ? '' : 'none';
  proxyField('proxy-list-fields').style.display = isAPI ? 'none' : '';
  proxyField('proxy-mode-summary').textContent = (row.dirty ? '尚未保存 · ' : '') + (proxyField('set-proxy-enabled').checked ? '新任务使用 ' + row.label + ' 的' + (isAPI ? ' API 代理。提取失败将报错。' : '独立代理列表。') : row.label + ' 的新任务将直连。');
  const status = row.status || {};
  const fetched = status.last_fetch ? new Date(status.last_fetch).toLocaleString() : '尚未提取';
  proxyField('proxy-pool-status').textContent = isAPI ? '可分配 ' + (status.available || 0) + ' 条 · 最近提取：' + fetched + (status.last_error ? ' · ' + status.last_error : '') : '此渠道使用独立列表，可测试第一条代理的出口 IP。';
  updateProxyHint(false);
  updateProxyCount(false);
}

function proxyDraftChanged() {
  stashProxyDraft();
  renderProxyTabs();
  renderProxySource();
  renderProxyButtons();
}

function proxySourceChanged() {
  // Ignore programmatic changes while rendering a different tab.
  if (proxySettings.rendering) return;
  proxyDraftChanged();
}

function updateProxyHint(stash = true) {
  if (stash) stashProxyDraft();
  const row = proxyRow();
  if (!row) return;
  const hint = proxyField('proxy-window-hint');
  hint.className = 'field-hint';
  try {
    const url = new URL(proxyField('set-proxy-api').value.trim());
    const raw = url.searchParams.get('sessTime');
    const minutes = Number(raw);
    const count = url.searchParams.get('count');
    hint.textContent = (count ? '每次提取 ' + count + ' 条。' : '') + (raw ? '出口每 ' + raw + ' 分钟轮换。' : '未指定 sessTime；请确认供应商的轮换周期。') + '此渠道建议轮换周期至少 ' + row.min_session_minutes + ' 分钟。';
    if (raw && (!Number.isInteger(minutes) || minutes < row.min_session_minutes)) hint.classList.add('proxy-hint-warning');
  } catch {
    hint.textContent = '此渠道建议轮换周期至少 ' + row.min_session_minutes + ' 分钟；低并发建议小批量提取。';
  }
  if (stash) renderProxyTabs();
}

function updateProxyCount(stash = true) {
  if (stash) stashProxyDraft();
  const count = proxyField('set-proxy-list').value.split(/[\r\n,]+/).map(line => line.trim()).filter(Boolean).length;
  proxyField('proxy-count').textContent = '识别到 ' + count + ' 个代理';
  if (stash) renderProxyTabs();
}

function useProxyExample() {
  const row = proxyRow();
  const minutes = Math.max(10, Math.ceil(row.min_session_minutes / 5) * 5);
  const region = 'US';
  proxyField('set-proxy-api').value = 'http://global.rrp.bestgo.work:8089/gen?zone=custom&ptype=1&region=' + region + '&count=1&proto=http&stype=txt&sessType=sticky&split=%5Cr%5Cn&sessTime=' + minutes + '&sessAuto=1';
  proxyDraftChanged();
}

function renderProxyButtons() {
  const row = proxyRow();
  if (!row) return;
  proxyField('proxy-test-btn').disabled = !!row.testing;
  proxyField('proxy-test-btn').textContent = row.testing ? '测试中…' : row.config.source === 'api' ? '提取并测试' : '测试第一个代理';
  proxyField('proxy-save-btn').disabled = !!row.saving;
  proxyField('proxy-save-btn').textContent = row.saving ? '保存中…' : '保存 ' + row.label + ' 配置';
}

function renderProxyResult() {
  const result = proxyRow()?.testResult;
  const box = proxyField('proxy-test-result');
  box.style.display = result ? 'flex' : 'none';
  box.className = 'proxy-result ' + (result?.kind || '');
  box.textContent = result?.text || '';
}

async function saveProxy() {
  stashProxyDraft();
  const row = proxyRow();
  if (!row || row.saving) return;
  const config = { ...row.config };
  row.saving = true;
  renderProxyButtons();
  try {
    const response = await api('/api/proxy/channels/' + row.channel, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(config),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(data.error || '保存失败');
    row.saved = JSON.stringify(config);
    if (proxySettings.channel === row.channel) stashProxyDraft();
    row.dirty = JSON.stringify(row.config) !== row.saved;
    row.status = { available: 0, last_fetch: row.status?.last_fetch, last_error: '' };
    toast(row.label + ' 代理配置已保存');
    renderProxyTabs();
    if (proxySettings.channel === row.channel) renderProxySource();
  } catch (error) {
    toast(error.message || '保存失败', true);
  } finally {
    row.saving = false;
    renderProxyButtons();
  }
}

async function testProxy() {
  stashProxyDraft();
  const row = proxyRow();
  if (!row || row.testing) return;
  const config = { ...row.config };
  row.testing = true;
  row.testResult = { kind: 'testing', text: '正在测试 ' + row.label + ' 的代理配置…' };
  renderProxyButtons();
  renderProxyResult();
  try {
    const response = await api('/api/proxy/channels/' + row.channel + '/test', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(config),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || '测试失败');
    row.testResult = { kind: 'ok', text: '✓ ' + row.label + ' · 识别 ' + data.count + ' 条 · 测试第一条出口 IP ' + data.ip + ' · ' + data.ms + ' ms' };
  } catch (error) {
    row.testResult = { kind: 'fail', text: '✗ ' + row.label + ' · ' + (error.message || '测试失败') };
  } finally {
    row.testing = false;
    if (proxySettings.channel === row.channel) renderProxyResult();
    renderProxyButtons();
  }
}

proxyField('set-proxy-enabled').addEventListener('change', proxyDraftChanged);
