'use strict';
/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  strategy_domestic: ['pool', 'strategy', 'domestic'],
  strategy_international: ['pool', 'strategy', 'international'],
  domestic_account_order: ['pool', 'account_order', 'domestic'],
  international_account_order: ['pool', 'account_order', 'international'],
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  task_sweep_enabled: ['schedule', 'task_sweep_enabled'], task_sweep_minutes: ['schedule', 'task_sweep_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  upstream_client_version: ['upstream', 'client_version'],
  upstream_cli_version: ['upstream', 'cli_version'],
  upstream_intl_client_version: ['upstream', 'intl_client_version'],
  upstream_intl_cli_version: ['upstream', 'intl_cli_version'],
  vw_enabled: ['upstream', 'version_watch', 'enabled'],
  vw_interval: ['upstream', 'version_watch', 'interval'],
  vw_cn_url: ['upstream', 'version_watch', 'cn_url'],
  vw_cn_regex: ['upstream', 'version_watch', 'cn_client_regex'],
  vw_global_url: ['upstream', 'version_watch', 'global_url'],
  vw_global_regex: ['upstream', 'version_watch', 'global_client_regex'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
  content_archive_enabled: ['logging', 'content_archive', 'enabled'],
  content_archive_keep_days: ['logging', 'content_archive', 'keep_days'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file',
  'upstream_client_version', 'upstream_cli_version',
  'upstream_intl_client_version', 'upstream_intl_cli_version',
  'vw_cn_url', 'vw_cn_regex', 'vw_global_url', 'vw_global_regex']);

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? (name.startsWith('strategy_') ? 'weighted_random' : '') : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      if (name.endsWith('_account_order')) v = raw.split(/[,，\s]+/).filter(Boolean);
      else if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
if ($('btnCfgReload')) $('btnCfgReload').onclick = loadConfig;

/* 配置分页：5 组字段一次只显示一组，避免一屏几百个输入框。
   当前页记在 localStorage，下次进来还停在同一页。 */
const CFG_TAB_KEY = 'wb2api.cfgtab';
function showCfgTab(name) {
  const tabs = $('cfgTabs'), form = $('cfgForm');
  if (!tabs || !form) return;
  const known = [...tabs.querySelectorAll('button[data-cfg]')].map(b => b.dataset.cfg);
  if (!known.includes(name)) {
    try { name = localStorage.getItem(CFG_TAB_KEY) || ''; } catch (e) { name = ''; }
  }
  if (!known.includes(name)) name = known[0] || 'service';
  tabs.querySelectorAll('button[data-cfg]').forEach(b => b.classList.toggle('on', b.dataset.cfg === name));
  // 只切换内容块：页签按钮本身也带 data-cfg，选择器必须限定 .box
  form.querySelectorAll('.box[data-cfg]').forEach(el => el.classList.toggle('cfghide', el.dataset.cfg !== name));
  try { localStorage.setItem(CFG_TAB_KEY, name); } catch (e) {}
}
if ($('cfgTabs')) $('cfgTabs').addEventListener('click', e => {
  const b = e.target.closest('button[data-cfg]');
  if (b) showCfgTab(b.dataset.cfg);
});

/* ── 客户端版本 ─────────────────────────────────────────────────────
   当前生效值来自上游客户端的实时字段（配置改了立刻变），巡检状态来自最近一次
   「拉版本源 → 比对 → 写回」。立即检查会同步跑一次，成功后配置已落盘并热生效。 */
async function loadVersions() {
  const now = $('verNow'), st = $('vwStatus');
  if (!now && !st) return;
  try {
    const d = await api('versions');
    const e = d.effective || {};
    if (now) {
      const cn = e.cn || {}, gl = e.global || {};
      now.textContent = '国内 ' + (cn.client || '?') + ' / CLI ' + (cn.cli || '?') +
        '　·　国际 ' + (gl.client || '?') + ' / CLI ' + (gl.cli || '?');
    }
    if (st) {
      const w = d.watch || {};
      st.textContent = w.last_check
        ? '上次检查 ' + new Date(w.last_check).toLocaleString() +
          (w.last_error ? '　失败：' + w.last_error : (w.last_note ? '　' + w.last_note : ''))
        : '还没检查过';
    }
  } catch (err) {
    if (st) st.textContent = '读取失败：' + err.message;
  }
}
if ($('btnVerCheck')) $('btnVerCheck').onclick = async () => {
  const btn = $('btnVerCheck');
  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = '检查中…';
  try {
    const d = await api('versions/check', { method: 'POST' });
    if (d.ok) {
      const ch = (d.watch && d.watch.changed) || [];
      toast(ch.length ? ('已更新：' + ch.join('、')) : '已是最新版本');
    } else {
      toast('检查未完成：' + (d.error || '未知原因'), 'bad');
    }
    await loadVersions();
    await loadConfig(); // 版本号可能被自动写回，表单要跟着刷新
  } catch (err) {
    toast(err.message, 'bad');
  } finally {
    btn.disabled = false;
    btn.textContent = old;
  }
};$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};
