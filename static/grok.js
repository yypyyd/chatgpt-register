const GROK_STATUS = {
  pending: '待注册',
  registering: '注册中',
  waiting_code: '等待验证码',
  registered: '已注册',
  register_failed: '注册失败',
};

let page = 1;
const size = 20;
let grokCache = {};
const grokSelected = new Set();
let codeId = null;
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
    || '<tr><td colspan="6" style="text-align:center;color:var(--text-3)">暂无 Grok 数据</td></tr>';
  const maxPage = Math.max(1, Math.ceil((d.total || 0) / size));
  renderPager('pager', page, maxPage, p => { page = p; load(); });
  syncBatchBar();
}

function rowHtml(x) {
  const canDownload = x.status === 'registered';
  const waiting = x.status === 'waiting_code';
  return `
    <tr class="${grokSelected.has(x.id) ? 'row-sel' : ''}">
      <td class="col-check"><input type="checkbox" ${grokSelected.has(x.id) ? 'checked' : ''} onclick="toggleSelect(${x.id}, this.checked)"></td>
      <td>${esc(x.email)}</td>
      <td>${fmtTime(x.created_at)}</td>
      <td><span class="badge ${esc(x.status)}">${GROK_STATUS[x.status] || esc(x.status)}</span></td>
      <td class="ship-cell"><span class="badge ${x.shipped ? 'registered' : 'pending'}">${x.shipped ? '已出库' : '未出库'}</span></td>
      <td>
        <button class="icon-btn" title="填写验证码" ${waiting ? '' : 'disabled'} onclick="openCode(${x.id})" style="font-size:12px;font-weight:700">CODE</button>
        <button class="icon-btn" title="日志" onclick="showLog(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h5"/></svg>
        </button>
        <button class="icon-btn" title="导出会话" ${canDownload ? '' : 'disabled'} onclick="downloadByIds([${x.id}])">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/><path d="M12 15V3"/></svg>
        </button>
        <button class="icon-btn danger" title="删除" onclick="del(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/></svg>
        </button>
      </td>
    </tr>`;
}

async function startGrok() {
  const email = document.getElementById('grok-email').value.trim();
  const note = document.getElementById('grok-note').value.trim();
  if (!email) return toast('请填写邮箱', true);
  const btn = document.getElementById('grok-start-btn');
  btn.disabled = true;
  try {
    const r = await api('/api/grok/registrations', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, note }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) return toast(d.error || '启动 Grok 注册失败', true);
    toast('Grok 注册已启动，等状态变成“等待验证码”后填入邮件代码');
    page = 1;
    load();
  } finally {
    btn.disabled = false;
  }
}

async function produceGrok() {
  const count = parseInt(document.getElementById('grok-count').value, 10);
  if (!count || count < 1) return toast('请填写有效数量', true);
  const btn = document.getElementById('grok-produce-btn');
  btn.disabled = true;
  try {
    const r = await api('/api/grok/produce', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ count }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) return toast(d.error || '启动 Grok 自动注册失败', true);
    toast('已从账号管理取号 ' + (d.started || 0) + ' 个，验证码会自动读取提交');
    page = 1;
    load();
  } finally {
    btn.disabled = false;
  }
}

function openCode(id) {
  codeId = id;
  const row = grokCache[id] || {};
  document.getElementById('code-title').textContent = '填写 Grok 验证码';
  document.getElementById('code-sub').textContent = row.email ? '邮箱：' + row.email : '等待 Grok 邮件安全代码。';
  document.getElementById('code-input').value = '';
  document.getElementById('code-modal').style.display = 'flex';
  setTimeout(() => document.getElementById('code-input').focus(), 80);
}

async function submitCode() {
  const code = document.getElementById('code-input').value.trim();
  if (!code) return toast('请填写验证码', true);
  const r = await api('/api/grok/registrations/' + codeId + '/code', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '提交验证码失败', true);
  closeModal('code-modal');
  toast('验证码已提交');
  load();
}

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

async function downloadSelected() {
  const ids = [...grokSelected];
  if (!ids.length) return;
  await downloadByIds(ids);
}
async function downloadByIds(ids) {
  const r = await api('/api/grok/download', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
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
  a.download = match ? match[1] : 'grok_auth.json';
  a.click();
  URL.revokeObjectURL(a.href);
  load();
}

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
  const r = await api('/api/grok/registrations', { method: 'DELETE' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast('全部删除失败: ' + (d.error || r.status), true);
  grokSelected.clear();
  page = 1;
  toast('已删除 ' + (d.deleted || 0) + ' 个 Grok 账号');
  load();
}

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

document.getElementById('search').addEventListener('keydown', e => {
  if (e.key === 'Enter') { page = 1; load(); }
});
document.getElementById('filter-status').addEventListener('change', () => { page = 1; load(); });
document.getElementById('code-input').addEventListener('keydown', e => {
  if (e.key === 'Enter') submitCode();
});

load();
setInterval(load, 3000);
