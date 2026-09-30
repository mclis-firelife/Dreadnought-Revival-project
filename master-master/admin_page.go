package main

import "net/http"

// serveAdminPage serves the operator dashboard shell: headings, empty
// tables, and the script. Deliberately public (no auth): it carries zero
// data, and serving it openly is what keeps the browser from caching the
// admin password — every open and every refresh starts at the login form,
// the password lives only in JS memory, and all data loads through the
// Basic-authed JSON API.
func serveAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(masterAdminPageHTML))
}

const masterAdminPageHTML = `<!doctype html>
<meta charset="utf-8">
<title>Master-Master — clusters</title>
<style>
  :root { color-scheme: dark; }
  body { font: 14px/1.5 system-ui, sans-serif; color:#d8e2ea; background:#0a1119;
    margin:0; padding:22px 26px; }
  h1 { font-size:18px; color:#6fd3ff; letter-spacing:.06em; margin:0 0 4px; }
  p.note { color:#7f93a5; font-size:12.5px; margin:0 0 16px; }
  .row { display:flex; gap:8px; align-items:center; margin-bottom:14px; }
  button { padding:7px 12px; font:inherit; font-size:13px; cursor:pointer; color:#e6eef5;
    background:#12293c; border:1px solid #2f5a78; border-radius:6px; }
  button.danger { border-color:#5a2e2e; color:#ff9b8f; background:transparent; }
  button:disabled { opacity:.5; }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th, td { text-align:left; padding:7px 9px; border-bottom:1px solid #21384d; vertical-align:top; }
  th { color:#7f93a5; text-transform:uppercase; font-size:11px; letter-spacing:.06em; }
  code { font:12px Consolas, monospace; color:#9fb4c4; word-break:break-all; }
  .badge { border-radius:99px; padding:2px 9px; font-size:11.5px; white-space:nowrap; }
  .ok { color:#8fe0a8; border:1px solid #2e5a3f; }
  .bad { color:#ff9b8f; border:1px solid #5a2e2e; }
  .warn { color:#ffd98f; border:1px solid #5a4a2e; }
  .msg { min-height:18px; font-size:13px; margin:10px 0; }
  .msg.bad { color:#ff9b8f; } .msg.good { color:#8fe0a8; }
  dialog { background:#0c141d; color:#d8e2ea; border:1px solid #2f5a78; border-radius:8px; padding:20px; width:min(480px,92vw); }
  dialog input { width:100%; padding:9px 11px; font:inherit; color:#e6eef5; background:#0c1621;
    border:1px solid #24405a; border-radius:6px; box-sizing:border-box; }
  dialog input { width:100%; padding:9px 11px; font:inherit; color:#e6eef5; background:#0c1621;
    border:1px solid #24405a; border-radius:6px; box-sizing:border-box; }
  dialog menu { display:flex; justify-content:flex-end; gap:8px; padding:0; margin:14px 0 0; }
  select { padding:7px 10px; font:inherit; font-size:13px; color:#e6eef5; background:#0c1621;
    border:1px solid #24405a; border-radius:6px; max-width:260px; }
  label.dim { color:#7f93a5; font-size:12.5px; }
  #login { position:fixed; inset:0; background:rgba(6,10,15,.96); display:flex; z-index:10; }
  #login[hidden] { display:none; }
  #login .box { margin:auto; width:min(360px,92vw); padding:28px; background:#0c141d;
    border:1px solid #2f5a78; border-radius:10px; }
  #login h2 { margin:0 0 6px; font-size:17px; color:#9fe4ff; }
  #login input { width:100%; padding:10px 12px; font:inherit; color:#e6eef5; background:#0c1621;
    border:1px solid #24405a; border-radius:6px; box-sizing:border-box; margin:10px 0 4px; }
</style>
<h1>Cluster directory</h1>
<p class="note">Every registered cluster, including stale and blocked ones. Browsers only see fresh online rows.</p>
<div class="row"><button onclick="load()">↻ Reload</button><span id="count"></span>
  <span style="flex:1"></span><button onclick="logout()">Sign out</button></div>
<div id="login"><div class="box">
  <h2>Operator sign-in</h2>
  <p class="note">Directory admin password. Kept in this page's memory only —
    refresh or a new tab signs out.</p>
  <form onsubmit="event.preventDefault();doLogin()">
    <input id="pw" type="password" autocomplete="current-password" placeholder="Admin password">
    <button style="width:100%;margin-top:10px" type="submit">Sign in</button>
  </form>
  <div class="msg" id="loginmsg"></div>
</div></div>
<div class="msg" id="msg"></div>
<h2>Manual sync</h2>
<p class="note">Trigger a push/pull cycle on the clusters right now instead of waiting for the
  next interval. <b>Roll out</b> copies the main cluster everywhere (forced): the main cluster
  pushes first, then every other cluster applies everything — even older snapshots — and pushes.
  Accounts that exist only elsewhere are kept, never deleted. Clusters without an agent URL are
  skipped (sync still runs on its interval once they push). Blocked clusters sync like everyone
  else; block only hides them from browsers.</p>
<div class="row">
  <button onclick="syncNow()">Sync all now</button>
  <label class="dim" for="mainSel">Main cluster:</label>
  <select id="mainSel" onchange="saveMain()"></select>
  <button onclick="rollout()">Roll out main → all</button>
  <button onclick="previewRollout()">Preview rollout</button>
</div>
<div class="msg" id="syncmsg"></div>
<table>
  <thead><tr><th>Cluster</th><th>Pushed</th><th>Applied</th><th>Status</th><th>Detail</th></tr></thead>
  <tbody id="syncrows"><tr><td colspan="5">No manual sync yet.</td></tr></tbody>
</table>
<h2>Clusters</h2>
<div class="row"><input id="motdAll" placeholder="Message of the day for ALL clusters…" maxlength="500" style="flex:1;max-width:420px;padding:7px 10px;font:inherit;font-size:13px;color:#e6eef5;background:#0c1621;border:1px solid #24405a;border-radius:6px;box-sizing:border-box"><button onclick="motdAll()">Broadcast MOTD</button></div>
<table>
  <thead><tr><th>Name</th><th>Status</th><th>Players</th><th>Servers</th><th>Address</th><th>Contact</th><th>Sync secret</th><th>Last sync</th><th>Accounts</th><th>Actions</th></tr></thead>
  <tbody id="rows"></tbody>
</table>
<h2>Sync status</h2>
<p class="note">Last push/pull per cluster from the audit log, mirrored account counts, and a live agent reachability check. Blocked clusters sync like everyone else; block only hides them from browsers.</p>
<div class="msg" id="syncmsg2"></div>
<table>
  <thead><tr><th>Cluster</th><th>Secret</th><th>Agent</th><th>Last push</th><th>Last pull</th><th>Mirrored</th><th>Agent check</th></tr></thead>
  <tbody id="syncrows2"></tbody>
</table>
<h2>Live matches</h2>
<p class="note">Accounts reported mid-match right now, across all clusters (fresh reports only).</p>
<table>
  <thead><tr><th>Player</th><th>Cluster</th><th>Since</th></tr></thead>
  <tbody id="presence"></tbody>
</table>
<h2>Heartbeat history</h2>
<p class="note">Online/offline transitions per cluster (30 days kept). Offline clusters show since when.</p>
<div class="row"><select id="histSel" onchange="loadHistory()"></select>
  <button onclick="window.location='/admin/api/backup'">⤓ Directory backup</button></div>
<div class="msg" id="histmsg"></div>
<table>
  <thead><tr><th>Time</th><th>Cluster</th><th>Event</th></tr></thead>
  <tbody id="histrows"></tbody>
</table>
<h2>Account sources</h2>
<p class="note">Which cluster contributed how many mirrored accounts.</p>
<div id="sources"></div>
<h2>Statistics</h2>
<p class="note">Uptime per cluster, sync traffic, account growth, sync errors and duplicate accounts.</p>
<div class="row"><label class="dim" for="statRange">Range:</label>
  <select id="statRange" onchange="loadStats()">
    <option value="24h">24 hours</option><option value="7d" selected>7 days</option>
    <option value="14d">14 days</option><option value="30d">30 days</option>
  </select></div>
<h3>Uptime</h3>
<div id="uptime"></div>
<h3>Sync volume per day</h3>
<table>
  <thead><tr><th>Day</th><th>Push calls</th><th>Push users</th><th>Pull calls</th><th>Pull users</th><th>Denied</th></tr></thead>
  <tbody id="volumerows"></tbody>
</table>
<h3>New accounts per day</h3>
<table>
  <thead><tr><th>Day</th><th>Total</th><th>Top source</th></tr></thead>
  <tbody id="growthrows"></tbody>
</table>
<h3>Sync errors</h3>
<table>
  <thead><tr><th>Time</th><th>Cluster</th><th>Direction</th><th>Endpoint</th><th>Status</th><th>Detail</th></tr></thead>
  <tbody id="errorrows"></tbody>
</table>
<h3>Duplicate accounts</h3>
<p class="note">Same email or callsign under different ids (double registrations from the sync window). Reconcile by hand; the agent keeps the local row.</p>
<table>
  <thead><tr><th>Field</th><th>Value</th><th>IDs</th><th>Names</th></tr></thead>
  <tbody id="duprows"></tbody>
</table>
<h2>Accounts (mirrored)</h2>
<div class="row"><input id="q" placeholder="Search username, email, id…" style="flex:1;max-width:320px"><button onclick="loadUsers()">Search</button><span id="ucount"></span></div>
<table>
  <thead><tr><th>Username</th><th>Email</th><th>Credits</th><th>Rank</th><th>Ships</th><th>Banned</th><th>Source</th><th>Synced</th></tr></thead>
  <tbody id="users"></tbody>
</table>
<h2>Sync log</h2>
<table>
  <thead><tr><th>Time</th><th>Cluster</th><th>Dir</th><th>Endpoint</th><th>Users</th><th>Status</th><th>Detail</th></tr></thead>
  <tbody id="synclog"></tbody>
</table>
<dialog id="motdDlg">
  <h3 style="margin-top:0">Message of the day</h3>
  <input id="motdText" maxlength="500" placeholder="Welcome…">
  <div class="row" style="margin-top:8px"><label class="dim" for="motdMins">Expires after (minutes, empty = permanent):</label>
    <input id="motdMins" placeholder="e.g. 120" style="width:120px"></div>
  <menu><button id="motdCancel">Cancel</button><button id="motdSave">Save</button></menu>
</dialog>
<dialog id="secretDlg">
  <h3 style="margin-top:0">Sync secret — copy it now</h3>
  <p class="note">Shown <b>once</b>: mail it to the cluster owner yourself. It is stored hashed only and can never be displayed again (revoke + generate rotates).</p>
  <div class="fp" id="secretText" style="font:13px Consolas,monospace;word-break:break-all;background:#0c1621;border:1px solid #24405a;border-radius:6px;padding:8px 10px;"></div>
  <p class="note" id="secretSent"></p>
  <menu><button id="secretClose">Done</button></menu>
</dialog>
<script>
  const $ = id => document.getElementById(id);
  const esc = v => String(v == null ? "" : v);
  let motdId = null;
  let lastClusters = [];
  // Operator password, page memory only: never stored (no cookie, no
  // localStorage), so every refresh and every new tab starts logged out.
  // The browser never sees a Basic challenge for the shell, which is what
  // would otherwise cache the password behind our backs.
  let authHdr = null;
  function say(t, bad) { $('msg').textContent = t; $('msg').className = 'msg ' + (bad ? 'bad' : 'good'); }
  function sayLogin(t) { $('loginmsg').textContent = t; $('loginmsg').className = 'msg ' + (t ? 'bad' : ''); }
  async function doLogin() {
    const pw = $('pw').value;
    if (!pw) { sayLogin('Enter the admin password.'); return; }
    sayLogin('');
    const hdr = 'Basic ' + btoa(':' + pw);
    try {
      const r = await fetch('/admin/api/sync-settings', { headers: { 'Authorization': hdr } });
      if (!r.ok) throw new Error('Wrong password.');
      authHdr = hdr;
      $('pw').value = '';
      $('login').hidden = true;
      say('');
      load();
    } catch (e) { sayLogin(String(e && e.message || e)); }
  }
  function logout() {
    authHdr = null;
    $('pw').value = '';
    $('login').hidden = false;
    sayLogin('');
    setTimeout(() => $('pw').focus(), 0);
  }
  async function call(method, path, body) {
    if (!authHdr) { logout(); throw new Error('Signed out — sign in again.'); }
    const r = await fetch(path, { method, headers: { 'Content-Type': 'application/json', 'Authorization': authHdr },
      body: body === undefined ? undefined : JSON.stringify(body) });
    if (r.status === 401) { logout(); throw new Error('Signed out — sign in again.'); }
    const doc = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(doc.error || ('HTTP ' + r.status));
    return doc;
  }
  async function load() {
    try {
      const d = await call('GET', '/admin/api/clusters');
      $('count').textContent = d.count + ' cluster(s)';
      const tb = $('rows');
      tb.textContent = '';
      // Version guard: the most common version among online clusters is the
      // fleet standard; anything else gets an "odd version" badge.
      const vcount = {};
      for (const c of (d.clusters || [])) {
        if (c.status === 'online' && c.version) vcount[c.version] = (vcount[c.version] || 0) + 1;
      }
      let fleetVersion = '';
      for (const [v, n] of Object.entries(vcount)) {
        if (!fleetVersion || n > vcount[fleetVersion]) fleetVersion = v;
      }
      for (const c of (d.clusters || [])) {
        const tr = document.createElement('tr');
        let badge;
        if (c.blocked) {
          badge = '<span class="badge bad">blocked</span>';
          const bits = [];
          if (c.blocked_reason) bits.push(c.blocked_reason);
          if (c.blocked_until) {
            const until = new Date(c.blocked_until);
            bits.push(until > new Date() ? 'until ' + until.toLocaleString() : 'expired, clears on heartbeat');
          } else bits.push('indefinite');
          badge += '<br><small style="color:#7f93a5">' + bits.join(' · ') + '</small>';
        } else badge = c.status === 'online' ? '<span class="badge ok">online</span>' : '<span class="badge warn">stale</span>';
        const secret = c.has_secret ? '<span class="badge ok">set</span>' : '<span class="badge warn">none</span>';
        tr.innerHTML = '<td><b></b><br><code></code></td><td>' + badge + '</td><td></td><td></td><td><code></code></td><td></td><td></td><td></td><td></td><td></td>';
        const t = tr.children;
        t[0].querySelector('b').textContent = c.name;
        t[0].querySelector('code').textContent = c.id.slice(0, 8) + ' · ' + (c.version || '') +
          ((c.version && fleetVersion && c.version !== fleetVersion) ? ' · ODD VERSION' : '');
        if (c.note) {
          const n = document.createElement('div');
          n.style.cssText = 'color:#ffd98f;font-size:12px;margin-top:2px';
          n.textContent = '✎ ' + c.note;
          t[0].append(n);
        }
        t[2].textContent = c.players;
        t[3].textContent = c.servers;
        t[4].querySelector('code').textContent = c.web_url + ' / ' + c.battle_ip;
        t[5].textContent = c.contact_email || '–';
        t[6].innerHTML = secret;
        t[7].textContent = c.last_sync || '–';
        t[8].textContent = (c.mirrored_users != null ? c.mirrored_users : '–') + ' account(s)';
        const act = t[9];
        const mk = (label, danger, fn) => {
          const b = document.createElement('button');
          b.textContent = label;
          if (danger) b.className = 'danger';
          b.onclick = fn;
          act.append(b, document.createTextNode(' '));
        };
        mk('MOTD', false, () => { motdId = c.id; $('motdText').value = c.motd || ''; $('motdDlg').showModal(); });
        mk('Note', false, () => {
          const note = prompt('Operator note for "' + c.name + '"? (owner, maintenance window, quirks — empty clears)', c.note || '');
          if (note === null) return;
          call('POST', '/admin/api/clusters/' + c.id + '/note', { note })
            .then(() => { say('Note saved.'); load(); })
            .catch((e) => say(String(e && e.message || e), true));
        });
        mk('Secret…', false, async () => {
          const how = prompt('Generate a fresh secret (shown once, mail it yourself), or send one straight to the cluster?\nType: generate / send / revoke', 'generate');
          if (!how) return;
          try {
            const r = await call('POST', '/admin/api/clusters/' + c.id + '/secret', { action: how.trim().toLowerCase() });
            if (r.secret) {
              $('secretText').textContent = r.secret;
              $('secretSent').textContent = r.sent ? 'Pushed to the cluster agent over HTTPS.' : (r.send_error ? 'Push failed: ' + r.send_error + ' — mail it instead.' : 'Mail it to ' + (c.contact_email || 'the owner') + ' yourself.');
              $('secretDlg').showModal();
            } else say(r.status || 'Done.');
            load();
          } catch (e) { say(String(e && e.message || e), true); }
        });
        if (c.blocked) mk('Unblock', false, async () => {
          try { await call('POST', '/admin/api/clusters/' + c.id + '/unblock'); say('Unblocked; returns on next heartbeat.'); load(); }
          catch (e) { say(String(e && e.message || e), true); }
        });
        else mk('Block', true, async () => {
          const reason = prompt('Reason for blocking "' + c.name + '"? (shown on refusal, empty = none)', '');
          if (reason === null) return;
          const mins = prompt('Minutes until auto-unblock? (0 or empty = indefinite)', '0');
          if (mins === null) return;
          const minutes = parseInt(mins, 10) || 0;
          if (!confirm('Kick "' + c.name + '" out of the browser? It cannot re-list until unblocked' + (minutes ? ' or ' + minutes + ' min pass' : '') + '.')) return;
          try {
            await call('POST', '/admin/api/clusters/' + c.id + '/block', { reason, minutes });
            say('Blocked.');
            load();
          }
          catch (e) { say(String(e && e.message || e), true); }
        });
        mk('Delete', true, async () => {
          if (!confirm('Delete "' + c.name + '" entirely?')) return;
          try { await call('DELETE', '/admin/api/clusters/' + c.id); say('Deleted.'); load(); }
          catch (e) { say(String(e && e.message || e), true); }
        });
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="10">No clusters registered yet.</td></tr>';
      lastClusters = d.clusters || [];
    } catch (e) { say(String(e && e.message || e), true); }
    loadMain(lastClusters);
    const hs = $('histSel');
    const cur = hs.value;
    hs.textContent = '';
    const all = document.createElement('option');
    all.value = '';
    all.textContent = 'All clusters';
    hs.append(all);
    for (const c of lastClusters) {
      const o = document.createElement('option');
      o.value = c.id;
      o.textContent = c.name;
      hs.append(o);
    }
    if (cur) hs.value = cur;
    loadHistory();
    loadSources();
    loadStats();
    loadSyncStatus();
    loadPresence();
    loadUsers();
    loadSyncLog();
  }
  function saySync2(t, bad) { $('syncmsg2').textContent = t; $('syncmsg2').className = 'msg ' + (bad ? 'bad' : 'good'); }
  async function loadSyncStatus() {
    try {
      const d = await call('GET', '/admin/api/syncstatus');
      const tb = $('syncrows2');
      tb.textContent = '';
      for (const c of (d.clusters || [])) {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td><b></b></td><td></td><td><code></code></td><td></td><td></td><td></td><td></td>';
        const t = tr.children;
        t[0].querySelector('b').textContent = c.name;
        t[1].innerHTML = c.has_secret ? '<span class="badge ok">set</span>' : '<span class="badge warn">none</span>';
        if (c.has_secret && c.secret_set_at) {
          const age = Math.max(0, Math.round((Date.now() - new Date(c.secret_set_at)) / 86400000));
          const s = document.createElement('div');
          s.style.cssText = 'font-size:12px;margin-top:2px;color:' + (age > 90 ? '#ff9b8f' : '#7f93a5');
          s.textContent = age === 0 ? 'rotated today' : ('rotated ' + age + 'd ago') + (age > 90 ? ' · ROTATE' : '');
          t[1].append(s);
        }
        t[2].querySelector('code').textContent = c.agent_url || '–';
        t[3].textContent = c.last_push ? c.last_push + ' (' + (c.last_push_status || '?') + ')' : '–';
        t[4].textContent = c.last_pull ? c.last_pull + ' (' + (c.last_pull_status || '?') + ')' : '–';
        t[5].textContent = c.mirrored_users;
        const cell = t[6];
        const rotate = document.createElement('button');
        rotate.textContent = 'Rotate secret';
        rotate.title = 'Generate fresh + push to the agent (one click rotation)';
        rotate.onclick = async () => {
          if (!confirm('Rotate the sync secret for "' + c.name + '"? The old one dies immediately.')) return;
          rotate.disabled = true;
          try {
            const r = await call('POST', '/admin/api/clusters/' + c.id + '/secret', { action: 'send' });
            if (r.secret) {
              $('secretText').textContent = r.secret;
              $('secretSent').textContent = r.sent ? 'Pushed to the cluster agent over HTTPS.' : (r.send_error ? 'Push failed: ' + r.send_error + ' — mail it instead.' : 'Mail it yourself.');
              $('secretDlg').showModal();
            } else saySync2(r.status || 'Done.', false);
            loadSyncStatus();
          } catch (e) { saySync2(String(e && e.message || e), true); }
          rotate.disabled = false;
        };
        cell.append(rotate, document.createTextNode(' '));
        if (c.agent_url) {
          const b = document.createElement('button');
          b.textContent = 'Test agent';
          b.onclick = async () => {
            b.disabled = true;
            try {
              const r = await call('POST', '/admin/api/clusters/' + c.id + '/ping');
              saySync2(c.name + ': ' + (r.ok ? 'reachable (' + r.latency_ms + ' ms).' : 'unreachable: ' + r.error), !r.ok);
            } catch (e) { saySync2(String(e && e.message || e), true); }
            b.disabled = false;
          };
          cell.append(b);
        } else cell.textContent = '–';
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="7">No clusters registered yet.</td></tr>';
    } catch (e) { saySync2(String(e && e.message || e), true); }
  }
  async function loadPresence() {
    try {
      const d = await call('GET', '/admin/api/presence');
      const tb = $('presence');
      tb.textContent = '';
      for (const p of (d.players || [])) {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td><b></b> <code></code></td><td></td><td></td>';
        const t = tr.children;
        t[0].querySelector('b').textContent = p.username || '(unknown)';
        t[0].querySelector('code').textContent = (p.user_id || '').slice(0, 8);
        t[1].textContent = p.cluster || p.cluster_id;
        t[2].textContent = p.since || '–';
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="3">Nobody mid-match right now.</td></tr>';
    } catch (e) { /* presence is live-only; the rest matters more */ }
  }
  async function motdAll() {
    const text = $('motdAll').value;
    if (!text.trim()) { say('Enter a message first.', true); return; }
    const mins = prompt('Expire after how many minutes? (empty = permanent)', '');
    if (mins === null) return;
    if (!confirm('Send this MOTD to EVERY cluster?')) return;
    try {
      const r = await call('POST', '/admin/api/motd-all', { motd: text, minutes: parseInt(mins, 10) || 0 });
      say('MOTD sent to ' + r.updated + ' cluster(s).');
      load();
    } catch (e) { say(String(e && e.message || e), true); }
  }
  async function previewRollout() {
    try {
      const d = await call('GET', '/admin/api/rollout-preview');
      const rows = (d.sources || []).map(s => s.cluster + ': ' + s.users + ' account(s)').join(' · ') || 'none';
      saySync('Preview — main: ' + d.main.name + ', accounts total ' + d.users_total +
        ' (' + d.users_main + ' from main, ' + d.users_only_elsewhere + ' only elsewhere, kept), ' +
        d.snapshots_flipping + ' snapshot(s) would flip, ' + d.bans + ' ban(s). Sources: ' + rows);
    } catch (e) { saySync(String(e && e.message || e), true); }
  }
  function sayHist(t, bad) { $('histmsg').textContent = t; $('histmsg').className = 'msg ' + (bad ? 'bad' : 'good'); }
  async function loadHistory() {
    try {
      const sel = $('histSel').value || '';
      const d = await call('GET', '/admin/api/heartbeat-history?limit=100' + (sel ? '&cluster=' + encodeURIComponent(sel) : ''));
      const tb = $('histrows');
      tb.textContent = '';
      for (const e of (d.events || [])) {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td></td><td></td><td></td>';
        const t = tr.children;
        t[0].textContent = e.time || '–';
        t[1].textContent = e.cluster || e.cluster_id;
        t[2].innerHTML = e.event === 'online' ? '<span class="badge ok">online</span>' : '<span class="badge bad">offline</span>';
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="3">No transitions recorded yet.</td></tr>';
    } catch (e) { sayHist(String(e && e.message || e), true); }
  }
  async function loadSources() {
    try {
      const d = await call('GET', '/admin/api/sources');
      const box = $('sources');
      box.textContent = '';
      const total = d.total || 0;
      for (const s of (d.sources || [])) {
        const pct = total ? Math.round((100 * s.users) / total) : 0;
        const row = document.createElement('div');
        row.style.cssText = 'display:flex;align-items:center;gap:10px;margin:4px 0;font-size:13px';
        const bar = document.createElement('div');
        bar.style.cssText = 'flex:1;height:14px;background:#0c1621;border:1px solid #24405a;border-radius:4px;position:relative';
        const fill = document.createElement('div');
        fill.style.cssText = 'width:' + pct + '%;height:100%;background:#3ba7d8;border-radius:3px';
        bar.append(fill);
        const label = document.createElement('span');
        label.style.minWidth = '220px';
        label.textContent = s.cluster + ': ' + s.users + ' (' + pct + '%)';
        row.append(label, bar);
        box.append(row);
      }
      if (!box.children.length) box.innerHTML = '<p class="note">No mirrored accounts yet.</p>';
    } catch (e) { /* chart is garnish */ }
  }
  function saySync(t, bad) { $('syncmsg').textContent = t; $('syncmsg').className = 'msg ' + (bad ? 'bad' : 'good'); }
  function renderSyncRows(main, results) {
    const tb = $('syncrows');
    tb.textContent = '';
    const row = (name, pushed, applied, ok, detail, star) => {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td></td><td></td><td></td><td></td><td></td>';
      const t = tr.children;
      t[0].textContent = (star ? '★ ' : '') + name;
      t[1].textContent = pushed;
      t[2].textContent = applied;
      t[3].innerHTML = ok ? '<span class="badge ok">ok</span>' : '<span class="badge bad">failed</span>';
      t[4].textContent = detail || '–';
      tb.append(tr);
    };
    if (main) row(main.name + ' (main)', main.pushed, main.applied, main.ok, main.detail, true);
    for (const r of (results || [])) row(r.name, r.pushed, r.applied, r.ok, r.detail, false);
    if (!tb.children.length) tb.innerHTML = '<tr><td colspan="5">No cluster has an agent URL yet.</td></tr>';
  }
  async function loadMain(clusters) {
    const s = $('mainSel');
    s.textContent = '';
    const none = document.createElement('option');
    none.value = '';
    none.textContent = '— pick the main cluster —';
    s.append(none);
    for (const c of (clusters || [])) {
      const o = document.createElement('option');
      o.value = c.id;
      o.textContent = c.name + (c.agent_url ? '' : ' (no agent URL)');
      s.append(o);
    }
    try {
      const d = await call('GET', '/admin/api/sync-settings');
      s.value = d.main_cluster_id || '';
    } catch (e) { saySync(String(e && e.message || e), true); }
  }
  async function saveMain() {
    try {
      await call('POST', '/admin/api/sync-settings', { main_cluster_id: $('mainSel').value });
      saySync('Main cluster saved.');
    } catch (e) { saySync(String(e && e.message || e), true); }
  }
  async function syncNow() {
    saySync('Triggering every cluster…');
    try {
      const d = await call('POST', '/admin/api/sync-now');
      renderSyncRows(null, d.results);
      const failed = (d.results || []).filter(r => !r.ok).length;
      saySync(failed ? (d.count - failed) + ' of ' + d.count + ' clusters synced, ' + failed + ' failed.'
        : 'All ' + d.count + ' clusters synced.', failed > 0);
      loadSyncLog();
    } catch (e) { saySync(String(e && e.message || e), true); }
  }
  async function rollout() {
    const main = $('mainSel').value;
    if (!main) { saySync('Pick the main cluster first.', true); return; }
    if (!confirm('Copy the main cluster state to every other cluster (forced)? Accounts that exist only elsewhere are kept.')) return;
    saySync('Main cluster pushes, then everyone else applies…');
    try {
      const d = await call('POST', '/admin/api/rollout');
      renderSyncRows(d.main, d.results);
      const failed = (d.results || []).filter(r => !r.ok).length;
      saySync(failed ? 'Rolled out with ' + failed + ' failure(s).' : 'Rolled out to ' + d.count + ' cluster(s).', failed > 0);
      loadSyncLog();
    } catch (e) { saySync(String(e && e.message || e), true); }
  }
  async function loadUsers() {
    try {
      const d = await call('GET', '/admin/api/syncusers?q=' + encodeURIComponent($('q').value) + '&limit=50');
      $('ucount').textContent = (d.count || 0) + ' account(s)';
      const tb = $('users');
      tb.textContent = '';
      for (const u of (d.users || [])) {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td><b></b></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td>';
        const t = tr.children;
        t[0].querySelector('b').textContent = u.username;
        t[1].textContent = u.email || '–';
        t[2].textContent = u.credits;
        t[3].textContent = u.rank;
        t[4].textContent = u.ships;
        t[5].innerHTML = u.banned ? '<span class="badge bad">banned</span>' : '–';
        t[6].textContent = u.source_cluster || '–';
        t[7].textContent = u.synced_at || '–';
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="8">No mirrored accounts yet — they arrive with the first sync push.</td></tr>';
    } catch (e) { say(String(e && e.message || e), true); }
  }
  async function loadSyncLog() {
    try {
      const d = await call('GET', '/admin/api/synclog?limit=100');
      const tb = $('synclog');
      tb.textContent = '';
    for (const e of (d.entries || [])) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td></td><td></td><td></td><td><code></code></td><td></td><td></td><td></td>';
      const t = tr.children;
        t[0].textContent = e.time;
        t[1].textContent = e.cluster || e.cluster_id.slice(0, 8);
        t[2].textContent = e.direction;
        t[3].querySelector('code').textContent = e.endpoint;
        t[4].textContent = e.users;
        t[5].textContent = e.status;
        t[6].textContent = e.detail || '–';
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="7">No sync traffic yet.</td></tr>';
    } catch (e) { say(String(e && e.message || e), true); }
  }
  $('motdCancel').onclick = () => $('motdDlg').close();
  $('motdSave').onclick = async () => {
    const mins = parseInt($('motdMins').value, 10) || 0;
    try {
      await call('POST', '/admin/api/clusters/' + motdId + '/motd', { motd: $('motdText').value, minutes: mins });
      $('motdDlg').close();
      $('motdMins').value = '';
      say('MOTD saved.');
      load();
    } catch (e) { say(String(e && e.message || e), true); }
  };
  async function loadStats() {
    const range = ($('statRange') && $('statRange').value) || '7d';
    try {
      const u = await call('GET', '/admin/api/uptime?range=' + encodeURIComponent(range));
      const box = $('uptime');
      box.textContent = '';
      for (const c of (u.clusters || [])) {
        const row = document.createElement('div');
        row.style.cssText = 'display:flex;align-items:center;gap:10px;margin:4px 0;font-size:13px';
        const bar = document.createElement('div');
        bar.style.cssText = 'flex:1;height:14px;background:#0c1621;border:1px solid #24405a;border-radius:4px;position:relative';
        const fill = document.createElement('div');
        const pct = Math.round(c.pct || 0);
        fill.style.cssText = 'width:' + pct + '%;height:100%;background:' + (pct >= 99 ? '#3ba7d8' : pct >= 90 ? '#ffd98f' : '#ff9b8f') + ';border-radius:3px';
        bar.append(fill);
        const label = document.createElement('span');
        label.style.minWidth = '260px';
        label.textContent = c.name + ': ' + pct + '% · ' + c.flaps + ' flap(s)' + (c.online_now ? '' : ' · OFFLINE');
        row.append(label, bar);
        box.append(row);
      }
      if (!box.children.length) box.innerHTML = '<p class="note">No clusters.</p>';
    } catch (e) { /* stats are garnish */ }
    try {
      const v = await call('GET', '/admin/api/sync-volume?days=14');
      const tb = $('volumerows');
      tb.textContent = '';
      for (const d of (v.days || []).slice(-14)) {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td></td><td></td><td></td><td></td><td></td><td></td>';
        const t = tr.children;
        t[0].textContent = d.day;
        t[1].textContent = d.push_calls; t[2].textContent = d.push_users;
        t[3].textContent = d.pull_calls; t[4].textContent = d.pull_users;
        t[5].textContent = d.denied || '–';
        tb.append(tr);
      }
    } catch (e) { /* garnish */ }
    try {
      const g = await call('GET', '/admin/api/growth?days=14');
      const tb = $('growthrows');
      tb.textContent = '';
      for (const d of (g.days || []).slice(-14)) {
        let top = '–', topN = 0;
        for (const [k, n] of Object.entries(d.sources || {})) {
          if (n > topN) { topN = n; top = k; }
        }
        const tr = document.createElement('tr');
        tr.innerHTML = '<td></td><td></td><td></td>';
        const t = tr.children;
        t[0].textContent = d.day;
        t[1].textContent = d.total || '–';
        t[2].textContent = d.total ? top + ' (' + topN + ')' : '–';
        tb.append(tr);
      }
    } catch (e) { /* garnish */ }
    try {
      const se = await call('GET', '/admin/api/sync-errors?limit=50');
      const tb = $('errorrows');
      tb.textContent = '';
      for (const e of (se.errors || [])) {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td></td><td></td><td></td><td></td><td></td><td></td>';
        const t = tr.children;
        t[0].textContent = e.time;
        t[1].textContent = e.cluster || e.cluster_id;
        t[2].textContent = e.direction;
        t[3].textContent = e.endpoint;
        t[4].innerHTML = '<span class="badge bad">' + e.status + '</span>';
        t[5].textContent = e.detail || '–';
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="6">No sync errors. Quiet is good.</td></tr>';
    } catch (e) { /* garnish */ }
    try {
      const dd = await call('GET', '/admin/api/duplicates');
      const tb = $('duprows');
      tb.textContent = '';
      for (const grp of (dd.groups || [])) {
        const tr = document.createElement('tr');
        tr.innerHTML = '<td></td><td></td><td><code></code></td><td></td>';
        const t = tr.children;
        t[0].textContent = grp.field;
        t[1].textContent = grp.value;
        t[2].querySelector('code').textContent = (grp.ids || []).map(id => id.slice(0, 8)).join(', ');
        t[3].textContent = (grp.names || []).join(', ');
        tb.append(tr);
      }
      if (!tb.children.length) tb.innerHTML = '<tr><td colspan="4">No duplicates. Clean.</td></tr>';
    } catch (e) { /* garnish */ }
  }
  // No auto-load: the login overlay is up, and loading would only 401.
  $('pw').focus();
</script>
`
