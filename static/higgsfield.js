const HIGGSFIELD_STATUS = {
  pending: '待生产',
  registering: '注册中',
  waiting_code: '等待验证码',
  registered: '已注册',
  register_failed: '注册失败',
  already_registered: '邮箱已注册',
};

/* 绑卡优惠状态：need_card 表示收银台已开好、等填真实银行卡 */
const TRIAL_STATUS = {
  need_card: '待填卡',
  already_active: '已在试用',
  not_eligible: '不符条件',
  failed: '领取失败',
};

let page = 1;
const size = 20;
let higgsfieldCache = {};
const higgsfieldSelected = new Set();
let logTimer = null;
let logId = null;

async function load() {
  const q = document.getElementById('search').value.trim();
  const status = document.getElementById('filter-status').value;
  const params = new URLSearchParams({ page, size });
  if (q) params.set('q', q);
  if (status) params.set('status', status);
  const r = await api('/api/higgsfield/registrations?' + params);
  const d = await r.json();
  higgsfieldCache = {};
  (d.data || []).forEach(x => { higgsfieldCache[x.id] = x; });
  document.getElementById('rows').innerHTML = (d.data || []).map(rowHtml).join('')
    || '<tr><td colspan="7" style="text-align:center;color:var(--text-3)">暂无 Higgsfield 数据</td></tr>';
  const maxPage = Math.max(1, Math.ceil((d.total || 0) / size));
  renderPager('pager', page, maxPage, p => { page = p; load(); });
  syncBatchBar();
}

function rowHtml(x) {
  const canDownload = x.status === 'registered';
  return `
    <tr class="${higgsfieldSelected.has(x.id) ? 'row-sel' : ''}">
      <td class="col-check"><input type="checkbox" ${higgsfieldSelected.has(x.id) ? 'checked' : ''} onclick="toggleSelect(${x.id}, this.checked)"></td>
      <td>${esc(x.email)}</td>
      <td>${fmtTime(x.created_at)}</td>
      <td><span class="badge ${esc(x.status)}">${HIGGSFIELD_STATUS[x.status] || esc(x.status)}</span></td>
      <td>${trialCell(x)}</td>
      <td class="ship-cell"><span class="badge ${x.shipped ? 'registered' : 'pending'}" title="导出后自动标记，不能手动修改">${x.shipped ? '已出库' : '未出库'}</span></td>
      <td>
        <button class="icon-btn" title="日志" onclick="showLog(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h5"/></svg>
        </button>
        <button class="icon-btn" title="领取绑卡优惠（只开收银台，不会自动提交卡号）" ${canDownload ? '' : 'disabled'} onclick="runTrial(${x.id})" style="font-size:10px;font-weight:700">绑卡</button>
        <button class="icon-btn" title="导出 Cookie 字符串" ${canDownload ? '' : 'disabled'} onclick="downloadHiggsfield(${x.id}, 'string')">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/><path d="M12 15V3"/></svg>
        </button>
        <button class="icon-btn" title="导出 Cookie JSON" ${canDownload ? '' : 'disabled'} onclick="downloadHiggsfield(${x.id}, 'json')" style="font-size:10px;font-weight:700">JSON</button>
        <button class="icon-btn danger" title="删除" onclick="del(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/></svg>
        </button>
      </td>
    </tr>`;
}

function trialCell(x) {
  if (!x.trial_status) return '<span class="badge pending" title="尚未领取">未领</span>';
  const cls = x.trial_status === 'need_card' || x.trial_status === 'already_active' ? 'registered'
    : (x.trial_status === 'failed' ? 'register_failed' : 'pending');
  const label = TRIAL_STATUS[x.trial_status] || esc(x.trial_status);
  if (x.checkout_url) {
    return `<a class="badge ${cls}" href="${esc(x.checkout_url)}" target="_blank" rel="noopener" title="打开 Stripe 收银台填卡">${label}</a>`;
  }
  return `<span class="badge ${cls}">${label}</span>`;
}

/* ===== 生产进度 ===== */
async function loadProduce() {
  try {
    const r = await api('/api/higgsfield/produce/status');
    const s = await r.json();
    document.getElementById('pd-pending').textContent = s.pending || 0;
    document.getElementById('pd-running').textContent = s.running_num || 0;
    document.getElementById('pd-registered').textContent = s.registered || 0;
    document.getElementById('pd-failed').textContent = s.failed || 0;
    document.getElementById('pd-stop').style.display = s.running ? '' : 'none';
  } catch (e) { /* ignore */ }
}

function openProduceModal() {
  document.getElementById('produce-count').value = 10;
  document.getElementById('produce-modal').style.display = 'flex';
}

async function startProduce() {
  const count = parseInt(document.getElementById('produce-count').value, 10);
  if (!count || count < 1) return toast('请输入有效数量', true);
  const r = await api('/api/higgsfield/produce', {
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
  if (!confirm('确定停止当前所有 Higgsfield 生产任务?')) return;
  await api('/api/higgsfield/produce/stop', { method: 'POST' });
  toast('已请求停止');
  loadProduce();
}

/* ===== 绑卡优惠 ===== */
async function runTrial(id) {
  toast('正在领取绑卡优惠 ...');
  const r = await api('/api/higgsfield/registrations/' + id + '/trial', { method: 'POST' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '领取失败', true);
  if (d.state === 'need_card') toast('收银台已开好，点「待填卡」打开后填真实银行卡');
  else toast('结果: ' + (TRIAL_STATUS[d.state] || d.state));
  load();
}

/* ===== 多选 ===== */
function toggleSelect(id, checked) {
  if (checked) higgsfieldSelected.add(id); else higgsfieldSelected.delete(id);
  syncBatchBar();
}
function toggleSelectAll(checked) {
  Object.keys(higgsfieldCache).forEach(id => {
    if (checked) higgsfieldSelected.add(Number(id)); else higgsfieldSelected.delete(Number(id));
  });
  load();
}
function clearSelection() { higgsfieldSelected.clear(); load(); }
function syncBatchBar() {
  const bar = document.getElementById('higgsfield-batch');
  bar.style.display = higgsfieldSelected.size ? 'flex' : 'none';
  document.getElementById('higgsfield-batch-count').textContent = '已选 ' + higgsfieldSelected.size + ' 项';
  const all = document.getElementById('higgsfield-check-all');
  const ids = Object.keys(higgsfieldCache).map(Number);
  all.checked = ids.length > 0 && ids.every(id => higgsfieldSelected.has(id));
}

/* ===== 导出 Cookie（string 字符串 / json 对象 / array 批量数组；导出即出库） ===== */
async function downloadHiggsfield(id, format) {
  await downloadByIds([id], format);
}
async function downloadSelected(format) {
  const ids = [...higgsfieldSelected];
  if (!ids.length) return;
  await downloadByIds(ids, format);
}
/* 一键导出全部"已注册未出库"，无需先勾选。 */
async function downloadUnshipped(format) {
  await downloadByIds([], format, true);
}
async function downloadByIds(ids, format, unshippedOnly) {
  const r = await api('/api/higgsfield/download', {
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
  const fallback = format === 'array' ? 'higgsfield_cookies_array.json'
    : format === 'json' ? 'higgsfield_cookies.json' : 'higgsfield_cookies.txt';
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = match ? match[1] : fallback;
  a.click();
  URL.revokeObjectURL(a.href);
  load();
}

/* ===== 删除 ===== */
async function delSelected() {
  const ids = [...higgsfieldSelected];
  if (!ids.length) return;
  if (!confirm('确定删除所选 ' + ids.length + ' 个 Higgsfield 账号？')) return;
  for (const id of ids) {
    await api('/api/higgsfield/registrations/' + id, { method: 'DELETE' });
    higgsfieldSelected.delete(id);
  }
  toast('已删除 ' + ids.length + ' 个');
  load();
}
async function del(id) {
  if (!confirm('确定删除 Higgsfield 账号 #' + id + '？')) return;
  const r = await api('/api/higgsfield/registrations/' + id, { method: 'DELETE' });
  if (!r.ok) return toast('删除失败', true);
  higgsfieldSelected.delete(id);
  toast('已删除');
  load();
}
async function deleteAllHiggsfield() {
  if (!confirm('确定删除全部 Higgsfield 账号记录？此操作不可恢复。')) return;
  if (!confirm('再次确认：将永久删除 Higgsfield 注册中的全部记录。')) return;
  const r = await api('/api/higgsfield/registrations', { method: 'DELETE' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast('全部删除失败: ' + (d.error || r.status), true);
  higgsfieldSelected.clear();
  page = 1;
  toast('已删除 ' + (d.deleted || 0) + ' 个 Higgsfield 账号');
  load();
  loadProduce();
}

/* ===== 日志 ===== */
async function showLog(id) {
  logId = id;
  document.getElementById('log-title').textContent = '执行日志';
  document.getElementById('log-body').textContent = '加载中...';
  document.getElementById('log-modal').style.display = 'flex';
  document.body.style.overflow = 'hidden';
  await refreshLog(false);
  clearInterval(logTimer);
  logTimer = setInterval(() => refreshLog(true), 2000);
}
async function refreshLog(silent) {
  if (logId == null) return;
  const r = await api('/api/higgsfield/registrations/' + logId + '/logs');
  if (!r.ok) { if (!silent) toast('读取日志失败', true); return; }
  const d = await r.json();
  document.getElementById('log-title').textContent = '执行日志 · ' + d.email;
  document.getElementById('log-body').textContent = (d.note ? '备注: ' + d.note + '\n\n' : '') + (d.log || '（无执行日志）');
}
function closeLog() {
  clearInterval(logTimer);
  logTimer = null;
  logId = null;
  document.getElementById('log-modal').style.display = 'none';
  document.body.style.overflow = '';
}

document.getElementById('search').addEventListener('keydown', e => {
  if (e.key === 'Enter') { page = 1; load(); }
});
document.getElementById('filter-status').addEventListener('change', () => { page = 1; load(); });

load();
loadProduce();
setInterval(load, 3000);
setInterval(loadProduce, 2000);
