'use strict';
/* ══ 密钥（一人一 key：各自用量 / 限额 / 可用账号）══════════════════════════
   明文只在「新建 / 轮换」的响应里出现一次：面板其它地方一律回显掩码。
   编辑复用同一张表单（keyEditId 为空 = 新建），避免维护两套字段。 */
let keyRows = [];
let keyEditId = null;
let keyAccounts = [];        // 账号候选（含积分，来自 /panel/api/keys）
let keyArchiveOn = true;     // 全局对话归档开关（密钥上的 content_log 只是允许）
let keyModelCatalog = null;  // 模型目录缓存：/panel/api/models 直连上游，一次会话只拉一次
let keyModelSelected = new Set();
let keyModelCustom = [];

function fmtNum(n) { return (Number(n) || 0).toLocaleString(); }
function keyNum(v) { const n = Number(v); return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0; }
function shortUid(uid) { return !uid ? '' : (uid.length > 10 ? uid.slice(0, 10) + '…' : uid); }

/* ── 弹窗 ─────────────────────────────────────────────────────────────
   复用面板既有的 .veil/.dlg（积分任务、券码弹窗同一套外观），只多两种关法：
   Esc 与点空白处。这两个只在密钥弹窗上生效，不改动其它弹窗的行为。 */
function openVeil(id) {
  const v = $(id);
  if (!v) return;
  v.classList.add('on');
  document.body.classList.add('veil-open');
  const f = v.querySelector('input:not([type=checkbox]):not([readonly]), select');
  if (f) setTimeout(() => f.focus(), 60);
}
function closeVeil(id) {
  const v = $(id);
  if (!v) return;
  v.classList.remove('on');
  if (!document.querySelector('.veil.on')) document.body.classList.remove('veil-open');
}
['keyVeil', 'keySecretVeil'].forEach(id => {
  const v = $(id);
  if (!v) return;
  v.addEventListener('click', e => { if (e.target === v) closeVeil(id); });
});
document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if ($('keySecretVeil') && $('keySecretVeil').classList.contains('on')) closeVeil('keySecretVeil');
  else if ($('keyVeil') && $('keyVeil').classList.contains('on')) closeVeil('keyVeil');
});

async function loadKeys() {
  const tb = $('keysBody');
  try {
    const d = await api('keys');
    keyRows = d.keys || [];
    keyAccounts = d.accounts || [];
    keyArchiveOn = d.content_archive !== false;
    fillKeyPinOptions();
    renderKeys();
    $('keysNote').textContent = keyRows.length ? keyRows.length + ' 个密钥' : '还没有分发密钥';
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="10"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}

/* ── 可用账号：勾选列表 ───────────────────────────────────────────────
   取代 <select multiple>：原生多选框要点 Ctrl、看不到积分，勾选状态也不直观。 */
function renderKeyAccountPick(selected) {
  const box = $('keyAccountPick');
  if (!box) return;
  if (!keyAccounts.length) { box.innerHTML = '<div class="pick-empty">暂无账号</div>'; return; }
  const sel = selected instanceof Set ? selected : new Set();
  box.innerHTML = keyAccounts.map(a => {
    const st = a.disabled ? '（禁用）' : (a.paused ? '（暂停）' : '');
    return '<label class="pick-item"><input type="checkbox" value="' + esc(a.uid) + '"' +
      (sel.has(a.uid) ? ' checked' : '') + '>' +
      '<span class="nm">' + esc(a.label || shortUid(a.uid)) + st + '</span>' +
      '<span class="meta">' + esc(a.realm || 'cn') + ' · ' + fmtNum(a.credits) + '</span></label>';
  }).join('');
}
function pickedAccounts() {
  const box = $('keyAccountPick');
  return box ? [...box.querySelectorAll('input:checked')].map(i => i.value) : [];
}

/* 「固定账号」下拉：与勾选列表共用同一份账号候选。 */
function fillKeyPinOptions() {
  const pin = $('keyPin');
  if (!pin) return;
  const prev = pin.value;
  pin.innerHTML = '<option value="">不固定</option>' + keyAccounts.map(a =>
    '<option value="' + esc(a.uid) + '">' +
    esc('[' + (a.realm || 'cn') + '] ' + (a.label || shortUid(a.uid)) + ' · ' + fmtNum(a.credits)) +
    (a.disabled ? '（禁用）' : (a.paused ? '（暂停）' : '')) + '</option>').join('');
  pin.value = [...pin.options].some(o => o.value === prev) ? prev : '';
}

/* ── 模型白名单：按真实模型目录勾选 ───────────────────────────────────
   白名单最终仍是一组「模型 id / 通配规则」，勾选只负责产出精确 id；
   目录外的写法（deepseek-* 之类）作为自定义规则单独展示，不静默丢掉。 */
function modelBare(id) { const i = id.indexOf(':'); return i < 0 ? id : id.slice(i + 1); }
function modelRealm(id) { const i = id.indexOf(':'); return i < 0 ? 'cn' : id.slice(0, i); }

async function ensureKeyModelCatalog() {
  if (keyModelCatalog) return keyModelCatalog;
  const box = $('keyModelPick');
  if (box) box.innerHTML = '<div class="pick-empty">正在向上游拉取模型目录…</div>';
  try {
    const d = await api('models');
    // 按域分组（CN 在前）再按名字排：55 个模型平铺一列时，用户找不到自己那一个。
    const rank = r => (r === 'cn' ? 0 : 1);
    keyModelCatalog = (d.models || []).slice().sort((a, b) => {
      const ra = rank(modelRealm(a.id)), rb = rank(modelRealm(b.id));
      if (ra !== rb) return ra - rb;
      return modelBare(a.id).localeCompare(modelBare(b.id));
    });
  } catch (e) {
    keyModelCatalog = []; // 失败不缓存：下次打开弹窗重试
    if (box) box.innerHTML = '<div class="pick-empty">模型目录拉取失败：' + esc(e.message) + '</div>';
    return keyModelCatalog;
  }
  return keyModelCatalog;
}

function renderKeyModelPick() {
  const box = $('keyModelPick');
  if (!box) return;
  if (!keyModelCatalog || !keyModelCatalog.length) {
    box.innerHTML = '<div class="pick-empty">模型目录为空（不影响保存，可改用下面的自定义规则）</div>';
    renderKeyModelCustom();
    return;
  }
  const q = ($('keyModelSearch').value || '').trim().toLowerCase();
  const rows = keyModelCatalog.filter(m =>
    !q || (m.id + ' ' + (m.name || '') + ' ' + (m.vendor || '')).toLowerCase().includes(q));
  if (!rows.length) {
    box.innerHTML = '<div class="pick-empty">没有匹配的模型</div>';
    renderKeyModelCustom();
    return;
  }
  const html = [];
  let lastRealm = '';
  rows.forEach(m => {
    const realm = modelRealm(m.id);
    if (realm !== lastRealm) {
      html.push('<div class="pick-group">' + esc(realm.toUpperCase()) + '</div>');
      lastRealm = realm;
    }
    html.push('<label class="pick-item"><input type="checkbox" data-model="' + esc(m.id) + '"' +
      (keyModelSelected.has(m.id) ? ' checked' : '') + '>' +
      '<span class="nm">' + esc(modelBare(m.id)) + '</span>' +
      '<span class="meta">' + (m.credits ? esc(m.credits) : '') + '</span></label>');
  });
  box.innerHTML = html.join('');
  renderKeyModelCustom();
}

function renderKeyModelCustom() {
  const wrap = $('keyModelCustomWrap');
  if (!wrap) return;
  wrap.hidden = !keyModelCustom.length;
  wrap.innerHTML = keyModelCustom.map((p, i) =>
    '<span class="tag mute">' + esc(p) +
    '<button type="button" data-i="' + i + '" title="移除">✕</button></span>').join('');
}

function addKeyModelRule() {
  const inp = $('keyModelCustom');
  if (!inp) return;
  const raw = (inp.value || '').trim();
  if (!raw) return;
  const ids = new Set((keyModelCatalog || []).map(m => m.id));
  raw.split(',').map(s => s.trim()).filter(Boolean).forEach(v => {
    if (ids.has(v)) { keyModelSelected.add(v); return; }
    if (!keyModelCustom.includes(v)) keyModelCustom.push(v);
  });
  inp.value = '';
  renderKeyModelPick();
}

/* splitKeyModels 把配置里的白名单还原成「勾选 + 自定义规则」。
   手写配置常见裸名（glm-5.2），能唯一匹配到某个域就自动勾上，避免编辑一次
   就把它降级成自定义规则。 */
function splitKeyModels(patterns) {
  keyModelSelected = new Set();
  keyModelCustom = [];
  const ids = new Set((keyModelCatalog || []).map(m => m.id));
  const bare = new Map();
  (keyModelCatalog || []).forEach(m => {
    const b = modelBare(m.id);
    if (!bare.has(b)) bare.set(b, []);
    bare.get(b).push(m.id);
  });
  (patterns || []).forEach(p => {
    const t = String(p).trim();
    if (!t) return;
    if (ids.has(t)) { keyModelSelected.add(t); return; }
    const hit = bare.get(t);
    if (hit && hit.length === 1) { keyModelSelected.add(hit[0]); return; }
    keyModelCustom.push(t);
  });
}

function keyScopeText(k) {
  if (k.pin) return '固定 ' + shortUid(k.pin);
  if (k.accounts && k.accounts.length) return k.accounts.length + ' 个号';
  return '全池';
}

function renderKeys() {
  const tb = $('keysBody');
  if (!keyRows.length) {
    tb.innerHTML = '<tr><td colspan="10"><div class="empty"><div class="big">还没有密钥</div>点右上角「新建」，生成一个只属于某人的 key</div></td></tr>';
    return;
  }
  tb.innerHTML = keyRows.map(k => {
    const t = k.today || {};
    const status = k.enabled ? '<span class="tag ok">启用</span>' : '<span class="tag bad">停用</span>';
    const limits = [];
    if (k.max_rpm) limits.push(k.max_rpm + ' RPM');
    if (k.daily_tokens) limits.push(fmtNum(k.daily_tokens) + ' /日');
    const used = k.daily_tokens ? Math.min(100, Math.round((k.today_tokens || 0) / k.daily_tokens * 100)) : null;
    const quota = used == null ? ''
      : '<div class="hint" style="font-size:11px;color:' + (used >= 100 ? 'var(--bad)' : 'var(--ink-3)') + '">日额度已用 ' + used + '%</div>';
    const note = k.note ? ' · ' + esc(k.note) : '';
    return '<tr data-id="' + esc(k.id) + '">' +
      '<td><div class="nm">' + esc(k.name || k.id) + '</div>' +
        '<div class="hint" style="font-size:11px;color:var(--ink-3)">' + esc(k.id) + note + '</div></td>' +
      '<td><code class="kv" data-masked="' + esc(k.masked) + '">' + esc(k.masked) + '</code>' +
        '<div style="margin-top:3px;white-space:nowrap">' +
        '<button class="xs" data-act="reveal">查看</button> ' +
        '<button class="xs" data-act="copy" hidden>复制</button></div></td>' +
      '<td>' + status + '</td>' +
      '<td>' + fmtNum(t.requests) +
        '<div class="hint" style="font-size:11px;color:var(--ink-3)">失败 ' + fmtNum(t.failed) + '</div></td>' +
      '<td>' + fmtNum(k.today_tokens) + quota + '</td>' +
      '<td>' + fmtNum(k.total_tokens) + '</td>' +
      '<td>' + (limits.length ? esc(limits.join(' · ')) : '不限') + '</td>' +
      '<td>' + esc(keyScopeText(k)) +
        (k.min_credit ? '<div class="hint" style="font-size:11px;color:var(--ink-3)">积分 ≥ ' + k.min_credit + '</div>' : '') + '</td>' +
      '<td>' + ago(k.last_active) + '</td>' +
      '<td class="ops" style="white-space:nowrap">' +
        '<button class="xs" data-act="edit">编辑</button> ' +
        '<button class="xs" data-act="toggle">' + (k.enabled ? '停用' : '启用') + '</button> ' +
        '<button class="xs" data-act="rotate">轮换</button> ' +
        '<button class="xs" data-act="reset">清零</button> ' +
        '<button class="xs danger" data-act="delete">删除</button>' +
      '</td></tr>';
  }).join('');
}

/* openKeyEditor 打开弹窗并装载数据：k 为空 = 新建。
   模型目录要等一次上游查询，所以先开弹窗再填充，避免点击后页面「卡住」。 */
async function openKeyEditor(k) {
  keyEditId = k ? k.id : null;
  $('keyEditorTitle').textContent = k ? ('编辑密钥 · ' + (k.name || k.id)) : '新建密钥';
  $('keyId').disabled = !!k;
  $('keyName').value = k ? (k.name || '') : '';
  $('keyId').value = k ? k.id : '';
  renderKeyAccountPick(new Set(k ? (k.accounts || []) : []));
  $('keyPin').value = k ? (k.pin || '') : '';
  $('keyMinCredit').value = k && k.min_credit ? k.min_credit : '';
  $('keyMaxRpm').value = k && k.max_rpm ? k.max_rpm : '';
  $('keyDailyTokens').value = k && k.daily_tokens ? k.daily_tokens : '';
  $('keyNote').value = k ? (k.note || '') : '';
  $('keyEnabled').checked = k ? !!k.enabled : true;
  $('keyContentLog').checked = k ? !!k.content_log : false;
  // 全局归档关着的时候，勾了也不会落盘：置灰 + 明说去哪儿开，避免「勾了却看不到」。
  $('keyContentLog').disabled = !keyArchiveOn;
  $('keyContentLogHint').hidden = keyArchiveOn;
  $('keyModelSearch').value = '';
  $('keyModelCustom').value = '';
  openVeil('keyVeil');
  $('keyName').focus();
  await ensureKeyModelCatalog();
  splitKeyModels(k ? (k.models || []) : []);
  renderKeyModelPick();
}

function showNewKey(key) {
  $('keyNewValue').value = key || '';
  openVeil('keySecretVeil');
  setTimeout(() => { $('keyNewValue').focus(); $('keyNewValue').select(); }, 30);
}

if ($('btnKeySave')) $('btnKeySave').onclick = async () => {
  const body = {
    name: $('keyName').value.trim(),
    accounts: pickedAccounts(),
    pin: $('keyPin').value,
    min_credit: keyNum($('keyMinCredit').value),
    max_rpm: keyNum($('keyMaxRpm').value),
    daily_tokens: keyNum($('keyDailyTokens').value),
    models: [...keyModelSelected, ...keyModelCustom],
    note: $('keyNote').value.trim(),
    enabled: $('keyEnabled').checked,
    content_log: $('keyContentLog').checked,
  };
  const btn = $('btnKeySave');
  btn.disabled = true;
  try {
    if (keyEditId) {
      await api('keys/' + encodeURIComponent(keyEditId), { method: 'POST', body: JSON.stringify(body) });
      toast('已保存');
      closeVeil('keyVeil');
    } else {
      const id = $('keyId').value.trim();
      if (id) body.id = id;
      const d = await api('keys', { method: 'POST', body: JSON.stringify(body) });
      closeVeil('keyVeil');
      showNewKey(d.key);
      toast('密钥已创建；之后可在列表里点「查看」再取明文');
    }
    loadKeys();
  } catch (e) { toast(e.message, 'bad'); }
  finally { btn.disabled = false; }
};

if ($('btnKeyNew')) $('btnKeyNew').onclick = () => openKeyEditor(null);
if ($('btnKeysRefresh')) $('btnKeysRefresh').onclick = () => loadKeys();
if ($('btnKeyCancel')) $('btnKeyCancel').onclick = () => closeVeil('keyVeil');
if ($('btnKeySecretDone')) $('btnKeySecretDone').onclick = () => closeVeil('keySecretVeil');
if ($('btnKeyCopy')) $('btnKeyCopy').onclick = async () => {
  const v = $('keyNewValue').value;
  if (!v) return;
  try { await navigator.clipboard.writeText(v); toast('已复制'); }
  catch (e) { $('keyNewValue').select(); toast('复制失败，请手动复制', 'bad'); }
};

/* 模型勾选：状态存 JS（搜索/全选会重渲染 DOM，勾选不能丢）。 */
if ($('keyModelPick')) $('keyModelPick').addEventListener('change', e => {
  const cb = e.target.closest('input[data-model]');
  if (!cb) return;
  if (cb.checked) keyModelSelected.add(cb.dataset.model);
  else keyModelSelected.delete(cb.dataset.model);
});
if ($('keyModelSearch')) $('keyModelSearch').addEventListener('input', () => renderKeyModelPick());
if ($('btnKeyModelAll')) $('btnKeyModelAll').onclick = () => {
  const q = ($('keyModelSearch').value || '').trim().toLowerCase();
  (keyModelCatalog || []).forEach(m => {
    if (!q || (m.id + ' ' + (m.name || '') + ' ' + (m.vendor || '')).toLowerCase().includes(q)) {
      keyModelSelected.add(m.id);
    }
  });
  renderKeyModelPick();
};
if ($('btnKeyModelNone')) $('btnKeyModelNone').onclick = () => { keyModelSelected.clear(); renderKeyModelPick(); };
if ($('btnKeyModelAdd')) $('btnKeyModelAdd').onclick = addKeyModelRule;
if ($('keyModelCustom')) $('keyModelCustom').addEventListener('keydown', e => {
  if (e.key === 'Enter') { e.preventDefault(); addKeyModelRule(); }
});
if ($('keyModelCustomWrap')) $('keyModelCustomWrap').addEventListener('click', e => {
  const b = e.target.closest('button[data-i]');
  if (!b) return;
  keyModelCustom.splice(Number(b.dataset.i), 1);
  renderKeyModelCustom();
});

/* toggleKeySecret 就地展开 / 收回某个密钥的明文。
   列表接口只回掩码，明文必须显式点「查看」才向后端要——避免一打开页面
   所有 key 就平铺在屏幕上（录屏、投屏、旁人一眼就看全了）。 */
async function toggleKeySecret(tr) {
  const code = tr.querySelector('code.kv');
  if (!code) return;
  const btn = tr.querySelector('button[data-act="reveal"]');
  const copy = tr.querySelector('button[data-act="copy"]');
  if (code.dataset.shown === '1') {
    code.textContent = code.dataset.masked;
    code.dataset.shown = '';
    if (btn) btn.textContent = '查看';
    if (copy) copy.hidden = true;
    return;
  }
  const d = await api('keys/' + encodeURIComponent(tr.dataset.id) + '/secret');
  code.textContent = d.key || '';
  code.dataset.shown = '1';
  if (btn) btn.textContent = '隐藏';
  if (copy) copy.hidden = false;
}

if ($('keysBody')) $('keysBody').addEventListener('click', async e => {
  const btn = e.target.closest('button[data-act]');
  if (!btn) return;
  const tr = btn.closest('tr');
  const id = tr && tr.dataset.id;
  const k = keyRows.find(x => x.id === id);
  if (!k) return;
  const act = btn.dataset.act;
  const path = 'keys/' + encodeURIComponent(id);
  try {
    if (act === 'edit') { openKeyEditor(k); return; }
    if (act === 'reveal') { await toggleKeySecret(tr); return; }
    if (act === 'copy') {
      const code = tr.querySelector('code.kv');
      const v = code && code.textContent;
      if (!v) return;
      try { await navigator.clipboard.writeText(v); toast('已复制'); }
      catch (err) { toast('复制失败，请手动选中', 'bad'); }
      return;
    }
    if (act === 'toggle') {
      await api(path, { method: 'POST', body: JSON.stringify({ enabled: !k.enabled }) });
      toast(k.enabled ? '已停用' : '已启用');
    } else if (act === 'rotate') {
      if (!confirm('轮换后旧 key 立即失效，正在使用的人需要换新 key。确认轮换？')) return;
      const d = await api(path + '/rotate', { method: 'POST' });
      showNewKey(d.key);
      toast('已轮换；旧 key 立即失效，新 key 可随时在列表里点「查看」');
    } else if (act === 'reset') {
      if (!confirm('清零该密钥的用量统计与 RPM 窗口？（不影响账号与密钥本身）')) return;
      await api(path + '/reset', { method: 'POST' });
      toast('已清零');
    } else if (act === 'delete') {
      if (!confirm('删除后该 key 立即失效，且用量统计一并清除。确认删除？')) return;
      await api(path + '/delete', { method: 'POST' });
      toast('已删除');
    }
    loadKeys();
  } catch (err) { toast(err.message, 'bad'); }
});

/* ══ 对话归档（按密钥 / 日期回看完整请求与响应）═════════════════════════════
   归档只对「全局开关打开 + 该密钥 content_log」的请求生效；本视图只读。 */
let ctRecords = [];
let ctHasMore = false;

async function loadContent() {
  const note = $('ctNote'), list = $('ctList');
  if (!list) return;
  try {
    const d = await api('keys');
    const sel = $('ctKey');
    const prev = sel.value;
    sel.innerHTML = '<option value="">全部密钥</option>' +
      (d.keys || []).map(k => '<option value="' + esc(k.id) + '">' + esc(k.name || k.id) + '</option>').join('');
    if (prev && (d.keys || []).some(k => k.id === prev)) sel.value = prev;

    const key = sel.value;
    const dd = await api('content/days' + (key ? '?key=' + encodeURIComponent(key) : ''));
    const days = dd.days || [];
    const ds = $('ctDate');
    const prevD = ds.value;
    ds.innerHTML = days.length
      ? days.map(x => '<option value="' + esc(x) + '">' + esc(x) + '</option>').join('')
      : '<option value="">（无归档）</option>';
    if (days.indexOf(prevD) >= 0) ds.value = prevD;
    if (!days.length) {
      ctRecords = [];
      if (dd.enabled === false) {
        list.innerHTML = '<div class="empty"><div class="big">对话归档未开启</div>' +
          '去「配置 → 日志」打开「对话正文归档」，再给要归档的密钥勾上「归档对话正文」。' +
          '保存后立即生效，不用重启；全局开关关着时，密钥上勾了也不会落盘。</div>';
      } else {
        list.innerHTML = '<div class="empty"><div class="big">还没有归档</div>' +
          '全局开关已开，但没有记录：确认该密钥勾选了「归档对话正文」，并且之后确实有请求经过它。</div>';
      }
      note.textContent = '—';
      return;
    }
    const date = ds.value;
    const rd = await api('content?limit=100' +
      (key ? '&key=' + encodeURIComponent(key) : '') +
      (date ? '&date=' + encodeURIComponent(date) : ''));
    ctRecords = rd.records || [];
    ctHasMore = !!rd.has_more;
    renderContent();
    note.textContent = ctRecords.length + ' 条 · 保留 ' + (dd.keep_days || '-') + ' 天' +
      (dd.enabled === false ? ' · 归档已关闭（历史仍可查看）' : '');
  } catch (e) {
    list.innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
  }
}

function renderContent() {
  const list = $('ctList');
  if (!ctRecords.length) {
    list.innerHTML = '<div class="empty">这一天没有记录</div>';
    return;
  }
  list.innerHTML = ctRecords.map((r, i) => {
    const ok = r.status >= 200 && r.status < 300;
    const toks = (r.prompt_tokens || 0) + (r.completion_tokens || 0);
    const when = r.time ? new Date(r.time).toLocaleString() : '—';
    const summary =
      '<summary style="cursor:pointer;padding:6px 0">' +
      '<span class="tag ' + (ok ? 'ok' : 'bad') + '">' + r.status + '</span> ' +
      '<b>' + esc(r.key || '—') + '</b> · ' + esc(r.model || '—') +
      ' <span class="hint" style="color:var(--ink-3)">' + esc(when) +
      (toks ? ' · ' + fmtNum(toks) + ' tok' : '') +
      (r.stream ? ' · 流式' : '') +
      (r.truncated ? ' · 已截断' : '') +
      (r.account ? ' · ' + esc(r.account) : '') +
      '</span>' +
      '<button class="xs danger" data-act="ct-del" data-i="' + i + '"' +
      ' style="float:right" title="删除这一条归档">删除</button>' +
      '</summary>';
    const pre = 'white-space:pre-wrap;word-break:break-word;overflow:auto;background:var(--surface-2);padding:10px;border-radius:8px;margin:4px 0 0';
    // 正文/请求/响应都不在这里渲染：归档单条可能有十几 MB，展开时再按 id 取。
    const body = '<div class="ct-body" data-loaded="0">' +
      '<div class="hint" style="color:var(--ink-3);padding:4px 0">展开这一条后加载完整请求与响应…</div></div>';
    return '<details style="border-bottom:1px solid var(--line-soft)" data-date="' + esc((r.time || '').slice(0, 10)) +
      '" data-id="' + esc(r.id) + '">' + summary + body + '</details>';
  }).join('') +
    (ctHasMore
      ? '<div style="padding:12px 0"><button class="xs" id="btnCtMore">加载更多（更早的记录）</button></div>'
      : '');
}

// ctBodyHtml 渲染单条归档的完整内容（由 content/item 返回）。
function ctBodyHtml(r) {
  const pre = 'white-space:pre-wrap;word-break:break-word;overflow:auto;background:var(--surface-2);padding:10px;border-radius:8px;margin:4px 0 0';
  return '<div style="display:grid;gap:10px;padding:6px 0 12px">' +
    (r.text
      ? '<div><div class="lb">助手正文</div><pre style="' + pre + ';max-height:420px">' + esc(r.text) + '</pre></div>'
      : '<div class="hint" style="color:var(--ink-3)">本条没有可读正文（工具调用 / 仅推理型响应），展开「原始响应」看完整帧。</div>') +
    '<div><div class="lb">请求</div><pre style="' + pre + ';max-height:320px">' + esc(prettyJSON(r.request)) + '</pre></div>' +
    (r.response ? '<details><summary style="cursor:pointer;color:var(--ink-3)">原始响应</summary><pre style="' + pre + ';max-height:420px">' + esc(r.response) + '</pre></details>' : '') +
    '</div>';
}

function prettyJSON(v) {
  if (v == null) return '';
  if (typeof v === 'string') {
    try { return JSON.stringify(JSON.parse(v), null, 2); } catch (e) { return v; }
  }
  try { return JSON.stringify(v, null, 2); } catch (e) { return String(v); }
}

if ($('btnCtRefresh')) $('btnCtRefresh').onclick = () => loadContent();
if ($('ctKey')) $('ctKey').onchange = () => loadContent();
if ($('ctDate')) $('ctDate').onchange = () => loadContent();

/* 删除归档：单条 / 按当前筛选清除 / 清空全部。归档不保留副本，全部二次确认。 */
async function deleteContent(body, okMsg) {
  const d = await api('content/delete', { method: 'POST', body: JSON.stringify(body) });
  toast(okMsg + (typeof d.deleted === 'number' && d.deleted > 0 ? '（' + d.deleted + ' 条）' : ''));
  await loadContent();
}

if ($('ctList')) $('ctList').addEventListener('click', async e => {
  if (e.target.id === 'btnCtMore') {
    e.preventDefault();
    await loadMoreContent();
    return;
  }
  const b = e.target.closest('button[data-act="ct-del"]');
  if (!b) return;
  e.preventDefault();
  e.stopPropagation();
  const r = ctRecords[Number(b.dataset.i)];
  if (!r) return;
  if (!confirm('删除这条归档？删除后不可恢复。')) return;
  const date = (r.time || '').slice(0, 10);
  try { await deleteContent({ date, id: r.id }, '已删除'); }
  catch (err) { toast(err.message, 'bad'); }
});

/* 展开详情才拉完整内容：列表只有摘要，单条归档可能十几 MB。
   toggle 事件不冒泡，所以用捕获阶段监听。 */
if ($('ctList')) $('ctList').addEventListener('toggle', async e => {
  const d = e.target;
  if (!d || d.tagName !== 'DETAILS' || !d.open) return;
  const box = d.querySelector('.ct-body');
  if (!box || box.dataset.loaded === '1') return;
  box.dataset.loaded = '1';
  box.innerHTML = '<div class="hint" style="color:var(--ink-3);padding:4px 0">加载中…</div>';
  try {
    const r = await api('content/item?date=' + encodeURIComponent(d.dataset.date || '') +
      '&id=' + encodeURIComponent(d.dataset.id || ''));
    box.innerHTML = ctBodyHtml(r.record || {});
  } catch (err) {
    box.dataset.loaded = '0'; // 失败允许重试
    box.innerHTML = '<div class="hint" style="color:var(--bad)">加载失败：' + esc(err.message) + '</div>';
  }
}, true);

/* 加载更早的一页：游标取当前列表最旧一条的时间（后端只回严格更早的记录）。 */
async function loadMoreContent() {
  if (!ctRecords.length) return;
  const key = $('ctKey') ? $('ctKey').value : '';
  const date = $('ctDate') ? $('ctDate').value : '';
  const oldest = ctRecords[0].time || '';
  if (!oldest) return;
  try {
    const rd = await api('content?limit=100' +
      (key ? '&key=' + encodeURIComponent(key) : '') +
      (date ? '&date=' + encodeURIComponent(date) : '') +
      '&before=' + encodeURIComponent(oldest));
    const older = (rd.records || []).filter(r => !ctRecords.some(x => x.id === r.id));
    ctRecords = older.concat(ctRecords);
    ctHasMore = !!rd.has_more && older.length > 0;
    renderContent();
    if (!older.length) toast('没有更早的记录了');
  } catch (err) {
    toast(err.message, 'bad');
  }
}

if ($('btnCtClearDay')) $('btnCtClearDay').onclick = async () => {
  const key = $('ctKey') ? $('ctKey').value : '';
  const date = $('ctDate') ? $('ctDate').value : '';
  if (!date) { toast('没有可清除的日期', 'bad'); return; }
  const who = key ? ('密钥 ' + key + ' 在 ') : '';
  const tip = key ? '' : '（整天，含所有密钥）';
  if (!confirm('删除 ' + who + date + tip + ' 的归档？删除后不可恢复。')) return;
  try { await deleteContent({ date, key }, '已清除'); }
  catch (err) { toast(err.message, 'bad'); }
};

if ($('btnCtPurge')) $('btnCtPurge').onclick = async () => {
  if (!confirm('清空全部对话归档？所有密钥、所有日期的记录都会被删除，不可恢复。')) return;
  const v = prompt('这一步不可恢复。确认请输入 DELETE：');
  if (v !== 'DELETE') { toast('已取消'); return; }
  try {
    const d = await api('content/delete', { method: 'POST', body: JSON.stringify({ all: true }) });
    toast('已清空' + (d.files ? '（' + d.files + ' 个文件）' : ''));
    await loadContent();
  } catch (err) { toast(err.message, 'bad'); }
};
