/* ===== Gmail 注册管理 ===== */
let gmPage = 1;
const size = 20;
let gmCache = {};
let gmLoading = false;
const gmSelected = new Set();

const GM_STATUS = {
  pending: '待注册',
  registering: '注册中',
  registered: '已注册',
  register_failed: '注册失败',
};

async function loadGmailList() {
  if (gmLoading) return;
  gmLoading = true;
  try {
    const q = document.getElementById('gm-search').value.trim();
    const status = document.getElementById('gm-filter').value;
    const params = new URLSearchParams({ page: gmPage, size });
    if (q) params.set('q', q);
    if (status) params.set('status', status);
    const r = await api('/api/gmail/registrations?' + params);
    const d = await r.json();
    gmCache = {};
    (d.data || []).forEach(x => (gmCache[x.id] = x));
    document.getElementById('gm-rows').innerHTML = (d.data || []).map(x => `
      <tr class="${gmSelected.has(x.id) ? 'row-sel' : ''}">
        <td class="col-check"><input type="checkbox" ${gmSelected.has(x.id) ? 'checked' : ''} onclick="toggleSelect(${x.id}, this.checked)"></td>
        <td>${esc(x.email)}</td>
        <td>${x.password ? `<span class="gm-pw">${esc(x.password)}</span> <button class="icon-btn" title="复制密码" onclick="copyPw(${x.id})"><svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg></button>` : '—'}</td>
        <td>${fmtTime(x.created_at)}</td>
        <td><span class="badge ${esc(x.status)}">${GM_STATUS[x.status] || esc(x.status)}</span></td>
        <td>
          ${x.status === 'registered' ? `<button class="icon-btn" title="取件" onclick="openMailModal(${x.id})">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 7 9 6 9-6"/></svg>
          </button>` : ''}
          <button class="icon-btn" title="日志" onclick="openLog(${x.id})">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6M16 13H8M16 17H8M10 9H8"/></svg>
          </button>
          ${x.status === 'registering' ? `<button class="icon-btn" title="停止" onclick="stopGmail(${x.id})">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9"><rect x="6" y="6" width="12" height="12" rx="1"/></svg>
          </button>` : ''}
          <button class="icon-btn danger" title="删除" onclick="delGmail(${x.id})">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/></svg>
          </button>
        </td>
      </tr>`).join('');
    const maxPage = Math.max(1, Math.ceil((d.total || 0) / size));
    renderPager('gm-pager', gmPage, maxPage, p => { gmPage = p; loadGmailList(); });
    syncBatchBar();
  } finally {
    gmLoading = false;
  }
}

/* ===== 多选 ===== */
function toggleSelect(id, checked) {
  if (checked) gmSelected.add(id); else gmSelected.delete(id);
  syncBatchBar();
}

function toggleSelectAll(checked) {
  Object.keys(gmCache).forEach(id => {
    if (checked) gmSelected.add(Number(id)); else gmSelected.delete(Number(id));
  });
  loadGmailList();
}

function clearSelection() {
  gmSelected.clear();
  loadGmailList();
}

function syncBatchBar() {
  const bar = document.getElementById('gm-batch');
  bar.style.display = gmSelected.size ? 'flex' : 'none';
  document.getElementById('gm-batch-count').textContent = '已选 ' + gmSelected.size + ' 项';
  const all = document.getElementById('gm-check-all');
  const ids = Object.keys(gmCache).map(Number);
  all.checked = ids.length > 0 && ids.every(id => gmSelected.has(id));
}

/* ===== 单个注册 ===== */
function openGmailModal() {
  document.getElementById('gm-username').value = '';
  document.getElementById('gm-note').value = '';
  document.getElementById('gm-modal').style.display = 'flex';
}

async function startGmail() {
  const username = document.getElementById('gm-username').value.trim() || ('user' + Math.floor(Math.random() * 100000));
  const note = document.getElementById('gm-note').value;
  const r = await api('/api/gmail/registrations', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, note }),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({}));
    return toast(e.error || '注册失败', true);
  }
  closeModal('gm-modal');
  toast('注册任务已启动');
  loadGmailList();
}

/* ===== 批量生产 ===== */
function openProduceModal() {
  document.getElementById('produce-count').value = '10';
  document.getElementById('produce-modal').style.display = 'flex';
}

async function doProduce() {
  const count = parseInt(document.getElementById('produce-count').value, 10);
  if (!count || count < 1) return toast('请输入有效数量', true);
  const r = await api('/api/gmail/produce', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ count }),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({}));
    return toast(e.error || '生产启动失败', true);
  }
  closeModal('produce-modal');
  toast('批量生产已启动');
  loadGmailList();
}

async function stopProduce() {
  await api('/api/gmail/produce/stop', { method: 'POST' });
  toast('已请求停止生产');
  loadGmailList();
}

/* ===== 生产进度轮询 ===== */
async function pollProduceStatus() {
  try {
    const r = await api('/api/gmail/produce/status');
    const d = await r.json();
    const bar = document.getElementById('gm-produce-bar');
    if (d.running) {
      bar.style.display = 'flex';
      document.getElementById('gm-produce-text').textContent =
        `生产中：在跑 ${d.running_num} · 已注册 ${d.registered} · 失败 ${d.failed} · 待生产 ${d.pending}`;
    } else {
      bar.style.display = 'none';
    }
  } catch (e) {}
}

/* ===== 删除 ===== */
async function delGmail(id) {
  if (!confirm('确定删除 #' + id + ' ?')) return;
  const r = await api('/api/gmail/registrations/' + id, { method: 'DELETE' });
  if (!r.ok) return toast('删除失败', true);
  toast('已删除');
  loadGmailList();
}

async function delSelected() {
  const ids = [...gmSelected];
  if (!ids.length) return;
  if (!confirm('确定删除所选 ' + ids.length + ' 个?')) return;
  for (const id of ids) {
    await api('/api/gmail/registrations/' + id, { method: 'DELETE' });
    gmSelected.delete(id);
  }
  toast('已删除 ' + ids.length + ' 个');
  loadGmailList();
}

async function deleteAllGmail() {
  if (!confirm('确定删除全部 Gmail 注册记录？此操作不可恢复。')) return;
  if (!confirm('再次确认：将永久删除所有 Gmail 注册记录。')) return;
  const r = await api('/api/gmail/registrations', { method: 'DELETE' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast('删除失败: ' + (d.error || r.status), true);
  gmSelected.clear();
  gmPage = 1;
  toast('已删除 ' + (d.deleted || 0) + ' 条记录');
  loadGmailList();
}

async function stopGmail(id) {
  await api('/api/gmail/registrations/' + id + '/stop', { method: 'POST' });
  toast('已请求停止');
  loadGmailList();
}

/* ===== 复制密码 ===== */
function copyPw(id) {
  const reg = gmCache[id];
  if (!reg || !reg.password) return;
  navigator.clipboard.writeText(reg.password)
    .then(() => toast('密码已复制: ' + reg.password))
    .catch(() => toast('复制失败，密码: ' + reg.password, true));
}

/* ===== 导出 ===== */
async function exportSelected(format) {
  const ids = [...gmSelected];
  if (!ids.length) return toast('请先勾选要导出的账号', true);
  const r = await api('/api/gmail/download', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids, format }),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({}));
    return toast(e.error || '导出失败', true);
  }
  const blob = await r.blob();
  const cd = r.headers.get('Content-Disposition') || '';
  const m = cd.match(/filename="(.+?)"/);
  const fname = m ? m[1] : 'gmail_export.' + (format === 'json' ? 'json' : 'txt');
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = fname;
  a.click();
  URL.revokeObjectURL(a.href);
  toast('导出成功');
  gmSelected.clear();
  loadGmailList();
}

/* ===== 日志 ===== */
let logTimer = null;
function openLog(id) {
  document.getElementById('log-title').textContent = '执行日志 #' + id;
  document.getElementById('log-body').textContent = '加载中...';
  document.getElementById('log-modal').style.display = 'flex';
  fetchLog(id);
  if (logTimer) clearInterval(logTimer);
  logTimer = setInterval(() => fetchLog(id), 2000);
}

function closeLog() {
  if (logTimer) { clearInterval(logTimer); logTimer = null; }
  document.getElementById('log-modal').style.display = 'none';
}

async function fetchLog(id) {
  const r = await api('/api/gmail/registrations/' + id + '/logs');
  const d = await r.json().catch(() => ({}));
  const body = document.getElementById('log-body');
  body.textContent = d.log || '暂无日志';
  body.scrollTop = body.scrollHeight;
}

/* ===== 加入邮箱池 ===== */
async function toMailbox() {
  const ids = [...gmSelected];
  if (!ids.length) return toast('请先勾选账号', true);
  const r = await api('/api/gmail/to-mailbox', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '加入失败', true);
  toast(`已加入邮箱池：新增 ${d.added}，跳过 ${d.skipped}`);
  gmSelected.clear();
  loadGmailList();
}

/* ===== 取件弹窗：打开开始轮询（3 秒），关闭立即停止 ===== */
let mailTimer = null;
let mailBoxId = null;
let mailMsgs = [];
let mailSelected = 0;
let mailFetching = false;
let mailListSig = '';
const mailBodyCache = {};

function openMailModal(id) {
  const reg = gmCache[id];
  if (!reg) return;
  mailBoxId = id;
  mailMsgs = [];
  mailSelected = 0;
  mailListSig = '';
  document.getElementById('mail-title').textContent = reg.email;
  document.getElementById('mail-sub').textContent = '收件箱 · Gmail IMAP · 3 秒自动刷新';
  document.getElementById('mail-list').innerHTML = '<div class="mail-empty">加载中...</div>';
  document.getElementById('mail-meta').innerHTML = '';
  document.getElementById('mail-frame').srcdoc = '';
  document.getElementById('mail-modal').style.display = 'flex';
  document.body.classList.add('modal-open');
  fetchMail();
  mailTimer = setInterval(fetchMail, 3000);
}

function closeMailModal() {
  if (mailTimer) { clearInterval(mailTimer); mailTimer = null; }
  mailBoxId = null;
  mailMsgs = [];
  document.getElementById('mail-modal').style.display = 'none';
  document.body.classList.remove('modal-open');
}

async function fetchMail() {
  if (mailFetching || mailBoxId === null) return;
  mailFetching = true;
  const id = mailBoxId;
  try {
    const r = await api('/api/gmail/registrations/' + id + '/messages');
    if (id !== mailBoxId) return;
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
      document.getElementById('mail-list').innerHTML =
        `<div class="mail-empty err">${esc(d.error || '取件失败')}</div>`;
      return;
    }
    const prevId = msgKey(mailMsgs[mailSelected]);
    mailMsgs = mergeMsgs(mailMsgs, d.items || []);
    const i = prevId ? mailMsgs.findIndex(m => msgKey(m) === prevId) : -1;
    mailSelected = i >= 0 ? i : 0;
    renderMailList();
    renderMailDetail();
  } finally {
    mailFetching = false;
  }
}

function msgKey(m) {
  if (!m) return '';
  return m.id || (m.subject + '|' + m.received_at);
}

function mergeMsgs(existing, incoming) {
  const map = new Map();
  (existing || []).forEach(m => map.set(msgKey(m), m));
  (incoming || []).forEach(m => map.set(msgKey(m), m));
  return [...map.values()].sort((a, b) =>
    new Date(b.received_at || 0) - new Date(a.received_at || 0));
}

function renderMailList() {
  const list = document.getElementById('mail-list');
  if (!mailMsgs.length) {
    mailListSig = '';
    list.innerHTML = '<div class="mail-empty">暂无邮件，等待新邮件...</div>';
    return;
  }
  const sig = mailSelected + '#' + mailMsgs.map(msgKey).join(',');
  if (sig === mailListSig) return;
  mailListSig = sig;
  list.innerHTML = mailMsgs.map((m, i) => `
    <div class="mail-item${i === mailSelected ? ' active' : ''}" onclick="selectMail(${i})">
      <div class="mail-item-from">${esc(m.from_name || m.from)}</div>
      <div class="mail-item-subject">${esc(m.subject)}</div>
      <div class="mail-item-time">${fmtTime(m.received_at)}</div>
    </div>`).join('');
}

function selectMail(i) {
  mailSelected = i;
  renderMailList();
  renderMailDetail();
}

function renderMailDetail() {
  const m = mailMsgs[mailSelected];
  const meta = document.getElementById('mail-meta');
  const frame = document.getElementById('mail-frame');
  if (!m) {
    meta.innerHTML = '';
    setFrameHTML(frame, '');
    frame.dataset.cur = '';
    return;
  }
  meta.innerHTML = `
    <div class="mail-subject">${esc(m.subject)}</div>
    <div class="mail-from">${esc(m.from_name || '')} &lt;${esc(m.from)}&gt;</div>
    <div class="mail-time">${fmtTime(m.received_at)}</div>`;
  const body = mailBodyCache[m.id];
  if (!body) {
    if (frame.dataset.cur !== 'loading:' + m.id) {
      frame.dataset.cur = 'loading:' + m.id;
      setFrameHTML(frame, '<!doctype html><meta charset="utf-8"><div style="padding:20px;color:#888">加载中...</div>');
    }
    loadMailBody(m.id);
    return;
  }
  const html = body.html || `<pre style="white-space:pre-wrap;font-family:inherit">${esc(body.text)}</pre>`;
  if (frame.dataset.cur !== 'body:' + m.id) {
    frame.dataset.cur = 'body:' + m.id;
    setFrameHTML(frame, html);
  }
}

const FRAME_BASE_CSS = '<style>::-webkit-scrollbar{width:0;height:0}html{scrollbar-width:none}</style>';

function setFrameHTML(frame, html) {
  const content = FRAME_BASE_CSS + (html || '<!doctype html><meta charset="utf-8">');
  try {
    const doc = frame.contentDocument || (frame.contentWindow && frame.contentWindow.document);
    if (doc) { doc.open(); doc.write(content); doc.close(); return; }
  } catch (e) {}
  frame.srcdoc = content;
}

async function loadMailBody(msgId) {
  if (mailBodyCache[msgId] || mailBoxId === null) return;
  const boxId = mailBoxId;
  const r = await api('/api/gmail/registrations/' + boxId + '/message?mid=' + encodeURIComponent(msgId));
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return;
  mailBodyCache[msgId] = { html: d.html || '', text: d.text || '' };
  const cur = mailMsgs[mailSelected];
  if (boxId === mailBoxId && cur && cur.id === msgId) renderMailDetail();
}

/* 点遮罩关闭取件弹窗 */
document.getElementById('mail-modal').addEventListener('click', e => {
  if (e.target === e.currentTarget) closeMailModal();
});

/* ===== 自动刷新 ===== */
setInterval(() => {
  if (!document.hidden) {
    loadGmailList();
    pollProduceStatus();
  }
}, 3000);

document.getElementById('gm-search').addEventListener('keydown', e => {
  if (e.key === 'Enter') { gmPage = 1; loadGmailList(); }
});
document.getElementById('gm-filter').addEventListener('change', () => { gmPage = 1; loadGmailList(); });

loadGmailList();
pollProduceStatus();
