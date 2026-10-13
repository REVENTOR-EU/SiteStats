/* website-stats dashboard (vanilla JS + uPlot) */
"use strict";

const $ = (id) => document.getElementById(id);
const state = {
  range: "24h",
  view: "overview",
  site: null,
  palette: {},
  timers: [],
};

/* ---------- helpers ---------- */

function colorFor(key) {
  if (!state.palette[key]) {
    const i = Object.keys(state.palette).length;
    const hue = ((i * 137.508) % 360) / 360;
    // hsl(hue, 62%, 55%) -> rgb (avoids space-syntax hsl() strings some
    // canvases reject)
    const hue2rgb = (p, q, t) => {
      if (t < 0) t += 1;
      if (t > 1) t -= 1;
      if (t < 1 / 6) return p + (q - p) * 6 * t;
      if (t < 1 / 2) return q;
      if (t < 2 / 3) return p + (q - p) * (2 / 3 - t) * 6;
      return p;
    };
    const l = 0.55, sat = 0.62;
    const q = l < 0.5 ? l * (1 + sat) : l + sat - l * sat;
    const pp = 2 * l - q;
    const r = Math.round(hue2rgb(pp, q, hue + 1 / 3) * 255);
    const g = Math.round(hue2rgb(pp, q, hue) * 255);
    const b = Math.round(hue2rgb(pp, q, hue - 1 / 3) * 255);
    const hex = "#" + [r, g, b].map((v) => v.toString(16).padStart(2, "0")).join("");
    state.palette[key] = { css: hex, fill: `rgba(${r},${g},${b},0.82)` };
  }
  return state.palette[key];
}

function fmtBytes(n, perSec) {
  if (n == null || isNaN(n)) return "–";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}${perSec ? "/s" : ""}`;
}
function fmtInt(n) {
  if (n == null || isNaN(n)) return "–";
  return Math.round(n).toLocaleString("en-US");
}
function fmtPct(n) {
  if (n == null || isNaN(n)) return "–";
  return `${n.toFixed(n < 10 ? 1 : 0)}%`;
}
function fmtTs(ts) {
  return new Date(ts * 1000).toLocaleString("en-GB", { day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit" });
}

async function api(path) {
  const res = await fetch(path, { headers: { "Accept": "application/json" } });
  if (res.status === 401) { showLogin(); throw new Error("unauthorized"); }
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return res.json();
}

function clearTimers() {
  state.timers.forEach(clearInterval);
  state.timers = [];
}
function every(ms, fn) {
  fn();
  state.timers.push(setInterval(fn, ms));
}

/* ---------- login ---------- */

function showLogin() { $("login").classList.remove("hidden"); }
async function tryLogin(password) {
  const res = await fetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
  });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    return j.error || "login failed";
  }
  $("login").classList.add("hidden");
  route();
  return null;
}
$("loginForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = await tryLogin($("loginPassword").value);
  const el = $("loginError");
  if (err) { el.textContent = err; el.classList.remove("hidden"); }
});
$("logoutBtn").addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST" });
  showLogin();
});

/* ---------- charts ---------- */

const chartDefaults = {
  width: 600,
  height: 240,
  ms: true,
  scales: { x: { time: true } },
};

function chartWidth(el) {
  const w = el.clientWidth || el.getBoundingClientRect().width;
  return Math.max(200, Math.floor(w) || 600);
}

// settleSize re-measures after layout settles (elements can be measured while
// still hidden right after a view switch).
function settleSize(up, el, height) {
  requestAnimationFrame(() => up.setSize({ width: chartWidth(el), height }));
}

// alignSeries: [{series:[[ts,v],...]}] -> {xs:[ts], cols:[[..aligned..]]}
function alignSeries(allSeries) {
  const xsSet = new Set();
  for (const s of allSeries) if (s) for (const [t] of s) xsSet.add(t);
  const xs = [...xsSet].sort((a, b) => a - b);
  const idx = new Map(xs.map((t, i) => [t, i]));
  const cols = allSeries.map((s) => {
    const out = new Array(xs.length).fill(null);
    if (s) for (const [t, v] of s) out[idx.get(t)] = v;
    return out;
  });
  return { xs, cols };
}

function stackedChart(el, entries) {
  el.innerHTML = "";
  // entries: [{name, color, fill, series: [[ts,v],...]}] sorted desc by magnitude
  const aligned = alignSeries(entries.map((e) => e.series));
  const xs = aligned.xs.map((t) => t * 1000);
  let maxV = 0;
  for (const e of entries) for (const [, v] of e.series) if (v > maxV) maxV = v;
  const softMax = Math.max(10, Math.ceil(maxV * 1.25));
  const series = [null];
  for (const e of entries) {
    series.push({
      label: e.name,
      stroke: e.color,
      width: 1.4,
      points: { show: false },
      spanGaps: true,
      fill: e.fill,
    });
  }
  const cols = aligned.cols;
  const opts = {
    width: chartWidth(el),
    height: 260,
    ms: true,
    scales: { x: { time: true } },
    series,
  };
  const up = new uPlot(opts, [xs, ...cols], el);
  settleSize(up, el, 260);
  return up;
}

function lineChart(el, defs, height = 190, yFmt = (v) => v) {
  el.innerHTML = "";
  const aligned = alignSeries(defs.map((d) => d.series));
  const xs = aligned.xs.map((t) => t * 1000);
  const series = [null];
  for (const d of defs) {
    series.push({
      label: d.name,
      stroke: d.color || "#4c8dff",
      width: 1.6,
      points: { show: false },
      spanGaps: true,
    });
  }
  const opts = {
    width: chartWidth(el),
    height,
    ms: true,
    scales: { x: { time: true } },
    series,
  };
  const up = new uPlot(opts, [xs, ...aligned.cols], el);
  settleSize(up, el, height);
  return up;
}


/* ---------- overview ---------- */

function renderChips(now) {
  const el = $("chips");
  if (!now) { el.innerHTML = ""; return; }
  const memPct = now.memTotal ? (now.memUsed / now.memTotal) * 100 : 0;
  const chips = [
    { label: "CPU", val: fmtPct(now.cpu), cls: now.cpu > 80 ? "crit" : now.cpu > 60 ? "warn" : "" },
    { label: "RAM", val: `${fmtBytes(now.memUsed * 1048576)} / ${fmtBytes(now.memTotal * 1048576)}`, cls: memPct > 90 ? "warn" : "" },
    { label: "Load", val: (now.load1 ?? 0).toFixed(2) },
    { label: "Net", val: `↓ ${fmtBytes(now.netRx, true)} ↑ ${fmtBytes(now.netTx, true)}` },
    { label: "Disk", val: `${(now.diskUsed ?? 0).toFixed(0)} / ${(now.diskTotal ?? 0).toFixed(0)} GB` },
  ];
  el.innerHTML = chips
    .map((c) => `<span class="chip ${c.cls}" title="${c.label}">${c.label} <b>${c.val}</b></span>`)
    .join("");
}

function renderRanges(el, current, cb) {
  el.innerHTML = "";
  for (const r of ["1h", "6h", "24h", "7d", "30d"]) {
    const b = document.createElement("button");
    b.textContent = r;
    if (r === current) b.classList.add("active");
    b.addEventListener("click", () => cb(r));
    el.appendChild(b);
  }
}

function sparkline(series, color) {
  if (!series || series.length < 2) return '<svg width="130" height="30"></svg>';
  const tail = series.slice(-288);
  const w = 130, h = 30;
  const maxV = Math.max(1, ...tail.map((p) => p[1]));
  const t0 = tail[0][0], t1 = tail[tail.length - 1][0];
  const pts = tail
    .map(([t, v]) => `${((t - t0) / Math.max(1, t1 - t0)) * w},${h - (v / maxV) * (h - 4) - 2}`)
    .join(" ");
  return `<svg width="${w}" height="${h}"><polyline points="${pts}" fill="none" stroke="${color}" stroke-width="1.4"/></svg>`;
}

function renderOverview(d) {
  renderChips(d.now);
  renderRanges($("ranges"), state.range, (r) => { state.range = r; route(); });

  const entries = d.sites
    .filter((s) => s.series && s.series.length)
    .map((s) => {
      const c = colorFor(s.name);
      return { name: s.name === "_sys" ? "System services" : s.name, color: c.css, fill: c.fill, series: s.series };
    });
  const el = $("mainChart");
  if (entries.length) stackedChart(el, entries);
  else el.innerHTML = '<p class="hint" style="padding:20px">collecting data… first chart available ~1 minute after start</p>';

  const tbody = $("siteTable").querySelector("tbody");
  tbody.innerHTML = "";
  for (const s of d.sites) {
    const c = colorFor(s.name);
    const tr = document.createElement("tr");
    tr.className = "site-row";
    const isSys = s.name === "_sys";
    const barCol = s.cpu > 80 ? "var(--red)" : s.cpu > 50 ? "var(--yellow)" : "var(--green)";
    tr.innerHTML = `
      <td><span class="swatch" style="background:${c.css}"></span>
          <span class="site-name">${isSys ? "System services" : s.name}</span>
          ${isSys ? "" : `<span class="site-domain"> ${s.domain}</span>`}</td>
      <td class="num">${fmtPct(s.cpu)}<span class="cpubar"><span style="width:${Math.min(100, s.cpu)}%;background:${barCol}"></span></span></td>
      <td class="num">${fmtBytes(s.mem * 1048576)}</td>
      <td class="num">${fmtInt(s.req)}</td>
      <td class="num">${s.r5 > 0 ? `<span style="color:var(--red)">${fmtInt(s.r5)}</span>` : fmtInt(0)}</td>
      <td class="num">${fmtInt(s.procs)}</td>
      <td class="num">${s.fpm ? `${s.fpm.active} / ${s.fpm.listenQueue}` : "–"}</td>
      <td>${isSys ? "" : sparkline(s.series, c.css)}</td>`;
    if (!isSys) tr.addEventListener("click", () => { location.hash = "#/site/" + s.name; });
    tbody.appendChild(tr);
  }

  const list = $("eventsList");
  list.innerHTML = d.events.length
    ? d.events.map((e) => `<li><span class="when">${fmtTs(e.ts)}</span><span class="sev ${e.severity || "warning"}">${e.severity || "warning"}</span><span><b>${e.site}</b> — ${e.message}</span></li>`).join("")
    : '<li class="empty">no alerts fired yet</li>';
}

function loadOverview() {
  renderRanges($("ranges"), state.range, (r) => { state.range = r; route(); });
  return api(`/api/overview?range=${state.range}`).then(renderOverview);
}

/* ---------- site detail ---------- */

function statCard(label, value, cls) {
  return `<div class="stat"><div class="label">${label}</div><div class="value ${cls || ""}">${value}</div></div>`;
}

function kvList(items, fmt) {
  if (!items || !items.length) return '<li class="empty">no data yet</li>';
  return items.map((it) => `<li><span class="k" title="${it.value}">${it.value}</span><span class="v">${fmt(it)}</span></li>`).join("");
}

function renderSite(d) {
  $("siteTitle").textContent = `${d.name} (${d.domain})`;
  const L = d.latest || {};
  const fpm = d.fpm;
  $("siteCards").innerHTML =
    statCard("CPU now", fmtPct(L.cpu), L.cpu > 40 ? "crit" : "") +
    statCard("Memory", fmtBytes(L.mem * 1048576)) +
    statCard("req/min", fmtInt(L.req)) +
    statCard("4xx / 5xx per min", `${fmtInt(L.r4)} / ${fmtInt(L.r5)}`, L.r5 > 0 ? "warn" : "") +
    statCard("Processes", fmtInt(L.procs)) +
    (fpm ? statCard("FPM active / queue", `${fpm.active} / ${fpm.listenQueue}`, fpm.listenQueue > 0 ? "warn" : "") : "") +
    (d.disk.length ? statCard("Disk (home)", fmtBytes(d.disk[d.disk.length - 1][1])) : "");

  const has = (k) => (d.series[k] && d.series[k].length ? d.series[k] : null);
  const single = (k, color) => (has(k) ? [{ name: k, color, series: d.series[k] }] : null);
  if (has("cpu")) lineChart($("cpuChart"), single("cpu", "#4c8dff"), 190, (v) => v.toFixed(0) + "%");
  if (has("mem")) lineChart($("memChart"), single("mem", "#3fb27f"), 190, (v) => fmtBytes(v * 1048576));
  if (has("req")) lineChart($("reqChart"), single("req", "#b585f5"), 190, (v) => fmtInt(v));
  if (has("bytes")) lineChart($("bytesChart"), single("bytes", "#d4a72c"), 190, (v) => fmtBytes(v) + "/m");
  const stDefs = [];
  if (has("r4")) stDefs.push({ name: "4xx", color: "#d4a72c", series: d.series.r4 });
  if (has("r5")) stDefs.push({ name: "5xx", color: "#e5534b", series: d.series.r5 });
  if (stDefs.length) lineChart($("statusChart"), stDefs, 190, (v) => fmtInt(v));
  if (has("fpmActive")) lineChart($("fpmChart"), single("fpmActive", "#3fb27f"), 190, (v) => fmtInt(v));

  $("procList").innerHTML = kvList(
    (d.procs || []).map((p) => ({ value: `${p.name} (pid ${p.pid})`, cpu: p.cpu, mem: p.memMb })),
    (it) => `${fmtPct(it.cpu)} · ${fmtBytes(it.mem * 1048576)}`
  );
  $("urlList").innerHTML = kvList(
    (d.topUrls || []).map((u) => ({ value: u.value, count: u.count })),
    (it) => `${fmtInt(it.count)} req`
  );
  $("ipList").innerHTML = kvList(
    (d.topIps || []).map((u) => ({ value: u.value, count: u.count, bytes: u.bytes })),
    (it) => `${fmtInt(it.count)} · ${fmtBytes(it.bytes)}`
  );
  $("diskChart").innerHTML = d.disk.length ? "" : '<p class="hint" style="padding:10px">first reading available within the hour after start</p>';
  if (d.disk.length) lineChart($("diskChart"), [{ name: "disk", color: "#4c8dff", series: d.disk }], 170, (v) => fmtBytes(v));

  $("siteEvents").innerHTML = d.events.length
    ? d.events.map((e) => `<li><span class="when">${fmtTs(e.ts)}</span><span class="sev ${e.severity || "warning"}">${e.severity || "warning"}</span><span>${e.message}</span></li>`).join("")
    : '<li class="empty">no alerts for this site</li>';
}

function loadSite(name) {
  renderRanges($("siteRanges"), state.range, (r) => { state.range = r; route(); });
  return api(`/api/site/${encodeURIComponent(name)}?range=${state.range}`).then(renderSite);
}

/* ---------- routing ---------- */

function route() {
  clearTimers();
  const m = location.hash.match(/^#\/site\/(.+)$/);
  if (m) {
    state.view = "site";
    state.site = decodeURIComponent(m[1]);
    $("overview").classList.add("hidden");
    $("siteview").classList.remove("hidden");
    every(15000, () => loadSite(state.site).catch(() => {}));
  } else {
    state.view = "overview";
    state.site = null;
    $("siteview").classList.add("hidden");
    $("overview").classList.remove("hidden");
    every(10000, () => loadOverview().catch(() => {}));
  }
  window.scrollTo(0, 0);
}
window.addEventListener("hashchange", route);
$("backLink").addEventListener("click", (e) => { e.preventDefault(); location.hash = ""; });

/* ---------- boot ---------- */

(async function boot() {
  try {
    const meta = await api("/api/meta");
    $("ver").textContent = "v" + meta.version;
    route();
  } catch (e) {
    // 401 → login overlay shown by api(); keep waiting
  }
})();
