const GROK_STATUS = {
  pending: '待生产',
  registering: '注册中',
  waiting_code: '等待验证码',
  registered: '已注册',
  register_failed: '注册失败',
};

let page = 1;
const size = 20;
let grokCache = {};
const grokSelected = new Set();
let logTimer = null;
let logId = null;

async function load() {
  const q = document.getElementById('search').value.trim();
  const status = document.getElementById('filter-status').value;
  const params = new URLSearchParams({ page, size });
  if (q) params.set('q', q);
  if (status) params.set('status', status);
  const r = await api('/api/grok/registrations?' + params);
  const d = await r.json();
  grokCache = {};
  (d.data || []).forEach(x => { grokCache[x.id] = x; });
  document.getElementById('rows').innerHTML = (d.data || []).map(rowHtml).join('')
    || '<tr><td colspan="7" style="text-align:center;color:var(--text-3)">暂无 Grok 数据</td></tr>';
  const maxPage = Math.max(1, Math.ceil((d.total || 0) / size));
  renderPager('pager', page, maxPage, p => { page = p; load(); });
  syncBatchBar();
}

function rowHtml(x) {
  const canDownload = x.status === 'registered';
  return `
    <tr class="${grokSelected.has(x.id) ? 'row-sel' : ''}">
      <td class="col-check"><input type="checkbox" ${grokSelected.has(x.id) ? 'checked' : ''} onclick="toggleSelect(${x.id}, this.checked)"></td>
      <td>${esc(x.email)}</td>
      <td>${fmtTime(x.created_at)}</td>
      <td><span class="badge ${esc(x.status)}">${GROK_STATUS[x.status] || esc(x.status)}</span></td>
      <td>${aliveCell(x)}</td>
      <td class="ship-cell"><span class="badge ${x.shipped ? 'registered' : 'pending'}" title="下载后自动标记，不能手动修改">${x.shipped ? '已出库' : '未出库'}</span></td>
      <td>
        <button class="icon-btn" title="日志" onclick="showLog(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h5"/></svg>
        </button>
        <button class="icon-btn" title="测活" onclick="liveCheckOne(${x.id})" style="font-size:10px;font-weight:700">测活</button>
        <button class="icon-btn" title="下载 Console" ${canDownload ? '' : 'disabled'} onclick="downloadGrok(${x.id}, 'console')">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/><path d="M12 15V3"/></svg>
        </button>
        <button class="icon-btn" title="下载 Sub2API" ${canDownload ? '' : 'disabled'} onclick="downloadGrok(${x.id}, 'sub2api')" style="font-size:10px;font-weight:700">Sub</button>
        <button class="icon-btn danger" title="删除" onclick="del(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/></svg>
        </button>
      </td>
    </tr>`;
}

/* ===== 生产进度 ===== */
async function loadProduce() {
  try {
    const r = await api('/api/grok/produce/status');
    const s = await r.json();
    document.getElementById('pd-pending').textContent = s.pending || 0;
    document.getElementById('pd-running').textContent = s.running_num || 0;
    document.getElementById('pd-registered').textContent = s.registered || 0;
    document.getElementById('pd-failed').textContent = s.failed || 0;
    document.getElementById('pd-stop').style.display = s.running ? '' : 'none';
  } catch (e) { /* ignore */ }
}

/* 浏览器就绪状态：未就绪禁用生产 */
let browserReady = true;
async function loadBrowserGate() {
  try {
    const s = await (await api('/api/browser/status')).json();
    browserReady = !!s.ready;
    const btn = document.getElementById('produce-btn');
    if (btn) {
      btn.disabled = !browserReady;
      btn.title = browserReady ? '' : (s.message || '缺少浏览器');
    }
  } catch (e) { /* ignore */ }
}

function openProduceModal() {
  if (!browserReady) return toast('缺少浏览器，正在下载或下载失败，暂不能生产', true);
  document.getElementById('produce-count').value = 10;
  document.getElementById('produce-modal').style.display = 'flex';
}

async function startProduce() {
  const count = parseInt(document.getElementById('produce-count').value, 10);
  if (!count || count < 1) return toast('请输入有效数量', true);
  const r = await api('/api/grok/produce', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ count }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '启动生产失败', true);
  closeModal('produce-modal');
  toast('已从账号管理取号 ' + (d.started || 0) + ' 个，验证码会自动读取提交');
  page = 1;
  loadProduce();
  load();
}

async function stopProduce() {
  if (!confirm('确定停止当前所有 Grok 生产任务?')) return;
  await api('/api/grok/produce/stop', { method: 'POST' });
  toast('已请求停止');
  loadProduce();
}

/* ===== 多选 ===== */
function toggleSelect(id, checked) {
  if (checked) grokSelected.add(id); else grokSelected.delete(id);
  syncBatchBar();
}
function toggleSelectAll(checked) {
  Object.keys(grokCache).forEach(id => {
    if (checked) grokSelected.add(Number(id)); else grokSelected.delete(Number(id));
  });
  load();
}
function clearSelection() { grokSelected.clear(); load(); }
function syncBatchBar() {
  const bar = document.getElementById('grok-batch');
  bar.style.display = grokSelected.size ? 'flex' : 'none';
  document.getElementById('grok-batch-count').textContent = '已选 ' + grokSelected.size + ' 项';
  const all = document.getElementById('grok-check-all');
  const ids = Object.keys(grokCache).map(Number);
  all.checked = ids.length > 0 && ids.every(id => grokSelected.has(id));
}

/* ===== 下载（Console 账号导入 JSON / Sub2API sso 池 JSON；下载即出库） ===== */
async function downloadGrok(id, format) {
  await downloadByIds([id], format);
}
async function downloadSelected(format) {
  const ids = [...grokSelected];
  if (!ids.length) return;
  await downloadByIds(ids, format);
}
/* 一键导出全部已注册未出库的账号，无需先勾选。 */
async function downloadUnshipped(format) {
  await downloadByIds([], format, true);
}
async function downloadByIds(ids, format, unshippedOnly) {
  const r = await api('/api/grok/download', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids, format, unshipped_only: !!unshippedOnly }),
  });
  if (!r.ok) {
    const d = await r.json().catch(() => ({}));
    return toast(d.error || '导出失败', true);
  }
  const blob = await r.blob();
  const disposition = r.headers.get('Content-Disposition') || '';
  const match = disposition.match(/filename="?([^";]+)"?/i);
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = match ? match[1] : (format === 'sub2api' ? 'grok2api_token.json' : 'grok_console_token.json');
  a.click();
  URL.revokeObjectURL(a.href);
  load();
}

/* ===== 删除 ===== */
async function delSelected() {
  const ids = [...grokSelected];
  if (!ids.length) return;
  if (!confirm('确定删除所选 ' + ids.length + ' 个 Grok 账号？')) return;
  for (const id of ids) {
    await api('/api/grok/registrations/' + id, { method: 'DELETE' });
    grokSelected.delete(id);
  }
  toast('已删除 ' + ids.length + ' 个');
  load();
}
async function del(id) {
  if (!confirm('确定删除 Grok 账号 #' + id + '？')) return;
  const r = await api('/api/grok/registrations/' + id, { method: 'DELETE' });
  if (!r.ok) return toast('删除失败', true);
  grokSelected.delete(id);
  toast('已删除');
  load();
}
async function deleteAllGrok() {
  if (!confirm('确定删除全部 Grok 账号记录？此操作不可恢复。')) return;
  if (!confirm('再次确认：将永久删除 Grok 注册中的全部记录。')) return;
  const r = await api('/api/grok/registrations', { method: 'DELETE' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast('全部删除失败: ' + (d.error || r.status), true);
  grokSelected.clear();
  page = 1;
  toast('已删除 ' + (d.deleted || 0) + ' 个 Grok 账号');
  load();
  loadProduce();
}

/* ===== 日志 ===== */
async function showLog(id) {
  logId = id;
  document.getElementById('log-title').textContent = '执行日志';
  document.getElementById('log-body').textContent = '加载中...';
  document.getElementById('log-shot-btn').style.display = 'none';
  document.getElementById('log-modal').style.display = 'flex';
  document.body.style.overflow = 'hidden';
  await refreshLog(false);
  clearInterval(logTimer);
  logTimer = setInterval(() => refreshLog(true), 2000);
}
async function refreshLog(silent) {
  if (logId == null) return;
  const r = await api('/api/grok/registrations/' + logId + '/logs');
  if (!r.ok) { if (!silent) toast('读取日志失败', true); return; }
  const d = await r.json();
  document.getElementById('log-title').textContent = '执行日志 · ' + d.email;
  document.getElementById('log-shot-btn').style.display = d.has_shot ? '' : 'none';
  document.getElementById('log-body').textContent = (d.note ? '备注: ' + d.note + '\n\n' : '') + (d.log || '（无执行日志）');
}
function closeLog() {
  clearInterval(logTimer);
  logTimer = null;
  logId = null;
  document.getElementById('log-modal').style.display = 'none';
  document.body.style.overflow = '';
}

/* ===== 异常截图 ===== */
async function viewShot() {
  if (logId == null) return;
  const r = await api('/api/grok/registrations/' + logId + '/shot');
  if (!r.ok) return toast('暂无异常截图', true);
  const blob = await r.blob();
  const img = document.getElementById('shot-img');
  if (img.dataset.url) URL.revokeObjectURL(img.dataset.url);
  img.src = img.dataset.url = URL.createObjectURL(blob);
  document.getElementById('shot-modal').style.display = 'flex';
}
function closeShot() {
  document.getElementById('shot-modal').style.display = 'none';
}

/* ===== 测活（手动，仅点击触发；只标状态不删号；unknown 不判死） ===== */
const LIVE_BASE = '/api/grok/registrations';
const ALIVE_LABEL = { alive: '有效', dead: '失效', unknown: '未知' };
let liveTimer = null;
function aliveCell(x) {
  if (!x.alive) return '<span class="badge pending" title="尚未测活">未测</span>';
  const cls = x.alive === 'alive' ? 'registered' : (x.alive === 'dead' ? 'register_failed' : 'pending');
  const t = x.alive_checked_at ? '最近检测: ' + fmtTime(x.alive_checked_at) : '';
  const quota = x.console_quota ? `<span class="quota-cell" title="Grok Console 额度">${esc(x.console_quota)}</span>` : '';
  return `<span class="badge ${cls}" title="${t}">${ALIVE_LABEL[x.alive] || esc(x.alive)}</span>${quota}`;
}
async function liveCheckOne(id) {
  toast('正在测活 #' + id + ' ...');
  const r = await api(LIVE_BASE + '/' + id + '/livecheck', { method: 'POST' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '测活失败', true);
  toast('测活完成: ' + (ALIVE_LABEL[d.alive] || d.alive) + (d.console_quota ? '（' + d.console_quota + '）' : ''));
  load();
}
function liveCheckAll() {
  if (!confirm('对全部已注册账号执行测活？只更新存活状态，不会删除账号。')) return;
  startLiveCheck([]);
}
function liveCheckSelected() {
  const ids = [...grokSelected];
  if (!ids.length) return toast('请先勾选账号', true);
  startLiveCheck(ids);
}
async function startLiveCheck(ids) {
  const r = await api(LIVE_BASE + '/livecheck', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '启动测活失败', true);
  toast('已开始测活 ' + (d.total || 0) + ' 个账号');
  pollLive();
}
function pollLive() {
  clearInterval(liveTimer);
  const el = document.getElementById('live-progress');
  const tick = async () => {
    try {
      const s = await (await api(LIVE_BASE + '/livecheck/status')).json();
      if (el && (s.running || s.done)) {
        el.style.display = '';
        const summary = `有效 ${s.alive} · 失效 ${s.dead} · 未知 ${s.unknown}`;
        el.textContent = s.running ? `测活中 ${s.done}/${s.total} · ${summary}` : `测活完成 · ${summary}`;
        if (!s.running) setTimeout(() => { if (el) el.style.display = 'none'; }, 8000);
      }
      if (!s.running) clearInterval(liveTimer);
    } catch (e) { /* ignore */ }
  };
  tick();
  liveTimer = setInterval(tick, 1500);
}

document.getElementById('search').addEventListener('keydown', e => {
  if (e.key === 'Enter') { page = 1; load(); }
});
document.getElementById('filter-status').addEventListener('change', () => { page = 1; load(); });

load();
loadProduce();
loadBrowserGate();
pollLive();
setInterval(load, 3000);
setInterval(loadProduce, 2000);
setInterval(loadBrowserGate, 2500);
