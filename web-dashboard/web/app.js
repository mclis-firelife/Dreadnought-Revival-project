/* Dreadnought Ops dashboard — vanilla JS, no dependencies. */
"use strict";

const $ = (id) => document.getElementById(id);
// tbodyFor: $() is getElementById and does not understand CSS selectors like
// "table tbody" (returns null -> "Cannot set properties of null").
const tbodyFor = (tableId) => {
  const t = $(tableId);
  return t ? t.querySelector("tbody") : null;
};
const state = {
  history: [], // {t, queue, matches, instances}
  lastUp: null,
  currentTab: "overview",
  logTimer: null,
  range: "all",
  seriesTimer: null,
};

async function api(path, opts = {}) {
  const res = await fetch(path, { credentials: "same-origin", ...opts });
  if (res.status === 401) {
    showLogin();
    throw new Error("not logged in");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
  return data;
}

function toast(msg, kind = "") {
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.textContent = msg;
  $("toasts").appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

/* ---------- login ---------- */
function showLogin() { $("login-overlay").classList.remove("hidden"); }
function hideLogin() { $("login-overlay").classList.add("hidden"); $("login-key").value = ""; }

async function doLogin() {
  const key = $("login-key").value;
  $("login-error").classList.add("hidden");
  try {
    await api("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ admin_key: key }),
    });
    hideLogin();
    toast("Signed in.", "ok");
    refreshAll();
  } catch (e) {
    $("login-error").textContent = "Sign-in failed: " + e.message;
    $("login-error").classList.remove("hidden");
  }
}

/* ---------- tabs ---------- */
function switchTab(name) {
  state.currentTab = name;
  document.querySelectorAll("#tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === name));
  document.querySelectorAll(".tab").forEach((s) => s.classList.toggle("active", s.id === "tab-" + name));
  if (name === "players") { loadPlayers(); loadBans(); loadSessions(); loadSleepers(); loadWealth(); }
  if (name === "queue") loadQueue();
  if (name === "matches") { loadInstances(); loadResults(); loadMatchesList(); loadHistory(); loadHeatmap(); loadShips(); loadModeStats(); }
  if (name === "market") loadCatalog();
  if (name === "audit") loadAudit();
  if (name === "online") loadOnline();
  if (name === "news") loadTiles();
  if (name === "backups") loadBackups();
  if (name === "servers") loadServers();
  if (name === "chat") loadChat();
  if (name === "logs") initLogs();
  if (name === "metrics") loadMetrics();
  if (name === "config") loadConfig();
  if (name === "reports") loadReports();
  if (name === "setup") { loadSetup(); loadSecrets(); }
  if (name === "overview") { loadStatus(); loadSeries(); loadTopKillers(); loadEconomyAndSessions(); }
}

/* ---------- overview ---------- */
function esc(v) {
  return String(v == null ? "" : v).replace(/[&<>"]/g, (c) => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));
}

async function loadStatus() {
  let data;
  try {
    data = await api("/api/status");
  } catch (e) { $("live-dot").classList.add("down"); return; }
  $("live-dot").classList.remove("down");
  $("hero-up").textContent = data.up + " / " + data.total;
  $("hero-up-sub").textContent = data.up === data.total ? "all healthy" : "DEGRADED — details below";
  $("hero-queue").textContent = data.queued_players;
  $("hero-matches").textContent = data.active_matches;
  $("hero-instances").textContent = data.instances;
  $("hero-servers").textContent = data.servers;
  $("hero-online").textContent = data.online;

  // History extras (mmogbrain admin overview, best-effort: blank on old builds).
  if (data.accounts != null) {
    $("tile-accounts").textContent = data.accounts;
    const day = data.new_accounts_24h || 0;
    $("tile-accounts-sub").textContent = `registered · +${day} / 24h`;
  }
  if (data.uptime_seconds != null) $("tile-uptime").textContent = fmtUptime(data.uptime_seconds);
  const crashes = data.host_crashes_recent || {};
  if (data.host_crashes_recent) {
    $("tile-crash-stack").textContent = crashes["stack overflow"] || 0;
    $("tile-crash-av").textContent = crashes["access violation"] || 0;
    $("tile-crash-clean").textContent = crashes["clean"] || 0;
  }
  const modes = data.modes_24h || {};
  const mkeys = Object.keys(modes);
  $("modes-row").textContent = mkeys.length
    ? "Modes / 24h: " + mkeys.map((k) => `${k} ×${modes[k]}`).join(" · ")
    : "No matches in the last 24h.";

  const grid = $("service-grid");
  grid.innerHTML = "";
  for (const [name, svc] of Object.entries(data.services)) {
    const up = !!svc.up;
    const extra = Object.entries(svc)
      .filter(([k]) => !["up", "http_code", "latency_ms", "status", "service"].includes(k))
      .map(([k, v]) => k + "=" + v).join("\n");
    const div = document.createElement("div");
    div.className = "service";
    div.innerHTML = `<div class="name"><span class="dot ${up ? "ok" : "bad"}"></span>${esc(name)}</div>
      <div class="meta">HTTP ${esc(svc.http_code)} · ${esc(svc.latency_ms)} ms${extra ? "\n" + esc(extra) : ""}</div>`;
    grid.appendChild(div);
  }

  // history ring buffer (~5 min at 5s)
  state.history.push({ t: Date.now(), q: data.queued_players, m: data.active_matches, i: data.instances });
  if (state.history.length > 60) state.history.shift();
  drawHistory($("chart-history"), state.history);

  // event feed on up/down transitions
  const key = data.up + "/" + data.total;
  if (state.lastUp !== null && state.lastUp !== key) {
    const ok = data.up === data.total;
    addEvent(ok ? "All services healthy again." : `Status change: ${data.up}/${data.total} online.`, ok ? "good" : "bad");
  }
  state.lastUp = key;

  // stuck queue: players waiting while nothing forms and nothing runs.
  if (data.queued_players > 0 && data.active_matches === 0 && data.instances === 0) state.queueStuck = (state.queueStuck || 0) + 1;
  else state.queueStuck = 0;
  state.lastStatus = data;
  refreshAlerts();
}

function addEvent(text, kind = "") {
  const feed = $("event-feed");
  if (feed.querySelector("p.muted")) feed.innerHTML = "";
  const div = document.createElement("div");
  div.className = "ev " + kind;
  div.innerHTML = `<span class="ts">${new Date().toLocaleTimeString()}</span> — ${esc(text)}`;
  feed.prepend(div);
  while (feed.children.length > 20) feed.lastChild.remove();
}

function drawHistory(canvas, hist) {
  const ctx = canvas.getContext("2d");
  const W = (canvas.width = canvas.clientWidth * 2);
  const H = (canvas.height = 360);
  ctx.clearRect(0, 0, W, H);
  if (hist.length < 2) {
    ctx.fillStyle = "#8b98b8"; ctx.font = "24px sans-serif";
    ctx.fillText("Collecting data…", 20, 40);
    return;
  }
  const max = Math.max(2, ...hist.map((p) => Math.max(p.q, p.m, p.i)));
  const series = [
    { key: "q", color: "#6ea8fe" },
    { key: "m", color: "#34d399" },
    { key: "i", color: "#a06bff" },
  ];
  ctx.strokeStyle = "rgba(120,150,220,.15)"; ctx.lineWidth = 1;
  for (let g = 0; g <= 4; g++) {
    const y = 20 + ((H - 40) * g) / 4;
    ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(W, y); ctx.stroke();
  }
  for (const s of series) {
    ctx.strokeStyle = s.color; ctx.lineWidth = 3; ctx.beginPath();
    hist.forEach((p, idx) => {
      const x = (W * idx) / (hist.length - 1);
      const y = H - 20 - ((H - 40) * p[s.key]) / max;
      idx ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
    });
    ctx.stroke();
  }
}

/* ---------- overview graphs (tile backgrounds) ---------- */
const RANGE_LABELS = { "2m": "2 min", "1h": "1 hour", "24h": "24 hours", "7d": "7 days", "14d": "14 days", "30d": "30 days", "1y": "1 year", "all": "all time" };
// Tile id -> series metric. Gauge tiles keep their live value from loadStatus;
// the series only draws the background line. Counter tiles take value+line.
const TILE_SERIES = {
  "spark-queue": "queued", "spark-matches": "matches", "spark-instances": "instances",
  "spark-servers": "servers", "spark-online": "online", "spark-accounts": "accounts",
  "spark-matches-total": "matches_total", "spark-kills": "kills",
  "spark-credits": "credits", "spark-reports": "reports",
};
const TILE_VALUE = {
  "matches_total": "tile-matches", "kills": "tile-kills", "credits": "tile-credits",
  "reports": "tile-reports", "accounts": "tile-accounts",
};
const SPARK_COLORS = {
  queued: "#6ea8fe", matches: "#34d399", instances: "#a06bff", servers: "#22d3ee",
  online: "#34d399", accounts: "#6ea8fe", matches_total: "#34d399", kills: "#f87171",
  credits: "#fbbf24", reports: "#a06bff",
};

function fmtNum(v) {
  if (v == null || isNaN(v)) return "–";
  v = Math.round(v);
  return v >= 1000000 ? (v / 1000000).toFixed(1) + "M" : v >= 10000 ? (v / 1000).toFixed(1) + "k" : String(v);
}

function fmtUptime(s) {
  if (s == null) return "–";
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m`;
}

async function loadSeries() {
  const metrics = [...new Set(Object.values(TILE_SERIES))].join(",");
  let data;
  try {
    data = await api(`/api/series?metrics=${encodeURIComponent(metrics)}&range=${encodeURIComponent(state.range)}`);
  } catch (e) { return; }
  await loadMarkers();
  const series = data.series || {};
  for (const [svgId, metric] of Object.entries(TILE_SERIES)) {
    const s = series[metric];
    if (!s) continue;
    drawSpark($(svgId), s.points, SPARK_COLORS[metric] || "#6ea8fe", markerCache, data.from, data.to);
    const valueId = TILE_VALUE[metric];
    if (valueId) $(valueId).textContent = fmtNum(s.total);
  }
  const note = $("recorded-note");
  if (data.recorded_since) {
    const since = new Date(data.recorded_since);
    note.textContent = `gauge lines recorded since ${since.toLocaleString()} · counters since database began`;
  } else {
    note.textContent = "no gauge samples yet — collecting (one per minute)";
  }
  loadSLA();
}

async function loadSLA() {
  try {
    const data = await api(`/api/sla?range=${encodeURIComponent(state.range)}`);
    const box = $("sla-box");
    const names = Object.keys(data.services || {});
    if (!names.length) {
      box.innerHTML = '<p class="muted">No samples in range yet — collecting (one per minute).</p>';
    } else {
      box.innerHTML = "<dl>" + names.sort().map((n) => {
        const p = data.services[n];
        const cls = p >= 99 ? "ok" : p >= 95 ? "warn" : "bad";
        return `<dt>${esc(n)}</dt><dd><span class="badge ${cls}">${p.toFixed(1)}%</span> <span class="muted-sm">${data.samples} samples</span></dd>`;
      }).join("") + "</dl>";
    }
    $("sla-note").textContent = `range: ${state.range} · ${data.samples || 0} samples` +
      (data.recorded_since ? ` · since ${new Date(data.recorded_since).toLocaleString()}` : "");
  } catch (e) { /* services card stays useful without it */ }
}

function setRange(r) {
  state.range = r;
  document.querySelectorAll("#rangebar button").forEach((b) => b.classList.toggle("active", b.dataset.range === r));
  loadSeries();
}

async function loadTopKillers() {
  try {
    const data = await api("/api/accounts");
    const list = (data.accounts || []).filter((a) => a.has_player_data && (a.kills || 0) > 0);
    list.sort((a, b) => (b.kills || 0) - (a.kills || 0));
    const tb = tbodyFor("top-table");
    tb.innerHTML = "";
    list.slice(0, 5).forEach((a, i) => {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${i + 1}</td><td>${esc(a.username) || "–"}</td>
        <td>${esc(a.kills)}</td><td>${esc(a.wins)}</td><td>${esc(a.matches)}</td>`;
      tb.appendChild(tr);
    });
    if (!list.length) tb.innerHTML = '<tr><td colspan="5" class="muted">No kills recorded yet.</td></tr>';
  } catch (e) { /* overview stays useful without it */ }
}

/* ---------- players (every registered account) ---------- */
let accountsCache = [];
async function loadPlayers() {
  try {
    const data = await api("/api/accounts");
    accountsCache = data.accounts || [];
    const withData = accountsCache.filter((a) => a.has_player_data).length;
    $("accounts-count").textContent = `${accountsCache.length} accounts · ${withData} with game data`;
    renderPlayers();
  } catch (e) { toast("Players: " + e.message, "err"); }
}
function renderPlayers() {
  const q = ($("accounts-search").value || "").toLowerCase();
  const tb = tbodyFor("accounts-table");
  tb.innerHTML = "";
  for (const a of accountsCache) {
    const hay = `${a.username} ${a.email} ${a.player_id} ${a.id}`.toLowerCase();
    if (q && !hay.includes(q)) continue;
    const tr = document.createElement("tr");
    const status = a.banned
      ? `<span class="badge bad">banned</span>`
      : a.has_player_data ? `<span class="badge ok">active</span>` : `<span class="badge warn">registered only</span>`;
    tr.innerHTML = `<td>${esc(a.username) || "–"}</td><td>${esc(a.email) || "–"}</td>
      <td>${a.player_id ? `<code>${esc(a.player_id)}</code>` : '<span class="muted">–</span>'}</td>
      <td>${esc(a.credits)}</td><td>${esc(a.premium)}</td><td>${esc(a.free_xp)}</td>
      <td>${a.has_player_data ? esc(a.rank) : "–"}</td><td>${esc(a.ships)}</td><td>${esc(a.matches)}</td><td>${esc(a.wins)}</td><td>${esc(a.kills)}</td>
      <td>${status}</td><td class="row"></td>`;
    const cell = tr.lastChild;
    if (a.player_id) {
      const d = document.createElement("button");
      d.className = "btn small"; d.textContent = "🔍";
      d.title = "Details";
      d.onclick = () => showPlayerDetail(a.player_id, a.username);
      cell.appendChild(d);
      const g = document.createElement("button");
      g.className = "btn small"; g.textContent = "→ Grant";
      g.onclick = () => { $("grant-id").value = a.player_id; toast("ID copied to the grant form."); };
      cell.appendChild(g);
      const p = document.createElement("button");
      p.className = "btn small"; p.textContent = "→ Prov";
      p.onclick = () => { $("prov-id").value = a.player_id; toast("ID copied to the provision form."); };
      cell.appendChild(p);
      const rs = document.createElement("button");
      rs.className = "btn small"; rs.textContent = "→ Reset";
      rs.onclick = () => { $("reset-id").value = a.player_id; toast("ID copied to the reset form."); };
      cell.appendChild(rs);
    }
    if (a.username) {
      const b = document.createElement("button");
      b.className = "btn small"; b.textContent = a.banned ? "→ Unban" : "→ Ban";
      b.onclick = () => { $("ban-user").value = a.username; toast("Name copied to the ban form."); };
      cell.appendChild(b);
    }
    tb.appendChild(tr);
  }
}

async function doGrantAll() {
  const c = parseInt($("grant-all-credits").value, 10) || 0;
  const p = parseInt($("grant-all-premium").value, 10) || 0;
  const x = parseInt($("grant-all-xp").value, 10) || 0;
  if (!c && !p && !x) { toast("Nothing to grant.", "err"); return; }
  const n = accountsCache.filter((a) => a.has_player_data).length;
  confirmAction("Give to all?", `${n} accounts get ${c} credits, ${p} premium, ${x} free XP each (added on top).`, async () => {
    const data = await api("/api/grant-all", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ credits: c, premium: p, free_xp: x }),
    });
    $("grant-all-result").textContent = `${data.granted} granted, ${data.failed} failed.`;
    toast(`Give to all: ${data.granted} ok, ${data.failed} errors.`, data.failed ? "err" : "ok");
    loadPlayers();
  });
}

/* ---------- client reports (in-game bug reports) ---------- */
let reportsNameCache = null;
async function loadReports() {
  try {
    const data = await api("/api/reports");
    const list = data.reports || [];
    $("reports-count").textContent = list.length + " reports";
    if (!reportsNameCache) {
      try {
        const acc = await api("/api/accounts");
        reportsNameCache = {};
        for (const a of (acc.accounts || [])) {
          if (a.player_id && a.username) reportsNameCache[String(a.player_id).toLowerCase()] = a.username;
        }
      } catch (e) { reportsNameCache = {}; }
    }
    const tb = tbodyFor("reports-table");
    tb.innerHTML = "";
    for (const r of list) {
      const who = reportsNameCache[String(r.player || "").toLowerCase()] || r.player;
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(r.id)}</td><td>${esc(r.when)}</td><td><code>${esc(who)}</code></td>
        <td>${esc(r.type)}</td><td>${esc(r.name)}</td><td>${esc(r.details)}</td>`;
      tb.appendChild(tr);
    }
    if (!list.length) tb.innerHTML = '<tr><td colspan="6" class="muted">No client reports yet.</td></tr>';
  } catch (e) { toast("Reports: " + e.message, "err"); }
}

/* ---------- activity heatmap + queue ETA (from the match archive) ---------- */
function parseHistTime(s) {
  if (!s) return null;
  const t = new Date(String(s).replace(" ", "T"));
  return isNaN(t) ? null : t;
}

async function heatmapData() {
  // Richer than the 30-row table: up to 500 recent matches for the grid.
  try {
    const data = await api("/api/history?limit=500");
    return data.matches || [];
  } catch (e) { return state.historyData || []; }
}

async function loadHeatmap() {
  const list = await heatmapData();
  const grid = Array.from({ length: 7 }, () => Array(24).fill(0));
  let max = 0;
  for (const m of list) {
    const t = parseHistTime(m.started_at);
    if (!t) continue;
    grid[(t.getDay() + 6) % 7][t.getHours()]++;
    if (grid[(t.getDay() + 6) % 7][t.getHours()] > max) max = grid[(t.getDay() + 6) % 7][t.getHours()];
  }
  const days = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
  const tbl = $("heatmap-table");
  let html = "<thead><tr><th></th>" + Array.from({ length: 24 }, (_, h) => `<th>${h}</th>`).join("") + "</tr></thead><tbody>";
  grid.forEach((row, d) => {
    html += `<tr><th>${days[d]}</th>` + row.map((v) => {
      const a = max ? (v / max).toFixed(2) : 0;
      return `<td style="background:rgba(110,168,254,${a})" title="${v} matches">${v || ""}</td>`;
    }).join("") + "</tr>";
  });
  tbl.innerHTML = html + "</tbody>";
  updateQueueETA(list);
}

// Estimated wait from archive pace: median gap between recent match starts,
// scaled by queue length over median match size. A rough pace estimate, not
// a promise — autoscale, modes and tiers all move it.
function updateQueueETA(matches) {
  const el = $("queue-eta");
  try {
    const times = (matches || []).map((m) => parseHistTime(m.started_at)).filter(Boolean)
      .sort((a, b) => a - b).slice(-20);
    const q = (state.lastQueue || []).length;
    if (q === 0) { el.textContent = "Estimated wait: nobody waiting."; return; }
    if (times.length < 2) { el.textContent = "Estimated wait: unknown (no recent matches)."; return; }
    const gaps = [];
    for (let i = 1; i < times.length; i++) gaps.push((times[i] - times[i - 1]) / 60000);
    gaps.sort((a, b) => a - b);
    const med = gaps[Math.floor(gaps.length / 2)];
    const sizes = (matches || []).slice(-20).map((m) => (m.players || []).length).filter((n) => n > 0).sort((a, b) => a - b);
    const medSize = sizes.length ? sizes[Math.floor(sizes.length / 2)] : 8;
    const eta = med * Math.max(1, Math.ceil(q / Math.max(1, medSize)));
    el.textContent = `Estimated wait: ~${eta < 1 ? "<1" : Math.round(eta)} min at recent pace (${q} waiting, median match every ${med.toFixed(0)} min).`;
  } catch (e) { el.textContent = "Estimated wait: unknown."; }
}

/* ---------- grant presets + CSV export (local conveniences) ---------- */
function grantPresets() {
  try { return JSON.parse(localStorage.getItem("dn-grant-presets") || "[]"); }
  catch (e) { return []; }
}
function saveGrantPresets(list) {
  localStorage.setItem("dn-grant-presets", JSON.stringify(list));
}
function refreshPresetSelect() {
  const sel = $("grant-preset");
  const list = grantPresets();
  sel.innerHTML = list.length
    ? list.map((p, i) => `<option value="${i}">${esc(p.name)}</option>`).join("")
    : `<option value="">(no presets saved)</option>`;
}
function downloadCSV(name, rows) {
  const csv = rows.map((r) => r.map((c) => `"${String(c == null ? "" : c).replace(/"/g, '""')}"`).join(",")).join("\n");
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([csv], { type: "text/csv" }));
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 5000);
}

/* ---------- sleepers, wealth, ships, modes ---------- */
async function loadSleepers() {
  const days = $("sleepers-days").value || "30";
  try {
    const data = await api("/api/sleepers?days=" + encodeURIComponent(days));
    const list = data.sleepers || [];
    $("sleepers-count").textContent = list.length + " dormant";
    const tb = tbodyFor("sleepers-table");
    tb.innerHTML = "";
    for (const s of list) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(s.name) || "–"}</td><td>${esc(s.rank)}</td><td>${esc(s.last_login)}</td>`;
      tb.appendChild(tr);
    }
    if (!list.length) tb.innerHTML = '<tr><td colspan="3" class="muted">Everybody active. Nice.</td></tr>';
  } catch (e) { toast("Sleepers: " + e.message, "err"); }
}

async function loadWealth() {
  try {
    const data = await api("/api/wealth");
    const bands = data.bands || [];
    const max = Math.max(1, ...bands.map((b) => b.count || 0));
    $("wealth-box").innerHTML = bands.map((b) => {
      const pct = Math.round((100 * (b.count || 0)) / max);
      return `<div style="display:flex;align-items:center;gap:10px;margin:3px 0;font-size:13px">
        <span style="min-width:110px" class="muted">${esc(b.label)}</span>
        <div style="flex:1;height:12px;background:#0c1621;border:1px solid #24405a;border-radius:4px">
          <div style="width:${pct}%;height:100%;background:#fbbf24;border-radius:3px"></div></div>
        <span style="min-width:60px">${esc(b.count)}</span></div>`;
    }).join("") || '<p class="muted">No accounts.</p>';
  } catch (e) { /* card stays blank */ }
}

async function loadShips() {
  try {
    const data = await api("/api/ships");
    const tb = tbodyFor("ships-table");
    tb.innerHTML = "";
    for (const s of (data.ships || [])) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(s.name)}</td><td>${Number(s.xp).toLocaleString()}</td><td>${esc(s.pilots)}</td>`;
      tb.appendChild(tr);
    }
    if (!(data.ships || []).length) tb.innerHTML = '<tr><td colspan="3" class="muted">No ship XP recorded yet — fly first.</td></tr>';
  } catch (e) { toast("Ships: " + e.message, "err"); }
}

async function loadModeStats() {
  try {
    const data = await api("/api/mode-stats");
    const tb = tbodyFor("modes-table");
    tb.innerHTML = "";
    for (const m of (data.modes || [])) {
      const wr = m.results ? Math.round((100 * m.wins) / m.results) : 0;
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(m.mode)}</td><td>${esc(m.matches)}</td><td>${esc(m.wins)}</td><td>${wr}%</td><td>${esc(m.kills)}</td>`;
      tb.appendChild(tr);
    }
    if (!(data.modes || []).length) tb.innerHTML = '<tr><td colspan="5" class="muted">No matches reported yet.</td></tr>';
  } catch (e) { toast("Modes: " + e.message, "err"); }
}

/* ---------- queue ---------- */
async function loadQueue() {
  try {
    const data = await api("/api/queue");
    const entries = data.queue || [];
    $("queue-count").textContent = entries.length + " entries";
    const tb = tbodyFor("queue-table");
    tb.innerHTML = "";
    for (const e of entries) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td><code>${esc(e.id)}</code></td><td><code>${esc(e.user_id)}</code></td>
        <td>${esc(e.game_mode)}</td><td>${esc(e.tier_min)}–${esc(e.tier_max)}</td>
        <td><span class="badge ${e.status === "waiting" ? "warn" : "ok"}">${esc(e.status)}</span></td>
        <td>${esc(e.queued_at)}</td><td></td>`;
      const kick = document.createElement("button");
      kick.className = "btn small danger"; kick.textContent = "Kick";
      kick.onclick = () => confirmAction("Remove from queue?", `${e.user_id} (${e.game_mode}) leaves the waiting queue.`, async () => {
        await api("/api/queue/kick/" + e.id, { method: "DELETE" });
        toast("Removed.", "ok");
        loadQueue();
      });
      tr.lastChild.appendChild(kick);
      tb.appendChild(tr);
    }
    drawHistory($("chart-queue"), state.history.length ? state.history : [{ t: 0, q: entries.length, m: 0, i: 0 }]);
    state.lastQueue = entries;
    updateQueueETA(state.historyData);
    if (!state.historyData) heatmapData().then((d) => { state.historyData = d; updateQueueETA(d); });
  } catch (e) { toast("Queue: " + e.message, "err"); }
}

/* ---------- instances ---------- */
async function loadInstances() {
  try {
    const data = await api("/api/instances");
    const list = data.instances || [];
    $("instances-count").textContent = `${list.length} running · ports: ${esc(data.ports_used || 0)}`;
    const tb = tbodyFor("instances-table");
    tb.innerHTML = "";
    for (const i of list) {
      const tr = document.createElement("tr");
      const players = Array.isArray(i.players) ? i.players.length : (i.players || 0);
      tr.innerHTML = `<td><code>${esc(i.id)}</code></td><td>${esc(i.port)}</td><td>${esc(i.game_mode)}</td>
        <td>${esc(i.map)}</td><td>${esc(players)}</td>
        <td>${i.ready === true ? '<span class="badge ok">ready</span>' : i.ready === false ? '<span class="badge warn">loading</span>' : "–"}</td>
        <td>${esc(i.started_at)}</td><td></td>`;
      const btn = document.createElement("button");
      btn.className = "btn small danger"; btn.textContent = "Stop";
      btn.onclick = () => confirmAction("Stop instance?", `${i.id} (${i.map} ${i.game_mode}) will be stopped.`, async () => {
        await api("/api/stop-instance/" + i.id, { method: "POST" });
        toast("Instance stopped.", "ok");
        loadInstances();
      });
      tr.lastChild.appendChild(btn);
      tb.appendChild(tr);
    }
  } catch (e) { toast("Instances: " + e.message, "err"); }
}

/* ---------- servers ---------- */
async function loadServers() {
  try {
    const data = await api("/api/servers");
    const list = data.servers || [];
    $("servers-count").textContent = list.length + " servers";
    const tb = tbodyFor("servers-table");
    tb.innerHTML = "";
    for (const s of list) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td><code>${esc(s.id)}</code></td><td>${esc(s.name)}</td>
        <td>${esc(s.ip)}:${esc(s.port)}</td><td>${esc(s.game_mode)}</td><td>${esc(s.map)}</td>
        <td>${esc(s.current_players)}/${esc(s.max_players)}</td>`;
      tb.appendChild(tr);
    }
  } catch (e) { toast("Servers: " + e.message, "err"); }
}

/* ---------- chat ---------- */
async function loadChat() {
  try {
    const ch = $("chat-channel").value || "global";
    const data = await api("/api/chat?channel=" + encodeURIComponent(ch));
    $("chat-channel-pill").textContent = ch;
    const feed = $("chat-feed");
    feed.innerHTML = "";
    const msgs = data.messages || [];
    if (!msgs.length) feed.innerHTML = '<p class="muted">No messages.</p>';
    for (const m of msgs) {
      const div = document.createElement("div");
      div.className = "ev";
      div.innerHTML = `<span class="ts">${esc(m.sent_at)}</span> <b>${esc(m.sender_id)}</b>: ${esc(m.content)}`;
      feed.appendChild(div);
    }
  } catch (e) { toast("Chat: " + e.message, "err"); }
}

/* ---------- logs ---------- */
async function initLogs() {
  if (!$("log-source").options.length) {
    try {
      const data = await api("/api/logs");
      for (const n of data.sources || []) {
        const o = document.createElement("option");
        o.value = n; o.textContent = n;
        $("log-source").appendChild(o);
      }
      $("log-source").value = "mmog-frames";
    } catch (e) { toast("Logs: " + e.message, "err"); return; }
  }
  loadLog();
}
async function loadLog() {
  const name = $("log-source").value;
  if (name === "battle-logs") {
    // file list mode
    try {
      const file = $("log-file").value;
      const data = await api("/api/logs?name=battle-logs" + (file ? "&file=" + encodeURIComponent(file) + "&lines=" + $("log-lines").value : ""));
      if (data.files) {
        $("log-file").classList.remove("hidden");
        const cur = $("log-file").value;
        $("log-file").innerHTML = "";
        for (const f of data.files) {
          const o = document.createElement("option");
          o.value = f; o.textContent = f;
          $("log-file").appendChild(o);
        }
        if (cur) $("log-file").value = cur;
        $("log-view").textContent = data.files.length ? "Pick a file …" : "No battle logs.";
        return;
      }
      renderLogLines(data.lines || [], data.file || "");
    } catch (e) { toast("Battle logs: " + e.message, "err"); }
    return;
  }
  $("log-file").classList.add("hidden");
  try {
    const data = await api(`/api/logs?name=${encodeURIComponent(name)}&lines=${$("log-lines").value}`);
    $("log-path").textContent = (data.path || "") + (data.truncated ? " · truncated (newest N lines)" : "");
    renderLogLines(data.lines || [], data.note || "");
  } catch (e) { toast("Log: " + e.message, "err"); }
}
function renderLogLines(lines, note) {
  const f = ($("log-filter").value || "").toLowerCase();
  const out = f ? lines.filter((l) => l.toLowerCase().includes(f)) : lines;
  $("log-view").textContent = (note ? note + "\n" : "") + (out.length ? out.join("\n") : "(empty)");
  $("log-view").scrollTop = $("log-view").scrollHeight;
}

/* ---------- metrics ---------- */
async function loadMetrics() {
  try {
    const data = await api("/api/metrics-summary");
    const grid = $("metrics-grid");
    grid.innerHTML = "";
    for (const [svc, gauges] of Object.entries(data)) {
      const box = document.createElement("div");
      box.className = "metric-box";
      let rows = "";
      if (gauges.error) rows = `<dt>error</dt><dd>${esc(gauges.error)}</dd>`;
      else {
        const keys = Object.keys(gauges);
        rows = keys.length
          ? keys.map((k) => `<dt><code>${esc(k)}</code></dt><dd>${esc(gauges[k])}</dd>`).join("")
          : "<dt>–</dt><dd>no dn_ gauges</dd>";
      }
      box.innerHTML = `<h3>${esc(svc)}</h3><dl class="kv">${rows}</dl>`;
      grid.appendChild(box);
    }
  } catch (e) { toast("Metrics: " + e.message, "err"); }
}

/* ---------- config ---------- */
async function loadConfig() {
  try {
    const data = await api("/api/config");
    const kv = (obj) => Object.entries(obj || {}).map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(typeof v === "object" ? JSON.stringify(v) : v)}</dd>`).join("");
    $("config-box").innerHTML = `<dl class="kv">${kv({
      server_ip: data.server_ip, public_host: data.public_host, game_binary: data.game_binary,
      dashboard_addr: data.dashboard_addr, jwt_secret_set: data.jwt_secret_set,
      admin_key_set: data.admin_key_set, internal_key_set: data.internal_key_set,
    })}</dl>`;
    $("cert-box").innerHTML = `<dl class="kv">${kv(data.cert)}</dl>`;
    $("switches-box").innerHTML = `<dl class="kv">${kv(data.switches)}</dl>`;
  } catch (e) { toast("Config: " + e.message, "err"); }
}

/* ---------- actions ---------- */
let modalFn = null;
function confirmAction(title, text, fn) {
  $("modal-title").textContent = title;
  $("modal-text").textContent = text;
  $("modal").classList.remove("hidden");
  modalFn = fn;
}

async function doGrant() {
  const id = $("grant-id").value.trim().toLowerCase();
  const payload = { user_id: id };
  const c = parseInt($("grant-credits").value, 10) || 0;
  const p = parseInt($("grant-premium").value, 10) || 0;
  const x = parseInt($("grant-xp").value, 10) || 0;
  if (c) payload.credits = c;
  if (p) payload.premium = p;
  if (x) payload.free_xp = x;
  confirmAction("Grant balance?", `${id}: ${c} credits, ${p} premium, ${x} free XP`, async () => {
    await api("/api/grant", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
    toast("Granted.", "ok");
    loadPlayers();
  });
}

/* ---------- online ---------- */
async function loadOnline() {
  try {
    const data = await api("/api/online");
    const list = data.online || [];
    $("online-count").textContent = list.length + " connected";
    const tb = tbodyFor("online-table");
    tb.innerHTML = "";
    for (const p of list) {
      const where = p.match_id
        ? `<span class="badge ok">Match ${esc(p.game_mode)} T${esc(p.team)}</span>`
        : p.queued_mode ? `<span class="badge warn">Queue ${esc(p.queued_mode)}</span>` : '<span class="muted">Hangar</span>';
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(p.name) || "–"}</td><td><code>${esc(p.player_id)}</code></td>
        <td>${esc((p.channels || []).join(", "))}</td>
        <td>${p.queued_mode ? esc(p.queued_mode) : "–"}</td><td>${where}</td><td>${p.team || "–"}</td>`;
      tb.appendChild(tr);
    }
  } catch (e) { toast("Online: " + e.message, "err"); }
}

/* ---------- broadcast ---------- */
async function sendBroadcast() {
  const channel = $("bc-channel").value.trim() || "dreadnought.global";
  const content = $("bc-message").value.trim();
  if (!content) { toast("Message missing.", "err"); return; }
  confirmAction("Send broadcast?", `"${content}" to ${channel} (all connected players).`, async () => {
    const data = await api("/api/broadcast", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ channel, content }),
    });
    toast(`Sent (${data.reached} reached).`, "ok");
    $("bc-message").value = "";
    loadChat();
  });
}

/* ---------- results ---------- */
async function loadResults() {
  try {
    const data = await api("/api/results?limit=50");
    const list = data.results || [];
    resultsCache = list;
    $("results-count").textContent = list.length + " reports";
    const tb = tbodyFor("results-table");
    tb.innerHTML = "";
    for (const r of list) {
      const cls = r.outcome === "win" ? "ok" : r.outcome === "loss" ? "bad" : "warn";
      const tr = document.createElement("tr");
      tr.innerHTML = `<td><code>${esc(r.match_id.slice(0, 8))}…</code></td><td><code>${esc(r.user_id.slice(0, 8))}…</code></td>
        <td>${esc(r.team)}</td><td><span class="badge ${cls}">${esc(r.outcome)}</span></td>
        <td>${esc(r.kills)}</td><td>${esc(r.credits)}</td><td>${esc(r.xp)}</td><td>${esc(r.reported_at)}</td>`;
      tb.appendChild(tr);
    }
    renderStats();
  } catch (e) { toast("Results: " + e.message, "err"); }
}

/* ---------- bans ---------- */
async function loadBans() {
  try {
    const data = await api("/api/bans");
    const list = data.bans || [];
    $("bans-count").textContent = list.length + " active";
    const tb = tbodyFor("bans-table");
    tb.innerHTML = "";
    for (const b of list) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(b.username)}</td><td>${esc(b.reason)}</td><td>${esc(b.since)}</td><td></td>`;
      const btn = document.createElement("button");
      btn.className = "btn small"; btn.textContent = "Unban";
      btn.onclick = () => {
        $("ban-user").value = b.username;
        toast("Name copied to the ban form — click Unban there.");
      };
      tr.lastChild.appendChild(btn);
      tb.appendChild(tr);
    }
  } catch (e) { toast("Bans: " + e.message, "err"); }
}

/* ---------- news tiles ---------- */
async function loadTiles() {
  try {
    const data = await api("/api/tiles");
    const list = data.tiles || [];
    $("tiles-count").textContent = list.length + " tiles";
    const tb = tbodyFor("tiles-table");
    tb.innerHTML = "";
    for (const t of list) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td><code>${esc(t.id)}</code></td><td>${esc(t.title)}</td><td>${esc(t.type)}</td>
        <td>${esc(t.section_size)}</td>
        <td>${t.active ? '<span class="badge ok">on</span>' : '<span class="badge warn">off</span>'}</td><td class="row"></td>`;
      const cell = tr.lastChild;
      const edit = document.createElement("button");
      edit.className = "btn small"; edit.textContent = "✎";
      edit.onclick = () => {
        $("tile-id").value = t.id; $("tile-title").value = t.title;
        $("tile-body").value = t.body || ""; $("tile-type").value = t.type;
        $("tile-size").value = t.section_size; $("tile-active").checked = !!t.active;
        toast("Tile copied to the form.");
      };
      const del = document.createElement("button");
      del.className = "btn small danger"; del.textContent = "Delete";
      del.onclick = () => confirmAction("Delete tile?", `${t.id} will be removed from the launcher.`, async () => {
        await api("/api/tiles/" + encodeURIComponent(t.id), { method: "DELETE" });
        toast("Deleted.", "ok");
        loadTiles();
      });
      cell.appendChild(edit); cell.appendChild(del);
      tb.appendChild(tr);
    }
  } catch (e) { toast("News: " + e.message, "err"); }
}

async function saveTile() {
  const payload = {
    id: $("tile-id").value.trim(),
    title: $("tile-title").value.trim(),
    body: $("tile-body").value,
    type: $("tile-type").value,
    section_size: $("tile-size").value,
    active: $("tile-active").checked,
  };
  if (!payload.id || !payload.title) { toast("ID and title are required.", "err"); return; }
  try {
    await api("/api/tiles", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
    toast("Saved — the launcher shows it on next load.", "ok");
    loadTiles();
  } catch (e) { toast(e.message, "err"); }
}

/* ---------- provision ---------- */
async function doProvision() {
  const id = $("prov-id").value.trim().toLowerCase();
  if (!/^[0-9a-f]{32}$/.test(id)) { toast("32-hex user ID required.", "err"); return; }
  const payload = {
    user_id: id,
    rank: parseInt($("prov-rank").value, 10) || 20,
    credits: parseInt($("prov-credits").value, 10) || 0,
    premium: parseInt($("prov-premium").value, 10) || 0,
    free_xp: parseInt($("prov-xp").value, 10) || 0,
  };
  confirmAction("Equip test account?", `${id}: rank ${payload.rank}, values are SET (not added), all ships+items unlocked.`, async () => {
    const data = await api("/api/provision", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload),
    });
    const s = data.summary || {};
    $("prov-result").textContent = `Rank ${s.rank}, ${s.ships} ships, ${s.items} items, ${s.loadouts} loadouts.`;
    toast("Equipped.", "ok");
    loadPlayers();
  });
}

/* ---------- player detail ---------- */
async function showPlayerDetail(pid, name) {
  const box = $("player-detail");
  box.innerHTML = '<p class="muted">Loading …</p>';
  try {
    const d = await api("/api/player/" + pid);
    const p = d.player || {};
    const kv = (obj) => Object.entries(obj || {}).map(([k, v]) => {
      const val = (v && typeof v === "object") ? JSON.stringify(v) : String(v == null ? "" : v);
      return `<dt>${esc(k)}</dt><dd>${esc(val)}</dd>`;
    }).join("");
    const fleetRows = (d.fleets || []).map((f) => `${esc(f.name)} (T${esc(f.fleet_type)}${f.active ? ", active" : ""}, ${esc(f.loadouts)} loadouts)`).join("<br>") || "–";
    const shipRows = (d.ships || []).slice(0, 12).map((s) => `${esc(s.name)} — ${esc(s.ship_xp)} XP`).join("<br>") || "–";
    const resRows = (d.recent_results || []).map((r) => `${esc(r.match_id.slice(0, 8))}: ${esc(r.outcome)} (+${esc(r.credits)}c/+${esc(r.xp)}xp)`).join("<br>") || "–";
    box.innerHTML = `<h3>${esc(name || pid)}</h3><dl class="kv">${kv({
      credits: p.credits, premium: p.premium, free_xp: p.free_xp,
      rank: p.current_rank, rank_xp: p.rank_xp, queue: d.queued_mode || "–",
      match: d.in_match ? `${d.live_match.game_mode} ${d.live_match.map} (T${d.live_match.team})` : "–",
      purchases: d.purchases,
    })}</dl>
    <p><b>Fleets:</b><br>${fleetRows}</p>
    <p><b>Ships (top 12 by XP):</b><br>${shipRows}</p>
    <p><b>Recent results:</b><br>${resRows}</p>
    <div id="player-progress"><p class="muted">Loading career …</p></div>`;
    showPlayerProgress(pid).then((html) => {
      const el = $("player-progress");
      if (el) el.innerHTML = html;
    });
  } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
}

/* ---------- queue actions ---------- */
async function clearQueue() {
  confirmAction("Clear queue?", "All waiting entries will be removed (running matches stay).", async () => {
    const data = await api("/api/queue/clear", { method: "POST" });
    toast(`${data.cleared} entries removed.`, "ok");
    loadQueue();
  });
}

async function forceMatch() {
  confirmAction("Force match?", "The largest waiting group plays now (cap: PLAYERS_PER_MATCH).", async () => {
    const data = await api("/api/force-match", { method: "POST" });
    toast(`Match formed: ${data.players} players (${data.game_mode}).`, "ok");
    loadQueue(); loadInstances();
  });
}

/* ---------- backups ---------- */
function fmtSize(n) {
  if (n > 1048576) return (n / 1048576).toFixed(1) + " MB";
  if (n > 1024) return (n / 1024).toFixed(1) + " KB";
  return n + " B";
}
async function loadBackups() {
  try {
    const data = await api("/api/backups");
    const list = data.backups || [];
    $("backups-count").textContent = list.length + " archives";
    const tb = tbodyFor("backups-table");
    tb.innerHTML = "";
    if (!list.length) tb.innerHTML = '<tr><td colspan="3" class="muted">No backups — set up cron: backup.sh --install-cron</td></tr>';
    for (const b of list) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td><code>${esc(b.name)}</code></td><td>${fmtSize(b.size)}</td><td>${esc(b.mtime)}</td>`;
      tb.appendChild(tr);
    }
  } catch (e) { toast("Backups: " + e.message, "err"); }
}

/* ---------- reset ---------- */
async function doReset() {
  const id = $("reset-id").value.trim().toLowerCase();
  if (!/^[0-9a-f]{32}$/.test(id)) { toast("32-hex user ID required.", "err"); return; }
  const currencies = $("reset-currencies").checked;
  const research = $("reset-research").checked;
  if (!currencies && !research) { toast("Nothing selected.", "err"); return; }
  const what = [currencies && "currencies", research && "research"].filter(Boolean).join(" + ");
  confirmAction("Really reset?", `${id}: ${what} will go back to fresh. Cannot be undone!`, async () => {
    await api("/api/reset", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user_id: id, currencies, research }),
    });
    $("reset-result").textContent = `Reset (${what}). Re-login the client.`;
    toast("Reset done.", "ok");
    loadPlayers();
  });
}

/* ---------- alerts ---------- */
async function refreshAlerts() {
  const box = $("alerts");
  if (!box) return;
  const st = state.lastStatus;
  const alerts = [];
  if (st && st.up < st.total) alerts.push(["bad", `${st.total - st.up} service(s) down — see Services below.`]);
  if ((state.queueStuck || 0) >= 12) alerts.push(["warn", `Queue stuck: ${st.queued_players} waiting for ~60s with nothing forming.`]);
  // Config + backups at most once a minute; status polls every 5s.
  if (!state.alertsAt || Date.now() - state.alertsAt > 60000) {
    state.alertsAt = Date.now();
    try {
      const cfg = await api("/api/config");
      const cert = cfg.cert || {};
      if (cert.expired) alerts.push(["bad", "TLS certificate EXPIRED — regenerate via gen-certs.sh."]);
      else if (cert.not_after) {
        const days = (new Date(cert.not_after) - Date.now()) / 86400000;
        if (days < 30) alerts.push(["warn", `TLS certificate expires in ${Math.max(0, Math.round(days))} days.`]);
      }
    } catch (e) { /* config optional for alerts */ }
    try {
      const bk = await api("/api/backups");
      if (!bk.count) alerts.push(["warn", "No backups yet — run backup.sh --install-cron."]);
    } catch (e) { /* backups optional for alerts */ }
    state.alertCache = alerts;
  } else if (state.alertCache) {
    alerts.push(...state.alertCache);
  }
  box.innerHTML = alerts.map(([k, t]) => `<div class="ev ${k === "bad" ? "bad" : ""}">${esc(t)}</div>`).join("");
}

/* ---------- match center ---------- */
let matchesCache = [];
async function loadMatchesList() {
  try {
    const data = await api("/api/matches?limit=50");
    matchesCache = data.matches || [];
    const sel = $("match-select");
    const cur = sel.value;
    sel.innerHTML = "";
    const sorted = [...matchesCache].sort((a, b) => {
      if ((a.status === "active") !== (b.status === "active")) return a.status === "active" ? -1 : 1;
      return b.created_at.localeCompare(a.created_at);
    });
    for (const m of sorted) {
      const o = document.createElement("option");
      o.value = m.id;
      o.textContent = `${m.status === "active" ? "● " : ""}${m.game_mode} ${m.map} · ${m.players}p · ${m.created_at}`;
      sel.appendChild(o);
    }
    if (cur) sel.value = cur;
    if (sel.value) showMatchDetail(sel.value);
    else $("match-detail").innerHTML = '<p class="muted">No matches yet.</p>';
  } catch (e) { toast("Matches: " + e.message, "err"); }
}

async function showMatchDetail(id) {
  const box = $("match-detail");
  box.innerHTML = '<p class="muted">Loading …</p>';
  try {
    const d = await api("/api/match/" + id);
    const m = d.match || {};
    const slots = d.slots || [];
    const t1 = slots.filter((s) => s.team === 1).length;
    const t2 = slots.filter((s) => s.team === 2).length;
    let extra = "";
    if (m.instance_id) {
      try {
        const inst = await api("/api/instance/" + m.instance_id);
        extra = `<dt>host</dt><dd>${inst.ready === true ? "ready" : inst.ready === false ? "loading" : "unknown"}${inst.port ? " · port " + esc(inst.port) : ""}</dd>`;
      } catch (e) { extra = `<dt>host</dt><dd>gone</dd>`; }
    }
    let logHint = "";
    if (m.server_port) {
      try {
        const logs = await api("/api/logs?name=battle-logs");
        const hit = (logs.files || []).find((f) => f.includes("port" + m.server_port));
        if (hit) logHint = `<dt>battle log</dt><dd><code>${esc(hit)}</code> (Logs tab)</dd>`;
      } catch (e) { /* logs optional */ }
    }
    const rows = slots.map((s) => `<td><code>${esc(s.user_id.slice(0, 8))}…</code></td><td>${esc(s.team)}</td>`).join("");
    const balance = await showTeamBalance(slots);
    box.innerHTML = `<dl class="kv">
      <dt>mode</dt><dd>${esc(m.game_mode)} on ${esc(m.map)}</dd>
      <dt>status</dt><dd>${esc(m.status)} · teams ${t1}v${t2}</dd>
      <dt>address</dt><dd>${esc(m.server_ip)}:${esc(m.server_port)}</dd>
      <dt>formed</dt><dd>${esc(m.created_at)}${m.server_ready_at ? " · host ready " + esc(m.server_ready_at) : ""}</dd>
      ${extra}${logHint}${balance}</dl>
      <div class="table-wrap"><table><thead><tr><th>Player</th><th>Team</th></tr></thead><tbody>${rows || '<tr><td colspan="2" class="muted">No slots.</td></tr>'}</tbody></table></div>`;
  } catch (e) { box.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
}

/* ---------- statistics ---------- */
let resultsCache = [];
function renderStats() {
  const list = resultsCache;
  const by = (k) => list.filter((r) => r.outcome === k).length;
  const wins = by("win"), losses = by("loss"), draws = by("draw"), unknown = list.length - wins - losses - draws;
  const kills = list.reduce((a, r) => a + (r.kills || 0), 0);
  const credits = list.reduce((a, r) => a + (r.credits || 0), 0);
  const xp = list.reduce((a, r) => a + (r.xp || 0), 0);
  const modes = {};
  for (const r of list) {
    const m = r.game_mode || "?";
    modes[m] = modes[m] || { n: 0, win: 0 };
    modes[m].n++;
    if (r.outcome === "win") modes[m].win++;
  }
  const modeRows = Object.entries(modes).map(([m, v]) =>
    `<dt>${esc(m)}</dt><dd>${v.n} reported · ${v.n ? Math.round((100 * v.win) / v.n) : 0}% won</dd>`).join("");
  $("stats-box").innerHTML = `<dl class="kv">
    <dt>reported</dt><dd>${list.length}</dd>
    <dt>win rate</dt><dd>${list.length ? Math.round((100 * wins) / list.length) : 0}% (${wins}W/${losses}L/${draws}D/${unknown}?)</dd>
    <dt>avg kills</dt><dd>${list.length ? (kills / list.length).toFixed(1) : 0}</dd>
    <dt>paid out</dt><dd>${credits} credits · ${xp} XP</dd>${modeRows}</dl>`;
  drawOutcomeBars($("chart-outcomes"), { wins, losses, draws, unknown });
}

function drawOutcomeBars(canvas, v) {
  const ctx = canvas.getContext("2d");
  const W = (canvas.width = canvas.clientWidth * 2);
  const H = (canvas.height = 280);
  ctx.clearRect(0, 0, W, H);
  const total = Math.max(1, v.wins + v.losses + v.draws + v.unknown);
  const bars = [
    ["win", v.wins, "#34d399"], ["loss", v.losses, "#f87171"],
    ["draw", v.draws, "#fbbf24"], ["?", v.unknown, "#8b98b8"],
  ];
  const bw = W / bars.length;
  ctx.font = "22px sans-serif"; ctx.textAlign = "center";
  bars.forEach(([label, n, color], i) => {
    const h = ((H - 60) * n) / total;
    ctx.fillStyle = color;
    ctx.fillRect(i * bw + bw * 0.25, H - 30 - h, bw * 0.5, h);
    ctx.fillStyle = "#e8eefc";
    ctx.fillText(`${label} ${n}`, i * bw + bw / 2, H - 8);
  });
}

/* ---------- history ---------- */
async function loadHistory() {
  try {
    const data = await api("/api/history?limit=30");
    const list = data.matches || [];
    state.historyData = list;
    $("history-count").textContent = list.length + " matches";
    const tb = tbodyFor("history-table");
    tb.innerHTML = "";
    for (const m of list) {
      const roster = (m.players || []).map((p) => `${esc(p.user_id.slice(0, 8))} T${esc(p.team)} ${esc(p.kills)}k`).join(", ") || "–";
      const tr = document.createElement("tr");
      tr.innerHTML = `<td><code>${esc(m.id.slice(0, 8))}…</code></td><td>${esc(m.mode)}</td><td>${esc(m.map)}</td>
        <td>${esc(m.started_at)}</td><td>${esc(m.players ? m.players.length : 0)}</td><td>${roster}</td>`;
      tb.appendChild(tr);
    }
  } catch (e) { toast("History: " + e.message, "err"); }
}

/* ---------- catalog ---------- */
let catalogCache = [];
async function loadCatalog() {
  try {
    const data = await api("/api/catalog");
    catalogCache = data.ships || [];
    $("catalog-count").textContent = catalogCache.length + " hulls";
    renderCatalog();
  } catch (e) { toast("Catalog: " + e.message, "err"); }
}
function renderCatalog() {
  const q = ($("catalog-search").value || "").toLowerCase();
  const tb = tbodyFor("catalog-table");
  tb.innerHTML = "";
  for (const s of catalogCache) {
    const hay = `${s.name} ${s.line} ${s.manufacturer} ${s.tier}`.toLowerCase();
    if (q && !hay.includes(q)) continue;
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${esc(s.name)}</td><td>${esc(s.tier)}</td><td>${esc(s.line)}</td>
      <td>${esc(s.manufacturer)}</td><td>${s.hero ? "hero" : "–"}</td>
      <td>${Number(s.price_credits).toLocaleString()}</td><td>${esc(s.owners)}</td>`;
    tb.appendChild(tr);
  }
}

/* ---------- audit ---------- */
async function loadAudit() {
  try {
    const data = await api("/api/audit?lines=500");
    const list = data.entries || [];
    $("audit-count").textContent = list.length + " entries";
    const tb = tbodyFor("audit-table");
    tb.innerHTML = "";
    for (const line of list) {
      let time = "", action = "", detail = line;
      try {
        const o = JSON.parse(line);
        time = o.time || ""; action = o.action || ""; detail = o.detail || "";
      } catch (e) { /* raw line */ }
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(time)}</td><td><code>${esc(action)}</code></td><td>${esc(detail)}</td>`;
      tb.appendChild(tr);
    }
    renderAuditTimeline(list);
  } catch (e) { toast("Audit: " + e.message, "err"); }
}

// Operator actions per day (last 14 days with entries): which days were busy.
function renderAuditTimeline(list) {
  const box = $("audit-timeline");
  const byDay = {};
  for (const line of (list || [])) {
    try {
      const o = JSON.parse(line);
      const day = String(o.time || "").slice(0, 10);
      if (!/^\d{4}-\d{2}-\d{2}$/.test(day)) continue;
      byDay[day] = byDay[day] || {};
      byDay[day][o.action || "?"] = (byDay[day][o.action || "?"] || 0) + 1;
    } catch (e) { /* raw line */ }
  }
  const days = Object.keys(byDay).sort().slice(-14);
  if (!days.length) { box.innerHTML = '<p class="muted">No timestamped actions yet.</p>'; return; }
  const max = Math.max(...days.map((d) => Object.values(byDay[d]).reduce((a, b) => a + b, 0)));
  box.innerHTML = days.map((d) => {
    const total = Object.values(byDay[d]).reduce((a, b) => a + b, 0);
    const top = Object.entries(byDay[d]).sort((a, b) => b[1] - a[1]).slice(0, 3)
      .map(([a, n]) => `${a} ×${n}`).join(", ");
    const pct = max ? Math.round((100 * total) / max) : 0;
    return `<div style="display:flex;align-items:center;gap:10px;margin:3px 0;font-size:13px">
      <span style="min-width:100px" class="muted">${esc(d)}</span>
      <div style="flex:1;height:12px;background:#0c1621;border:1px solid #24405a;border-radius:4px">
        <div style="width:${pct}%;height:100%;background:#6ea8fe;border-radius:3px"></div></div>
      <span style="min-width:220px">${esc(top)} (${total})</span></div>`;
  }).join("");
}

/* ---------- crash reports ---------- */
async function loadCrashes() {
  try {
    const data = await api("/api/crashes");
    const list = data.entries || [];
    const dirSel = $("crash-dir");
    const cur = dirSel.value;
    dirSel.innerHTML = "";
    if (!list.length) {
      $("crash-view").textContent = "No crash reports yet.";
      return;
    }
    for (const e of list) {
      const o = document.createElement("option");
      o.value = (e.dir ? "d:" : "f:") + e.name;
      o.textContent = (e.dir ? "📁 " : "📄 ") + e.name;
      dirSel.appendChild(o);
    }
    if (cur) dirSel.value = cur;
    showCrash();
  } catch (e) { toast("Crashes: " + e.message, "err"); }
}

async function showCrash() {
  const v = $("crash-dir").value || "";
  const isDir = v.startsWith("d:");
  const name = v.slice(2);
  const fileSel = $("crash-file");
  try {
    if (isDir) {
      const data = await api("/api/crashes?dir=" + encodeURIComponent(name));
      const files = data.files || [];
      fileSel.classList.remove("hidden");
      fileSel.innerHTML = "";
      for (const f of files) {
        const o = document.createElement("option");
        o.value = f.name; o.textContent = `${f.name} (${fmtSize(f.size)})`;
        fileSel.appendChild(o);
      }
      if (!files.length) { $("crash-view").textContent = "(empty report folder)"; return; }
      const first = await api(`/api/crashes?dir=${encodeURIComponent(name)}&file=${encodeURIComponent(files[0].name)}&lines=200`);
      $("crash-view").textContent = (first.lines || []).join("\n") || "(empty)";
      return;
    }
    fileSel.classList.add("hidden");
    const data = await api("/api/crashes?file=" + encodeURIComponent(name) + "&lines=200");
    $("crash-view").textContent = (data.lines || []).join("\n") || "(empty)";
  } catch (e) { $("crash-view").textContent = e.message; }
}

/* ---------- sessions ---------- */
async function loadSessions() {
  try {
    const data = await api("/api/sessions");
    const list = data.sessions || [];
    $("sessions-count").textContent = list.length + " active";
    const tb = tbodyFor("sessions-table");
    tb.innerHTML = "";
    for (const s of list) {
      const tr = document.createElement("tr");
      tr.innerHTML = `<td>${esc(s.username)}</td><td>${esc(s.created_at)}</td><td>${esc(s.expires_at)}</td>
        <td>${s.expired ? '<span class="badge warn">expired</span>' : '<span class="badge ok">live</span>'}</td><td></td>`;
      const btn = document.createElement("button");
      btn.className = "btn small danger"; btn.textContent = "Revoke";
      btn.onclick = () => confirmAction("Revoke session?", `${s.username} will be signed out.`, async () => {
        await api("/api/sessions/" + s.id, { method: "DELETE" });
        toast("Session revoked.", "ok");
        loadSessions();
      });
      tr.lastChild.appendChild(btn);
      tb.appendChild(tr);
    }
  } catch (e) { toast("Sessions: " + e.message, "err"); }
}

/* ---------- player progress (career, season, contracts) ---------- */
async function showPlayerProgress(pid) {
  try {
    const d = await api("/api/player/" + pid + "/progress");
    const goals = (d.goals || []).map((g) => {
      const stages = (g.stages || []).map((s) => s.amount).join("/");
      return `${esc(g.title)} [${esc(g.category)}]: ${esc(g.progress)}${stages ? " (stages " + esc(stages) + ")" : ""}`;
    }).join("<br>") || "–";
    const seasons = (d.seasons || []).map((s) => `${esc(s.season_id)}: level ${esc(s.level)} (${esc(s.xp)} XP)`).join("<br>") || "–";
    const contracts = (d.contracts || []).map((c) => `${esc(c.contract_id)}: ${esc(c.state)} ${esc(c.progress)}%`).join("<br>") || "–";
    const counters = (d.counters || []).slice(0, 8).map((c) => `${esc(c.counter_id)}${c.counter_sub_id ? "/" + esc(c.counter_sub_id) : ""}: ${esc(c.value)}`).join("<br>") || "–";
    return `<p><b>Career goals:</b><br>${goals}</p>
      <p><b>Seasons:</b><br>${seasons}</p>
      <p><b>Contracts:</b><br>${contracts}</p>
      <p><b>Top counters:</b><br>${counters}</p>`;
  } catch (e) { return `<p class="error">${esc(e.message)}</p>`; }
}

/* ---------- markers, search, balance, economy, sessions, maintenance ---------- */
let markerCache = [];

// drawSpark paints a filled area sparkline into an <svg> that sits BEHIND
// the tile text (CSS .spark): same tile size, no layout change. Flat lines
// (all zeros / single value) render as a baseline, never as an error.
// markers draw as vertical lines when their timestamp falls in [from, to].
function drawSpark(svg, points, color, markers, from, to) {
  if (!svg) return;
  const W = 100, H = 30;
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.innerHTML = "";
  const ns = "http://www.w3.org/2000/svg";
  if (points && points.length >= 2) {
    const vals = points.map((p) => +p[1] || 0);
    const max = Math.max(...vals);
    const min = Math.min(...vals);
    const span = max - min || 1;
    const step = W / (vals.length - 1);
    let d = "";
    vals.forEach((v, i) => {
      const x = (i * step).toFixed(1);
      const y = (H - 2 - ((v - min) / span) * (H - 5)).toFixed(1);
      d += (i ? "L" : "M") + x + " " + y;
    });
    const area = document.createElementNS(ns, "path");
    area.setAttribute("d", d + `L${W} ${H}L0 ${H}Z`);
    area.setAttribute("fill", color);
    area.setAttribute("opacity", "0.25");
    const line = document.createElementNS(ns, "path");
    line.setAttribute("d", d);
    line.setAttribute("fill", "none");
    line.setAttribute("stroke", color);
    line.setAttribute("stroke-width", "1.2");
    line.setAttribute("vector-effect", "non-scaling-stroke");
    svg.appendChild(area);
    svg.appendChild(line);
  }
  if (markers && from && to) {
    const f = new Date(from).getTime(), t = new Date(to).getTime();
    if (t > f) {
      for (const m of markers) {
        const mt = m.t * 1000;
        if (mt < f || mt > t) continue;
        const x = ((mt - f) / (t - f)) * W;
        const ln = document.createElementNS(ns, "line");
        ln.setAttribute("x1", x); ln.setAttribute("x2", x);
        ln.setAttribute("y1", 0); ln.setAttribute("y2", H);
        ln.setAttribute("stroke", "#fbbf24");
        ln.setAttribute("stroke-width", "0.8");
        const title = document.createElementNS(ns, "title");
        title.textContent = m.label || "";
        ln.appendChild(title);
        svg.appendChild(ln);
      }
    }
  }
}

async function loadMarkers() {
  try {
    const data = await api("/api/events");
    markerCache = data.events || [];
  } catch (e) { markerCache = []; }
  const box = $("marker-list");
  box.innerHTML = "";
  for (const m of markerCache) {
    const div = document.createElement("div");
    div.className = "ev";
    div.innerHTML = `<span class="ts">${esc(new Date(m.t * 1000).toLocaleString())}</span> — ${esc(m.label)}`;
    const del = document.createElement("button");
    del.className = "btn small danger";
    del.textContent = "✕";
    del.onclick = async () => {
      await api("/api/events/" + encodeURIComponent(m.id), { method: "DELETE" }).catch((e) => toast(e.message, "err"));
      loadMarkers();
      loadSeries();
    };
    div.appendChild(del);
    box.appendChild(div);
  }
  if (!markerCache.length) box.innerHTML = '<p class="muted">No markers — restarts and deploys land here.</p>';
}

async function globalSearch() {
  const q = ($("global-search").value || "").trim().toLowerCase();
  if (!q) return;
  try {
    const data = await api("/api/accounts");
    const hit = (data.accounts || []).find((a) =>
      (a.username || "").toLowerCase().includes(q) || (a.email || "").toLowerCase().includes(q) ||
      (a.player_id || "").toLowerCase() === q || (a.id || "").toLowerCase() === q);
    if (!hit) { toast("No account matches.", "err"); return; }
    switchTab("players");
    $("accounts-search").value = hit.username || hit.email || q;
    if (!accountsCache.length) await loadPlayers();
    renderPlayers();
    toast(`Found ${hit.username || hit.email}.`, "ok");
  } catch (e) { toast(e.message, "err"); }
}

async function showTeamBalance(slots) {
  // Average rank + kills per team: are matches fair?
  try {
    const data = await api("/api/accounts");
    const byPid = {};
    for (const a of (data.accounts || [])) {
      if (a.player_id) byPid[String(a.player_id).toLowerCase()] = a;
    }
    const teams = {};
    for (const s of (slots || [])) {
      const t = s.team || 0;
      teams[t] = teams[t] || { n: 0, rank: 0, kills: 0 };
      const a = byPid[String((s.user_id || "").replace(/-/g, "").toLowerCase())];
      teams[t].n++;
      if (a) { teams[t].rank += a.rank || 0; teams[t].kills += a.kills || 0; }
    }
    const rows = Object.entries(teams).map(([t, v]) =>
      `<dt>team ${esc(t)}</dt><dd>${v.n} pilots · avg rank ${(v.n ? v.rank / v.n : 0).toFixed(1)} · ${v.kills} kills</dd>`).join("");
    return rows ? `<dt>balance</dt><dd><dl class="kv">${rows}</dl></dd>` : "";
  } catch (e) { return ""; }
}

async function loadEconomyAndSessions() {
  try {
    const data = await api(`/api/series?metrics=${encodeURIComponent("credits,spending,online")}&range=${encodeURIComponent(state.range)}`);
    const series = data.series || {};
    const pay = series.credits || {}, spend = series.spending || {}, online = series.online || {};
    $("eco-payouts").textContent = fmtNum(pay.total) + " credits";
    $("eco-spending").textContent = fmtNum(spend.total) + " credits";
    drawSpark($("spark-eco-payouts"), pay.points, "#34d399");
    drawSpark($("spark-eco-spending"), spend.points, "#fbbf24");
    const net = (pay.total || 0) - (spend.total || 0);
    $("eco-note").textContent = `net ${net >= 0 ? "+" : ""}${fmtNum(net)} credits in range · faucet vs sink`;
    // Sessions from the online line: peak, average, span.
    const pts = online.points || [];
    const vals = pts.map((p) => +p[1] || 0);
    if (!vals.length) {
      $("session-box").innerHTML = '<p class="muted">No samples in range yet.</p>';
      return;
    }
    let peak = 0, peakT = 0, sum = 0;
    vals.forEach((v, i) => {
      sum += v;
      if (v > peak) { peak = v; peakT = pts[i][0]; }
    });
    const byHour = Array(24).fill(0), byHourN = Array(24).fill(0);
    pts.forEach((p) => {
      const h = new Date(p[0] * 1000).getHours();
      byHour[h] += +p[1] || 0; byHourN[h]++;
    });
    let bestH = 0;
    byHour.forEach((v, h) => { if (byHourN[h] && v / byHourN[h] > (byHour[bestH] / Math.max(1, byHourN[bestH]))) bestH = h; });
    // Busiest weekday from the same points.
    const days = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
    const byDay = Array(7).fill(0), byDayN = Array(7).fill(0);
    pts.forEach((p) => {
      const d = new Date(p[0] * 1000).getDay();
      byDay[d] += +p[1] || 0; byDayN[d]++;
    });
    let bestD = 0;
    byDay.forEach((v, d) => { if (byDayN[d] && v / byDayN[d] > (byDay[bestD] / Math.max(1, byDayN[bestD]))) bestD = d; });
    $("session-box").innerHTML = `<dl class="kv">
      <dt>peak online</dt><dd>${peak} (${peakT ? new Date(peakT * 1000).toLocaleString() : "–"})</dd>
      <dt>average online</dt><dd>${(sum / vals.length).toFixed(1)}</dd>
      <dt>busiest hour</dt><dd>${bestH}:00–${bestH}:59</dd>
      <dt>busiest weekday</dt><dd>${days[bestD]}</dd>
      <dt>samples</dt><dd>${vals.length} points in range</dd></dl>`;
  } catch (e) { /* cards stay blank, tiles carry the page */ }
}

async function setMaintenance(on) {
  const msg = ($("maint-message").value || "").trim() || "Maintenance in progress — servers restart shortly.";
  try {
    if (on) {
      if (!confirm("Start maintenance mode? Launcher tile + chat broadcast go out now.")) return;
      await api("/api/tiles", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: "maintenance", type: "maintenance", section_size: "full", title: "Maintenance", body: msg, active: true }),
      });
      await api("/api/broadcast", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ channel: "dreadnought.global", content: "[Maintenance] " + msg }),
      });
      $("maint-state").textContent = "Maintenance ON since " + new Date().toLocaleTimeString() + ".";
      toast("Maintenance mode on.", "ok");
    } else {
      await api("/api/tiles/maintenance", { method: "DELETE" }).catch(() => {});
      await api("/api/broadcast", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ channel: "dreadnought.global", content: "[Maintenance] All clear — servers are back." }),
      });
      $("maint-state").textContent = "Maintenance ended " + new Date().toLocaleTimeString() + ".";
      toast("Maintenance mode off.", "ok");
    }
    if (state.currentTab === "news") loadTiles();
  } catch (e) { toast(e.message, "err"); }
}

/* ---------- setup & services (first run + control) ---------- */
let setupTimer = null;

async function loadSetup() {
  let st;
  try {
    st = await api("/api/setup-state");
  } catch (e) { toast("Setup: " + e.message, "err"); return; }
  const item = (ok, label, sub) =>
    `<dt>${ok ? "✅" : "⬜"}</dt><dd>${esc(label)}${sub ? ` <span class="muted-sm">${esc(sub)}</span>` : ""}</dd>`;
  $("setup-checklist").innerHTML = "<dl class=\"kv\">" + [
    item(st.secrets_env_exists, "run/secrets.env exists", st.secrets_env_exists ? "" : "write it below, then run setup"),
    item(st.jwt_set, "JWT_SECRET set", ""),
    item(st.admin_set, "ADMIN_KEY in secrets.env", "dashboard key source: " + (st.key_source || "?")),
    item(st.game_binary_exists, "GAME_BINARY", st.game_binary || "(unset — no battle servers without it)"),
    item(st.go_present, "Go toolchain", st.go_present ? "" : "install golang (or build elsewhere)"),
    item(st.wine_present, "Wine", st.wine_present ? "" : "optional — battle servers need it"),
  ].join("") + "</dl>";
  $("setup-banner").style.display = st.secrets_env_exists ? "none" : "";
  const tb = tbodyFor("svc-table");
  tb.innerHTML = "";
  const names = Object.keys(st.running || {}).sort();
  for (const name of names) {
    const up = st.running[name];
    const tr = document.createElement("tr");
    tr.innerHTML = `<td><code>${esc(name)}</code></td>
      <td>${up ? '<span class="badge ok">running</span>' : '<span class="badge warn">stopped</span>'}</td><td></td>`;
    const cell = tr.lastChild;
    if (up) {
      const b = document.createElement("button");
      b.className = "btn small danger";
      b.textContent = "Stop";
      b.onclick = () => confirmAction("Stop service?", `${name} gets SIGTERM (pidfile-checked).`, async () => {
        await api("/api/services/stop/" + name, { method: "POST" });
        toast(name + " stopped.", "ok");
        loadSetup();
      });
      cell.appendChild(b);
    } else {
      cell.innerHTML = '<span class="muted">Start all to start</span>';
    }
    tb.appendChild(tr);
  }
  $("setup-job").textContent = st.job_running ? ("running: " + st.job_running) : "";
  loadSetupLog(false);
}

async function runJob(path, label) {
  try {
    await api(path, { method: "POST" });
    toast(label + " started — watch the job log.", "ok");
    loadSetupLog(true);
  } catch (e) { toast(e.message, "err"); }
}

async function loadSetupLog(follow) {
  try {
    const data = await api("/api/setup-log?lines=200");
    const el = $("setup-log");
    el.textContent = (data.lines || []).join("\n") || "(empty — run setup or start services)";
    if (follow || ($("setup-follow") && $("setup-follow").checked)) el.scrollTop = el.scrollHeight;
    $("setup-job").textContent = data.running ? ("running: " + data.running) : "";
  } catch (e) { /* log optional */ }
}

async function loadSecrets() {
  try {
    const data = await api("/api/secrets");
    if (!loadSecrets.touched) $("secrets-text").value = data.content || "";
  } catch (e) { toast("Secrets: " + e.message, "err"); }
}

async function saveSecrets() {
  try {
    await api("/api/secrets", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ content: $("secrets-text").value }),
    });
    loadSecrets.touched = false;
    toast("secrets.env saved (mode 600). Services pick it up on (re)start.", "ok");
    loadSetup();
  } catch (e) { toast(e.message, "err"); }
}

/* ---------- polling ---------- */
async function refreshAll() {
  try { await api("/api/me"); hideLogin(); } catch { showLogin(); return; }
  // First run: no secrets.env yet — land on the Setup tab, not the overview.
  try {
    const st = await api("/api/setup-state");
    if (!st.secrets_env_exists) {
      switchTab("setup");
      toast("Welcome — no secrets.env yet. Work through Setup, then start everything.", "ok");
      return;
    }
  } catch (e) { /* setup-state optional; fall through */ }
  if (state.currentTab === "overview") { loadStatus(); loadSeries(); loadEconomyAndSessions(); }
  else switchTab(state.currentTab);
}

function tickClock() {
  $("clock").textContent = new Date().toLocaleTimeString();
}

document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("#tabs button").forEach((b) => b.onclick = () => switchTab(b.dataset.tab));
  $("login-btn").onclick = doLogin;
  $("login-key").addEventListener("keydown", (e) => { if (e.key === "Enter") doLogin(); });
  $("logout-btn").onclick = async () => { await api("/api/logout", { method: "POST" }).catch(() => {}); showLogin(); };
  $("accounts-reload").onclick = loadPlayers;
  $("accounts-search").oninput = renderPlayers;
  $("grant-all-btn").onclick = () => doGrantAll().catch((e) => toast(e.message, "err"));
  $("prov-btn").onclick = () => doProvision().catch((e) => toast(e.message, "err"));
  $("reset-btn").onclick = () => doReset().catch((e) => toast(e.message, "err"));
  $("online-reload").onclick = loadOnline;
  $("results-reload").onclick = loadResults;
  $("bc-send").onclick = () => sendBroadcast().catch((e) => toast(e.message, "err"));
  $("tiles-reload").onclick = loadTiles;
  $("tile-save").onclick = saveTile;
  $("queue-clear-btn").onclick = () => clearQueue().catch((e) => toast(e.message, "err"));
  $("force-match-btn").onclick = () => forceMatch().catch((e) => toast(e.message, "err"));
  $("backups-reload").onclick = loadBackups;
  $("match-select").onchange = (e) => showMatchDetail(e.target.value);
  $("match-reload").onclick = loadMatchesList;
  $("history-reload").onclick = loadHistory;
  $("catalog-reload").onclick = loadCatalog;
  $("catalog-search").oninput = renderCatalog;
  $("audit-reload").onclick = loadAudit;
  $("crash-reload").onclick = loadCrashes;
  $("crash-dir").onchange = showCrash;
  $("crash-file").onchange = () => {
    const v = $("crash-dir").value || "";
    if (!v.startsWith("d:")) return;
    const dir = v.slice(2), file = $("crash-file").value;
    if (!file) return;
    api(`/api/crashes?dir=${encodeURIComponent(dir)}&file=${encodeURIComponent(file)}&lines=200`)
      .then((d) => { $("crash-view").textContent = (d.lines || []).join("\n") || "(empty)"; })
      .catch((e) => { $("crash-view").textContent = e.message; });
  };
  $("sessions-reload").onclick = loadSessions;
  $("queue-reload").onclick = loadQueue;
  $("instances-reload").onclick = loadInstances;
  $("servers-reload").onclick = loadServers;
  $("chat-reload").onclick = loadChat;
  $("metrics-reload").onclick = loadMetrics;
  $("setup-reload").onclick = loadSetup;
  $("svc-start-all").onclick = () => runJob("/api/services/start-all", "Start all");
  $("svc-stop-all").onclick = () => confirmAction("Stop ALL services?", "The whole stack (not this dashboard) goes down.", () => runJob("/api/services/stop-all", "Stop all"));
  $("setup-run").onclick = () => confirmAction("Run setup?", "scripts/setup.sh builds everything (takes minutes).", () => runJob("/api/setup/run", "Setup"));
  $("setup-log-reload").onclick = () => loadSetupLog(true);
  $("secrets-reload").onclick = () => { loadSecrets.touched = false; loadSecrets(); };
  $("secrets-save").onclick = saveSecrets;
  $("secrets-text").oninput = () => { loadSecrets.touched = true; };
  setupTimer = setInterval(() => { if (state.currentTab === "setup") loadSetupLog(false); }, 5000);
  $("sleepers-reload").onclick = loadSleepers;
  $("sleepers-days").onchange = loadSleepers;
  $("reports-reload").onclick = loadReports;
  $("sla-reload").onclick = loadSLA;
  $("marker-add").onclick = async () => {
    const label = ($("marker-label").value || "").trim();
    if (!label) { toast("Label the marker first.", "err"); return; }
    try {
      await api("/api/events", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ label }),
      });
      $("marker-label").value = "";
      toast("Marker added.", "ok");
      loadMarkers();
      loadSeries();
    } catch (e) { toast(e.message, "err"); }
  };
  $("global-search").addEventListener("keydown", (e) => { if (e.key === "Enter") globalSearch(); });
  $("maint-start").onclick = () => setMaintenance(true);
  $("maint-stop").onclick = () => setMaintenance(false);
  $("accounts-csv").onclick = () => {
    const rows = [["username", "email", "player_id", "credits", "premium", "free_xp", "rank", "ships", "matches", "wins", "kills", "banned"]];
    for (const a of (accountsCache || [])) rows.push([a.username, a.email, a.player_id, a.credits, a.premium, a.free_xp, a.rank, a.ships, a.matches, a.wins, a.kills, a.banned]);
    downloadCSV("dreadnought-players.csv", rows);
    toast("Players CSV downloaded.", "ok");
  };
  $("history-csv").onclick = async () => {
    const data = await api("/api/history?limit=500").catch((e) => { toast(e.message, "err"); return null; });
    if (!data) return;
    const rows = [["id", "mode", "map", "started_at", "players"]];
    for (const m of (data.matches || [])) rows.push([m.id, m.mode, m.map, m.started_at, (m.players || []).length]);
    downloadCSV("dreadnought-matches.csv", rows);
    toast("Matches CSV downloaded.", "ok");
  };
  $("grant-preset-apply").onclick = () => {
    const list = grantPresets();
    const p = list[+$("grant-preset").value];
    if (!p) { toast("No preset selected.", "err"); return; }
    $("grant-all-credits").value = p.credits; $("grant-all-premium").value = p.premium; $("grant-all-xp").value = p.xp;
    toast(`Preset "${p.name}" filled in — press Give to all to run it.`, "ok");
  };
  $("grant-preset-save").onclick = () => {
    const name = prompt("Preset name?", "Event kit");
    if (!name || !name.trim()) return;
    const list = grantPresets();
    list.push({ name: name.trim(), credits: +$("grant-all-credits").value || 0, premium: +$("grant-all-premium").value || 0, xp: +$("grant-all-xp").value || 0 });
    saveGrantPresets(list);
    refreshPresetSelect();
    toast("Preset saved (this browser).", "ok");
  };
  $("grant-preset-del").onclick = () => {
    const list = grantPresets();
    const i = +$("grant-preset").value;
    if (!list[i]) { toast("No preset selected.", "err"); return; }
    if (!confirm(`Delete preset "${list[i].name}"?`)) return;
    list.splice(i, 1);
    saveGrantPresets(list);
    refreshPresetSelect();
  };
  refreshPresetSelect();
  document.querySelectorAll("#rangebar button").forEach((b) => { b.onclick = () => setRange(b.dataset.range); });
  tickClock();
  setInterval(tickClock, 1000);
  refreshAll();
  setInterval(() => { if (state.currentTab === "overview") loadSeries(); }, 60000);
  if (state.currentTab === "overview") loadSeries();
  $("log-reload").onclick = loadLog;
  $("log-source").onchange = () => { $("log-file").innerHTML = ""; loadLog(); };
  $("log-file").onchange = loadLog;
  $("log-filter").oninput = loadLog;
  $("grant-btn").onclick = () => doGrant().catch((e) => toast(e.message, "err"));
  $("ban-btn").onclick = () => {
    const u = $("ban-user").value.trim(), r = $("ban-reason").value.trim();
    confirmAction("Ban player?", `${u} — reason: ${r}`, async () => {
      await api("/api/ban", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username: u, reason: r }) });
      toast("Banned.", "ok");
    });
  };
  $("unban-btn").onclick = () => {
    const u = $("ban-user").value.trim();
    confirmAction("Unban?", u, async () => {
      await api("/api/unban", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username: u }) });
      toast("Unbanned.", "ok");
    });
  };
  $("modal-cancel").onclick = () => { $("modal").classList.add("hidden"); modalFn = null; };
  $("modal-ok").onclick = async () => {
    $("modal").classList.add("hidden");
    try { modalFn && await modalFn(); } catch (e) { toast(e.message, "err"); }
    modalFn = null;
  };
  tickClock();
  setInterval(tickClock, 1000);
  refreshAll();
  setInterval(() => { if (state.currentTab === "overview") loadStatus(); }, 5000);
  state.logTimer = setInterval(() => {
    if (state.currentTab === "logs" && $("log-follow").checked) loadLog();
  }, 5000);
});
