/* website-stats dashboard (vanilla JS + uPlot) */
"use strict";

const $ = (id) => document.getElementById(id);

// HTML-escape untrusted strings (request paths and client IPs from access
// logs are attacker-controlled) before inserting them into templates.
const esc = (s) => String(s).replace(/[&<>"']/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
const state = {
  range: "24h",
  view: "overview",
  site: null,
  palette: {},
  timers: [],
  chartView: "areas",
  lastOverview: null,
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
    // golden-angle hue steps plus rotating lightness/saturation keep
    // successive colors far apart even with dozens of sites
    const l = [0.55, 0.40, 0.68, 0.48, 0.62, 0.35][i % 6];
    const sat = i % 2 ? 0.70 : 0.55;
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
  const el = $("loginError");
  const showErr = (msg) => { el.textContent = msg; el.classList.remove("hidden"); };
  if ($("loginPassword").dataset.mode === "setup") {
    const pw = $("loginPassword").value;
    if (pw.length < 8) return showErr("Password must be at least 8 characters.");
    const res = await fetch("/api/setup-password", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password: pw }),
    });
    if (!res.ok) {
      const j = await res.json().catch(() => ({}));
      return showErr(j.error || "setup failed");
    }
    delete $("loginPassword").dataset.mode;
    const err = await tryLogin(pw);
    if (err) return showErr(err);
    return;
  }
  const err = await tryLogin($("loginPassword").value);
  if (err) showErr(err);
});
/* ---------- profile menu & password ---------- */

$("profileBtn").addEventListener("click", (e) => {
  e.stopPropagation();
  $("profileDropdown").classList.toggle("hidden");
});
document.addEventListener("click", (e) => {
  if (!e.target.closest(".profile-menu")) $("profileDropdown").classList.add("hidden");
});
$("signoutBtn").addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST" });
  $("profileDropdown").classList.add("hidden");
  showLogin();
});

function showPwModal(mode) {
  $("pwModal").classList.remove("hidden");
  $("pwError").classList.add("hidden");
  $("pwCurrent").classList.toggle("hidden", mode === "setup");
  $("pwCurrent").value = "";
  $("pwNew").value = "";
  $("pwConfirm").value = "";
  $("pwModal").dataset.mode = mode;
  $("pwNew").focus();
}
$("changePwBtn").addEventListener("click", () => {
  $("profileDropdown").classList.add("hidden");
  showPwModal("change");
});
$("pwModal").addEventListener("click", (e) => {
  if (e.target === e.currentTarget) $("pwModal").classList.add("hidden");
});
$("pwForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = (msg) => { $("pwError").textContent = msg; $("pwError").classList.remove("hidden"); };
  const mode = $("pwModal").dataset.mode;
  const nw = $("pwNew").value, cf = $("pwConfirm").value;
  if (nw.length < 8) return err("New password must be at least 8 characters.");
  if (nw !== cf) return err("Passwords do not match.");
  const url = mode === "setup" ? "/api/setup-password" : "/api/password";
  const body = mode === "setup" ? { password: nw } : { current: $("pwCurrent").value, new: nw };
  const res = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    return err(j.error || "failed");
  }
  $("pwModal").classList.add("hidden");
});

/* ---------- settings page ---------- */

function settingsErr(msg) {
  const el = $("settingsError");
  el.textContent = msg;
  el.classList.remove("hidden");
  $("settingsSaved").classList.add("hidden");
}

function loadSettings() {
  api("/api/settings").then((s) => {
    $("setEmailEnabled").checked = s.email.enabled;
    $("setSmtpHost").value = s.email.smtpHost || "";
    $("setSmtpUser").value = s.email.username || "";
    $("setSmtpPass").value = "";
    $("setSmtpPass").placeholder = s.email.hasPassword ? "(unchanged)" : "";
    $("setEmailFrom").value = s.email.from || "";
    $("setEmailTo").value = (s.email.to || []).join(", ");
    $("setWebhookEnabled").checked = s.webhook.enabled;
    $("setWebhookUrl").value = s.webhook.url || "";
    const tbody = $("rulesTable").querySelector("tbody");
    tbody.innerHTML = "";
    draftRules = s.rules || [];
    for (const r of draftRules) tbody.appendChild(ruleRow(r));
    if (!draftRules.length) tbody.appendChild(ruleRow({}));
      }).catch((e) => settingsErr("could not load settings: " + e.message));
}

$("settingsBtn").addEventListener("click", () => { location.hash = "#/settings"; });
$("settingsBackLink").addEventListener("click", (e) => { e.preventDefault(); location.hash = ""; });
$("addRuleBtn").addEventListener("click", () => {
  $("rulesTable").querySelector("tbody").appendChild(ruleRow({}));
});

const METRICS = [
  ["site.cpu", "Site CPU %"],
  ["site.mem", "Site memory (MB)"],
  ["site.mem_pct", "Site memory (% of total)"],
  ["site.req", "Site requests/min"],
  ["site.err5", "Site 5xx/min"],
  ["system.cpu", "System CPU %"],
  ["system.load1", "System load (1 min)"],
  ["system.load5", "System load (5 min)"],
  ["system.load15", "System load (15 min)"],
  ["system.mem_pct", "System RAM used (% of total)"],
];

let draftRules = [];

function ruleRow(r) {
  const tr = document.createElement("tr");
  tr.innerHTML = `
    <td><input type="text" class="r-name" value="${esc(r.name || "")}" placeholder="my-rule"></td>
    <td><select class="r-metric">${METRICS.map(([v, l]) => `<option value="${v}"${v === r.metric ? " selected" : ""}>${l}</option>`).join("")}</select></td>
    <td><input type="text" class="r-site" value="${esc(r.site || "*")}" title="Site user, * = all sites"></td>
    <td><select class="r-op">${[">", ">=", "<", "<="].map((o) => `<option${o === (r.op || ">") ? " selected" : ""}>${o}</option>`).join("")}</select></td>
    <td><input type="number" class="r-value" value="${r.value ?? 0}" step="any"></td>
    <td><input type="text" class="r-for" value="${esc(r.for || "5m")}" title="Breach duration before firing, e.g. 5m"></td>
    <td><input type="text" class="r-cooldown" value="${esc(r.cooldown || "1h")}" title="Minimum time between notifications"></td>
    <td><select class="r-sev"><option value="warning"${r.severity !== "critical" ? " selected" : ""}>warning</option><option value="critical"${r.severity === "critical" ? " selected" : ""}>critical</option></select></td>
    <td><button type="button" class="r-del" title="Delete rule">✕</button></td>`;
  tr.querySelector(".r-del").addEventListener("click", () => tr.remove());
  return tr;
}

function settingsErr(msg) {
  const el = $("settingsError");
  el.textContent = msg;
  el.classList.remove("hidden");
}

function collectRules() {
  return [...$("rulesTable").querySelectorAll("tbody tr")].map((tr) => ({
    name: tr.querySelector(".r-name").value,
    metric: tr.querySelector(".r-metric").value,
    site: tr.querySelector(".r-site").value,
    op: tr.querySelector(".r-op").value,
    value: parseFloat(tr.querySelector(".r-value").value) || 0,
    for: tr.querySelector(".r-for").value,
    cooldown: tr.querySelector(".r-cooldown").value,
    severity: tr.querySelector(".r-sev").value,
  }));
}

function settingsPayload() {
  return {
    email: {
      enabled: $("setEmailEnabled").checked,
      smtpHost: $("setSmtpHost").value,
      username: $("setSmtpUser").value,
      password: $("setSmtpPass").value,
      from: $("setEmailFrom").value,
      to: $("setEmailTo").value.split(",").map((s) => s.trim()).filter(Boolean),
    },
    webhook: {
      enabled: $("setWebhookEnabled").checked,
      url: $("setWebhookUrl").value,
    },
    rules: collectRules(),
  };
}

$("settingsForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("settingsError").classList.add("hidden");
  $("settingsSaved").classList.add("hidden");
  const res = await fetch("/api/settings", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(settingsPayload()),
  });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    return settingsErr(j.error || "saving failed");
  }
  $("settingsSaved").classList.remove("hidden");
  loadSettings(); // refresh placeholders (e.g. hasPassword)
});

$("testMailBtn").addEventListener("click", async () => {
  const st = $("testMailStatus");
  st.textContent = "sending…";
  const body = settingsPayload();
  body.rules = []; // not needed for the mail probe
  const res = await fetch("/api/settings/test-email", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    st.textContent = j.error || "test failed";
    return;
  }
  st.textContent = "test email sent ✓";
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

// Builds bottom-up stacked bands from site entries: top sites as their own
// band (largest at the bottom), everything else pooled into "Others".
function buildBands(entries) {
  const totalOf = (e) => e.series.reduce((a, p) => a + p[1], 0);
  entries = [...entries].sort((a, b) => totalOf(b) - totalOf(a));
  const aligned = alignSeries(entries.map((e) => e.series));
  const xs = aligned.xs.map((t) => t * 1000);
  // every site gets its own band, largest at the bottom
  const idxOf = new Map(aligned.xs.map((t, i) => [t, i]));
  const bands = entries.map((e) => {
    const row = new Array(aligned.xs.length).fill(null);
    for (const [t, v] of e.series) row[idxOf.get(t)] = v;
    return { name: e.name, color: e.color, fill: e.fill, row };
  });
  return { xs, bands };
}

// Stacked areas: each band is drawn as the cumulative sum up to that site,
// opaque fill, drawn top-down so every band keeps its own color.
function stackedChart(el, entries) {
  el.innerHTML = "";
  const aligned = alignSeries(entries.map((e) => e.series));
  const xs = aligned.xs.map((t) => t * 1000);
  const built = buildBands(entries);
  const n = built.bands.length;

  // hover tooltip: report the band (site) under the cursor
  el.style.position = "relative";
  el.innerHTML = "";
  const tip = document.createElement("div");
  tip.className = "chart-tip";
  el.appendChild(tip);

  // cumulative rows: band i's line = sum of bands 0..i (bottom-up)
  const cum = [];
  for (let b = 0; b < n; b++) {
    const row = new Array(xs.length).fill(null);
    for (let i = 0; i < xs.length; i++) {
      let sum = 0, has = false;
      for (let k = 0; k <= b; k++) {
        const v = built.bands[k].row[i];
        if (v != null) { sum += v; has = true; }
      }
      row[i] = has ? sum : null;
    }
    cum.push(row);
  }
  let maxStack = 0;
  for (const v of cum[n - 1] || []) if (v != null && v > maxStack) maxStack = v;
  const softMax = Math.max(10, Math.ceil(maxStack * 1.15));

  // draw top-down (largest cumulative first) so the bottom band stays visible;
  // each series must be paired with its own cumulative row
  const data = [xs];
  const series = [null];
  for (let b = n - 1; b >= 0; b--) {
    data.push(cum[b]);
    series.push({
      label: built.bands[b].name,
      stroke: built.bands[b].color,
      fill: built.bands[b].color,
      width: 1,
      points: { show: false },
      spanGaps: true,
      // legend/tooltip value: the band's own share, not its cumulative
      // upper boundary (series k's lower boundary is series k+1)
      value: (u2, v, si, di) => {
        if (v == null) return "–";
        const lower = si < u2.data.length - 1 ? (u2.data[si + 1][di] ?? 0) : 0;
        return Math.max(0, v - lower).toFixed(2) + "%";
      },
    });
  }
  const opts = {
    ...chartDefaults,
    width: chartWidth(el),
    height: 280,
    scales: { x: { time: true }, y: { auto: false, range: () => [0, softMax] } },
    series,
    hooks: {
      setCursor: [(u) => {
        const { left, top } = u.cursor;
        if (left == null || left < 0 || top == null || top < 0) {
          tip.style.opacity = 0;
          return;
        }
        const idx = u.posToIdx(left);
        const yv = u.posToVal(top, "y");
        // data[1..n] are the cumulative rows, pushed top band first, so
        // series k corresponds to band n-k; the band under yv is the one
        // whose region [cum of band b-1 .. cum of band b] contains yv —
        // i.e. the LOWEST cumulative line still >= yv
        let k = 0;
        for (let s = n; s >= 1; s--) {
          const cum = data[s][idx];
          if (cum != null && cum >= yv) { k = s; break; }
        }
        if (!k) { tip.style.opacity = 0; return; }
        const lower = k < n ? (data[k + 1][idx] ?? 0) : 0;
        const val = (data[k][idx] ?? 0) - lower;
        const band = built.bands[n - k];
        tip.textContent = `${band.name}: ${val.toFixed(1)}%`;
        tip.style.left = Math.min(left + 14, u.width - 140) + "px";
        tip.style.top = Math.max(top - 12, 0) + "px";
        tip.style.opacity = 1;
      }],
    },
  };
  const up = new uPlot(opts, data, el);
  settleSize(up, el, 280);
  return up;
}

// ---------- infrared palette (thermal camera look) ----------
const IR_STOPS = [
  [0.0, [1, 1, 8]],
  [0.2, [41, 12, 106]],
  [0.45, [172, 20, 88]],
  [0.7, [240, 100, 40]],
  [0.9, [252, 180, 25]],
  [1.0, [255, 245, 200]],
];
function infrared(t) {
  t = Math.max(0, Math.min(1, t));
  for (let i = 1; i < IR_STOPS.length; i++) {
    if (t <= IR_STOPS[i][0]) {
      const [t0, c0] = IR_STOPS[i - 1];
      const [t1, c1] = IR_STOPS[i];
      const f = (t - t0) / (t1 - t0);
      return c0.map((v, k) => Math.round(v + (c1[k] - v) * f));
    }
  }
  return IR_STOPS[IR_STOPS.length - 1][1];
}

// Heatmap view: one row per site, columns = time buckets, color = infrared
// intensity of the peak CPU in that bucket. Rows sorted by total, hottest
// site on top.
function drawHeatmap(el, entries) {
  el.innerHTML = "";
  const COLS = 28;
  const xsSet = new Set();
  for (const e of entries) for (const [t] of e.series) xsSet.add(t);
  const xs = [...xsSet].sort((a, b) => a - b);
  if (!xs.length) { el.innerHTML = '<p class="hint" style="padding:20px">no data yet</p>'; return; }
  const colOf = new Map(xs.map((t, i) => [t, Math.min(COLS - 1, Math.floor((i / xs.length) * COLS))]));

  const rows = entries.map((e) => {
    const cells = new Array(COLS).fill(null);
    for (const [t, v] of e.series) {
      const c = colOf.get(t);
      cells[c] = Math.max(cells[c] ?? 0, v);
    }
    return { name: e.name, color: e.color, cells, total: cells.reduce((a, b) => a + (b || 0), 0) };
  }).sort((a, b) => b.total - a.total);
  const gMax = Math.max(...rows.flatMap((r) => r.cells.filter((c) => c != null)), 0.001);

  const grid = document.createElement("div");
  grid.style.cssText = "display:grid;grid-template-columns:200px repeat(" + COLS + ",1fr);gap:3px;margin-top:4px";
  grid.appendChild(document.createElement("div"));
  for (let cI = 0; cI < COLS; cI++) {
    const h = document.createElement("div");
    h.style.cssText = "font-size:10px;color:var(--muted);text-align:center";
    const t = xs[Math.min(xs.length - 1, Math.floor(((cI + 0.5) / COLS) * xs.length))];
    h.textContent = new Date(t * 1000).getHours() + "h";
    grid.appendChild(h);
  }
  for (const row of rows) {
    const lbl = document.createElement("div");
    lbl.style.cssText = "font-size:11.5px;color:var(--muted);white-space:nowrap;overflow:hidden;text-overflow:ellipsis;text-align:right;padding-right:8px;align-self:center";
    lbl.textContent = row.name; lbl.title = row.name;
    grid.appendChild(lbl);
    for (let cI = 0; cI < COLS; cI++) {
      const v = row.cells[cI];
      const cell = document.createElement("div");
      cell.style.height = "17px";
      cell.style.borderRadius = "3px";
      if (v == null) {
        cell.style.background = "var(--panel2)";
      } else {
        const [r, g, b] = infrared(v / gMax);
        cell.style.background = `rgb(${r},${g},${b})`;
        cell.title = row.name + " — " + v.toFixed(2) + "%";
      }
      grid.appendChild(cell);
    }
  }
  el.appendChild(grid);
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

function renderViewToggle() {
  const el = $("viewToggle");
  el.innerHTML = "";
  for (const [v, label] of [["areas", "Areas"], ["heat", "Heatmap"]]) {
    const b = document.createElement("button");
    b.textContent = label;
    if (state.chartView === v) b.classList.add("active");
    b.addEventListener("click", () => {
      state.chartView = v;
      if (state.lastOverview) renderOverview(state.lastOverview);
    });
    el.appendChild(b);
  }
}

function renderOverview(d) {
  state.lastOverview = d;
  renderChips(d.now);
  renderRanges($("ranges"), state.range, (r) => { state.range = r; route(); });
  renderViewToggle();

  const entries = d.sites
    .filter((s) => s.series && s.series.length)
    .map((s) => {
      const c = colorFor(s.name);
      return { name: s.name === "_sys" ? "System services" : s.name, color: c.css, fill: c.fill, series: s.series };
    });
  const el = $("mainChart");
  if (!entries.length) {
    el.innerHTML = '<p class="hint" style="padding:20px">collecting data… first chart available ~1 minute after start</p>';
  } else if (state.chartView === "heat") {
    drawHeatmap(el, entries);
  } else {
    stackedChart(el, entries);
  }

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
          <span class="site-name">${esc(isSys ? "System services" : s.name)}</span>
          ${isSys ? "" : `<span class="site-domain"> ${esc(s.domain)}</span>`}</td>
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
    ? d.events.map((e) => `<li><span class="when">${fmtTs(e.ts)}</span><span class="sev ${esc(e.severity || "warning")}">${esc(e.severity || "warning")}</span><span><b>${esc(e.site)}</b> — ${esc(e.message)}</span></li>`).join("")
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
  return items.map((it) => `<li><span class="k" title="${esc(it.value)}">${esc(it.value)}</span><span class="v">${fmt(it)}</span></li>`).join("");
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
    ? d.events.map((e) => `<li><span class="when">${fmtTs(e.ts)}</span><span class="sev ${esc(e.severity || "warning")}">${esc(e.severity || "warning")}</span><span>${esc(e.message)}</span></li>`).join("")
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
  $("overview").classList.add("hidden");
  $("siteview").classList.add("hidden");
  $("settingsview").classList.add("hidden");
  if (m) {
    state.view = "site";
    state.site = decodeURIComponent(m[1]);
    $("siteview").classList.remove("hidden");
    every(15000, () => loadSite(state.site).catch(() => {}));
  } else if (location.hash === "#/settings") {
    state.view = "settings";
    $("settingsSaved").classList.add("hidden");
    $("settingsview").classList.remove("hidden");
    loadSettings();
  } else {
    state.view = "overview";
    $("overview").classList.remove("hidden");
    every(10000, () => loadOverview().catch(() => {}));
  }
  window.scrollTo(0, 0);
}
window.addEventListener("hashchange", route);
$("backLink").addEventListener("click", (e) => { e.preventDefault(); location.hash = ""; });

/* ---------- boot ---------- */

// First-time setup: no password configured yet → the login card becomes a
// "choose a password" form.
function showSetupMode() {
  showLogin();
  document.querySelector("#loginForm h1").textContent = "First-time setup";
  document.querySelector("#loginForm p").textContent = "Choose a password for this dashboard.";
  $("loginPassword").placeholder = "Choose a password (min. 8 characters)";
  $("loginPassword").dataset.mode = "setup";
  $("loginPassword").focus();
}

(async function boot() {
  try {
    const st = await fetch("/api/setup-status").then((r) => r.json());
    if (st && st.configured === false) { showSetupMode(); return; }
  } catch (e) { /* fall through to normal login */ }
  try {
    const meta = await api("/api/meta");
    $("ver").textContent = "v" + meta.version;
    route();
  } catch (e) {
    // 401 → login overlay shown by api(); keep waiting
  }
})();
