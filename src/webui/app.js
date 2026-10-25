(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var state = { settings: null, groups: [], current: '', group: '', options: [], delays: {} };
  async function api(path, opts) {
    opts = opts || {};
    var init = { method: opts.method || 'GET', headers: {}, cache: 'no-store' };
    if (opts.body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(opts.body); }
    var res = await fetch('/api/' + path, init);
    var text = await res.text();
    var data = null;
    try { data = text ? JSON.parse(text) : null; } catch (e) { data = { error: text }; }
    if (!res.ok) throw new Error((data && data.error) || ('HTTP ' + res.status));
    return data;
  }
  var tt = null;
  function toast(msg, err) { var el = $('toast'); el.textContent = msg; el.className = 'toast show' + (err ? ' err' : ''); clearTimeout(tt); tt = setTimeout(function () { el.className = 'toast'; }, 3200); }

  async function refresh() {
    try {
      var s = await api('status');
      var pill = $('pill');
      pill.className = 'pill ' + (s.running ? 'ok' : 'bad');
      pill.querySelector('span').textContent = s.running ? '运行中' : '已停止';
      $('s-core').textContent = s.version || '—';
      $('s-core-note').textContent = s.running ? ('PID 正常 · v' + (s.app_version || '')) : '内核未运行';
      $('s-port').textContent = s.mixed_port;
      $('s-mode').textContent = s.allow_lan ? '允许局域网' : '仅本机';
      $('s-update').textContent = s.last_update || '尚未更新';
      $('s-sub').textContent = s.subscription_set ? '已配置' : '未配置';
      state.groups = s.groups || [];
      state.group = s.current_group || '';
      state.options = s.options || [];
      state.current = s.current_node || '';
      renderNodes();
    } catch (e) { var p = $('pill'); p.className = 'pill bad'; p.querySelector('span').textContent = '连接失败'; }
  }

  function renderNodes() {
    var sel = $('node-select');
    if (!state.options || !state.options.length) { sel.innerHTML = '<option value="">暂无节点</option>'; return; }
    sel.innerHTML = state.options.map(function (n) {
      var d = state.delays[n];
      var label = n + (d && d.delay ? (' · ' + d.delay + 'ms') : (d && d.error ? ' · 超时' : ''));
      return '<option value="' + n.replace(/"/g, '&quot;') + '"' + (n === state.current ? ' selected' : '') + '>' + label + '</option>';
    }).join('');
    sel.dataset.group = state.group;
  }

  async function selectNode() {
    var sel = $('node-select');
    if (!sel.value || !sel.dataset.group) return;
    try { await api('nodes/select', { method: 'POST', body: { group: sel.dataset.group, name: sel.value } }); toast('已切换：' + sel.value); refresh(); }
    catch (e) { toast(e.message, true); }
  }

  async function testDelay() {
    if (!state.group) { toast('还没有可用节点', true); return; }
    var btn = $('btn-delay'); btn.disabled = true; btn.textContent = '测速中…';
    $('delay-note').textContent = '正在测试 ' + state.options.length + ' 个节点…';
    try {
      var d = await api('nodes/delay?group=' + encodeURIComponent(state.group));
      state.delays = {};
      var ok = 0;
      (d.results || []).forEach(function (r) { state.delays[r.name] = { delay: r.delay, error: r.error }; if (r.delay) ok++; });
      renderNodes();
      $('delay-note').textContent = '测速完成：' + ok + '/' + (d.results || []).length + ' 个节点可用（单位 ms，越小越快）';
    } catch (e) { $('delay-note').textContent = '测速失败：' + e.message; toast(e.message, true); }
    finally { btn.disabled = false; btn.textContent = '测速'; }
  }

  async function loadCore() {
    try {
      var d = await api('core');
      $('core-current').textContent = d.current || '—';
      $('core-latest').textContent = d.latest || '—';
      $('core-note').textContent = d.error ? ('获取最新版本失败：' + d.error) : '';
    } catch (e) { /* ignore */ }
  }

  async function updateCore() {
    if (!confirm('更新内核会重启内核并中断当前连接，继续？')) return;
    var btn = $('btn-core-update'); btn.disabled = true; btn.textContent = '更新中…';
    $('core-note').textContent = '正在通过代理从 GitHub 下载并校验…';
    try {
      var d = await api('core/update', { method: 'POST' });
      toast(d.updated ? ('内核已更新到 ' + d.latest) : ('已是最新 ' + d.latest));
      $('core-note').textContent = d.updated ? ('已更新：' + d.current + ' → ' + d.latest) : '已是最新版本。';
      loadCore();
    loadPAC(); refresh();
    } catch (e) { $('core-note').textContent = '更新失败：' + e.message; toast(e.message, true); }
    finally { btn.disabled = false; btn.textContent = '更新内核'; }
  }

  async function loadPAC() {
    try {
      var d = await api('pac');
      if ($('pac-url')) {
        var u = '';
        if (d.host && d.port) { u = 'http://' + d.host + ':' + d.port + '/pac'; }
        if (!u) { u = d.url || ''; if (u && u.charAt(0) === '/') { u = location.origin + u; } }
        $('pac-url').value = u;
      }
      if ($('pac-note')) {
        $('pac-note').textContent = d.error ? ('更新失败：' + d.error) :
          ((d.source === 'builtin' ? '内置版本 ' : '当前版本 ') + (d.version || '-') + ' · 规则 ' + (d.rules || 0) + ' 条');
      }
    } catch (e) { /* ignore */ }
  }

  async function updatePAC() {
    var btn = $('btn-pac-update'); btn.disabled = true; btn.textContent = '更新中…';
    if ($('pac-note')) $('pac-note').textContent = '正在拉取 gfwlist…';
    try {
      var d = await api('pac/update', { method: 'POST' });
      if ($('pac-url')) {
        var u2 = '';
        if (d.host && d.port) { u2 = 'http://' + d.host + ':' + d.port + '/pac'; }
        if (!u2) { u2 = d.url || ''; if (u2 && u2.charAt(0) === '/') { u2 = location.origin + u2; } }
        $('pac-url').value = u2;
      }
      if ($('pac-note')) $('pac-note').textContent = (d.source === 'builtin' ? '内置版本 ' : '当前版本 ') + (d.version || '-') + ' · 规则 ' + (d.rules || 0) + ' 条';
      toast('PAC 已更新');
    } catch (e) { if ($('pac-note')) $('pac-note').textContent = '更新失败：' + e.message; toast(e.message, true); }
    finally { btn.disabled = false; btn.textContent = '更新 PAC'; }
  }

  async function copyPAC() {
    var v = $('pac-url').value;
    if (!v) return;
    try { await navigator.clipboard.writeText(v); toast('PAC 地址已复制'); }
    catch (e) { $('pac-url').select(); document.execCommand('copy'); toast('已复制'); }
  }

  async function loadSettings() {
    try {
      var s = await api('settings');
      state.settings = s;
      $('sub-url').value = s.subscription_url || '';
      $('sub-hours').value = s.update_hours || 12;
      $('auto-update').checked = !!s.auto_update;
      $('mixed-port').value = s.mixed_port;
      $('mode').value = s.mode;
      $('log-level').value = s.log_level;
      $('allow-lan').checked = !!s.allow_lan;
      if ($('pac-host')) { $('pac-host').value = s.pac_host || ''; }
    } catch (e) { toast(e.message, true); }
  }

  async function saveSettings(update) {
    var s = JSON.parse(JSON.stringify(state.settings || {}));
    s.subscription_url = $('sub-url').value.trim();
    s.update_hours = parseInt($('sub-hours').value, 10) || 12;
    s.auto_update = $('auto-update').checked;
    s.mixed_port = parseInt($('mixed-port').value, 10) || 7891;
    s.mode = $('mode').value;
    s.log_level = $('log-level').value;
    s.allow_lan = $('allow-lan').checked;
    if ($('pac-host')) { s.pac_host = $('pac-host').value.trim(); }
    try {
      var out = await api('settings', { method: 'PUT', body: s });
      state.settings = out;
      toast('设置已保存');
      $('set-note').textContent = '已保存；修改端口/模式后需重启内核生效。';
      if (update) await updateSub();
      refresh();
    } catch (e) { toast(e.message, true); }
  }

  async function updateSub() {
    var btn = $('btn-update'); btn.disabled = true; btn.textContent = '更新中…';
    $('sub-note').textContent = '正在下载订阅并校验…';
    try {
      var d = await api('subscription/update', { method: 'POST' });
      $('sub-note').textContent = '更新成功，节点数约 ' + d.nodes + '。';
      toast('订阅已更新');
      refresh();
    } catch (e) { $('sub-note').textContent = '更新失败：' + e.message; toast(e.message, true); }
    finally { btn.disabled = false; btn.textContent = '立即更新'; }
  }

  async function restartCore() {
    var btn = $('btn-restart'); btn.disabled = true; btn.textContent = '重启中…';
    try { var d = await api('core/restart', { method: 'POST' }); toast('内核已重启：' + (d.version || '')); refresh(); }
    catch (e) { toast(e.message, true); }
    finally { btn.disabled = false; btn.textContent = '重启内核'; }
  }

  async function loadLogs() {
    try {
      var d = await api('logs');
      var lines = (d.lines || []).concat(d.core && d.core.length ? ['— 内核日志 —'].concat(d.core) : []);
      $('logs').textContent = lines.join('\n') || '（暂无日志）';
      $('logs').scrollTop = $('logs').scrollHeight;
    } catch (e) { /* ignore */ }
  }

  async function init() {
    $('node-select').addEventListener('change', selectNode);
    $('btn-save').addEventListener('click', function () { saveSettings(false); });
    $('btn-save-sub').addEventListener('click', function () { saveSettings(true); });
    $('btn-update').addEventListener('click', updateSub);
    $('btn-restart').addEventListener('click', restartCore);
    $('btn-logs').addEventListener('click', loadLogs);
    $('btn-delay').addEventListener('click', testDelay);
    $('btn-core-update').addEventListener('click', updateCore);
    $('btn-pac-update').addEventListener('click', updatePAC);
    $('btn-pac-copy').addEventListener('click', copyPAC);
    await loadSettings();
    await refresh();
    await loadLogs();
    loadCore();
    loadPAC();
    setInterval(refresh, 10000);
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init); else init();
})();
