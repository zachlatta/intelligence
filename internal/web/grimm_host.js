// Live host resource overview widget in the Grimm page header's upper right.
// Reads the grimm_host_resources view via /api/sql on load and whenever the Grimm SSE
// stream emits a host_resources event (woken by Postgres NOTIFY on host reports).
"use strict";
(() => {
const root = document.querySelector("#grimm");
const slot = document.querySelector("#g-host");
if (!root || !slot) return;

const sessionId = root.dataset.session || "";
if (!/^[0-9a-f-]{36}$/i.test(sessionId)) return;

const STALE_SEC = 180;
let current = null;
let pinned = false;
let hoverOpen = false;
let closeTimer = null;
let inflight = false;
let pendingRefresh = false;

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function num(v) { return typeof v === "number" && Number.isFinite(v) ? v : null; }
function pct(v) { return v === null ? "–" : `${Math.round(v)}%`; }
function gb(bytes) {
  const b = num(bytes);
  if (b === null || b < 0) return "–";
  if (b >= 1024 ** 4) return `${(b / (1024 ** 4)).toFixed(2)} TB`;
  if (b >= 1024 ** 3) return `${(b / (1024 ** 3)).toFixed(1)} GB`;
  if (b >= 1024 ** 2) return `${Math.round(b / (1024 ** 2))} MB`;
  return `${Math.round(b / 1024)} KB`;
}
function mbps(bps) {
  const b = num(bps);
  if (b === null) return "–";
  return `${(b / (1024 * 1024)).toFixed(1)} MB/s`;
}
function dur(secs) {
  const s = num(secs);
  if (s === null) return "–";
  if (s < 60) return `${s.toFixed(1)}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}
function ageSec(iso) {
  if (!iso) return null;
  const ms = Date.now() - Date.parse(iso);
  return Number.isFinite(ms) ? Math.max(0, Math.floor(ms / 1000)) : null;
}
function fmtAge(sec) {
  if (sec === null) return "never";
  if (sec < 60) return `${sec}s ago`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m ago`;
  return `${Math.floor(sec / 86400)}d ago`;
}
function level(val, warn, crit) {
  const v = num(val);
  if (v === null) return "lv-none";
  if (v >= crit) return "lv-crit";
  if (v >= warn) return "lv-warn";
  return "lv-ok";
}
function isStale(d) {
  if (!d || !d.has_reading) return false;
  const sec = ageSec(d.minute);
  if (sec !== null) return sec >= STALE_SEC;
  return !d.fresh;
}
function worstLevel(levels) {
  if (levels.includes("lv-crit")) return "lv-crit";
  if (levels.includes("lv-warn")) return "lv-warn";
  return "lv-ok";
}

function miniMeter(label, valueText, fillPct, lv) {
  const w = Math.max(0, Math.min(100, fillPct ?? 0));
  return `<span class="g-hm ${lv}"><span class="g-hm-k">${label}</span><span class="g-hm-bar"><i style="width:${w.toFixed(0)}%"></i></span><span class="g-hm-v">${esc(valueText)}</span></span>`;
}

function renderCardBody(d, stale, ageText) {
  const host = d.host || "host";
  const href = `/resources#${encodeURIComponent(host)}`;
  const statusPill = !d.has_reading
    ? `<span class="g-hb lv-none">no reading</span>`
    : stale
      ? `<span class="g-hb lv-warn">stale · ${esc(ageText)}</span>`
      : `<span class="g-hb lv-ok">${esc(ageText)}</span>`;
  const onlinePill = d.host_online === false ? `<span class="g-hb lv-crit">offline</span>` : "";

  const head = `<div class="g-hc-head">
    <div class="g-hc-title"><strong>${esc(host)}</strong>${onlinePill}${statusPill}</div>
    <div class="g-hc-actions">
      <a class="g-hc-link" href="${href}">Full charts →</a>
      <button type="button" class="g-hc-close" aria-label="Close host resource details">×</button>
    </div>
  </div>`;

  if (!d.has_reading) {
    return head + `<p class="g-hc-empty">No host resource readings recorded for <code>${esc(host)}</code> yet.</p>`;
  }

  const cpuAvg = num(d.cpu_avg_pct);
  const cpuMax = num(d.cpu_max_pct);
  const cpus = num(d.cpus) || 0;
  const load1 = num(d.load1);
  const load5 = num(d.load5);
  const load15 = num(d.load15);
  const loadPer = num(d.load1_per_cpu) ?? (cpus > 0 && load1 !== null ? load1 / cpus : null);
  const load5Per = cpus > 0 && load5 !== null ? load5 / cpus : null;
  const load15Per = cpus > 0 && load15 !== null ? load15 / cpus : null;

  const memTot = num(d.mem_total) || 0;
  const memAvail = num(d.mem_available_min) || 0;
  const memUsed = Math.max(0, memTot - memAvail);
  const memPct = num(d.mem_used_frac) !== null ? d.mem_used_frac * 100 : (memTot > 0 ? (memUsed / memTot) * 100 : null);

  const swapTot = num(d.swap_total) || 0;
  const swapUsed = num(d.swap_used_max) || 0;
  const swapAvail = Math.max(0, swapTot - swapUsed);

  const paseoMax = num(d.paseo_mem_max);
  const paseoPeak = num(d.paseo_mem_peak);
  const paseoOom = num(d.paseo_oom_kills) || 0;

  const cpuLv = level(cpuAvg, 70, 90);
  const memLv = level(memPct, 75, 90);
  const loadLv = level(loadPer, 0.75, 1.2);

  // Overview stats grid
  const stats = `<div class="g-hc-grid">
    <div class="g-hc-stat">
      <div class="g-hc-k">CPU${cpus ? ` <span>(${cpus} cores)</span>` : ""}</div>
      <div class="g-hc-v ${cpuLv}">${pct(cpuAvg)} <span class="g-hc-sub">avg · ${pct(cpuMax)} peak</span></div>
      <div class="g-hc-bar ${cpuLv}"><i style="width:${Math.min(100, Math.max(0, cpuAvg ?? 0)).toFixed(0)}%"></i></div>
    </div>
    <div class="g-hc-stat">
      <div class="g-hc-k">Load <span>1 · 5 · 15m</span></div>
      <div class="g-hc-v ${loadLv}">${load1 !== null ? `${load1.toFixed(1)} · ${load5?.toFixed(1) ?? "–"} · ${load15?.toFixed(1) ?? "–"}` : "–"}</div>
      <div class="g-hc-sub">${loadPer !== null ? `${loadPer.toFixed(2)}/cpu` : "–"}${load5Per !== null ? ` · ${load5Per.toFixed(2)} · ${load15Per?.toFixed(2) ?? "–"}` : ""}</div>
    </div>
    <div class="g-hc-stat">
      <div class="g-hc-k">Memory</div>
      <div class="g-hc-v ${memLv}">${pct(memPct)} <span class="g-hc-sub">${gb(memAvail)} free</span></div>
      <div class="g-hc-bar ${memLv}"><i style="width:${Math.min(100, Math.max(0, memPct ?? 0)).toFixed(0)}%"></i></div>
      <div class="g-hc-sub">${gb(memUsed)} / ${gb(memTot)}${swapTot > 0 ? ` · swap ${gb(swapAvail)} free (${gb(swapUsed)}/${gb(swapTot)})` : ""}</div>
    </div>
    <div class="g-hc-stat">
      <div class="g-hc-k">Paseo cgroup</div>
      <div class="g-hc-v">${paseoMax !== null ? gb(paseoMax) : "n/a"}</div>
      <div class="g-hc-sub">${paseoPeak !== null ? `peak ${gb(paseoPeak)}` : "no cgroup data"}${paseoOom > 0 ? ` · <b class="lv-crit">${paseoOom} OOM</b>` : ""}</div>
    </div>
  </div>`;

  // Disk I/O, Pressure (PSI), OOM kills
  const readBps = num(d.disk_read_bps);
  const writeBps = num(d.disk_write_bps);
  const iops = num(d.disk_iops);
  const busyAvg = num(d.disk_busy_avg_pct);
  const busyMax = num(d.disk_busy_max_pct);
  const busyLv = level(busyAvg, 50, 80);

  const psiCpu = num(d.psi_cpu_some);
  const psiMem = num(d.psi_mem_some);
  const psiMemFull = num(d.psi_mem_full);
  const psiIo = num(d.psi_io_some);
  const psiIoFull = num(d.psi_io_full);
  const hasPsi = psiCpu !== null || psiMem !== null || psiIo !== null;

  const ioPsi = `<div class="g-hc-sec">
    <div class="g-hc-two">
      <div>
        <h3>Disk I/O</h3>
        <div class="g-hc-kv">
          <span>Read <b>${mbps(readBps)}</b> · Write <b>${mbps(writeBps)}</b></span>
          <span>${iops !== null ? `${Math.round(iops)} IOPS` : "–"}${busyAvg !== null ? ` · busy <b class="${busyLv}">${pct(busyAvg)}</b>${busyMax !== null ? ` (${pct(busyMax)} peak)` : ""}` : ""}</span>
        </div>
      </div>
      <div>
        <h3>Pressure (PSI)</h3>
        <div class="g-hc-kv">${hasPsi ? `
          <span>CPU <b class="${level(psiCpu, 10, 25)}">${psiCpu !== null ? psiCpu.toFixed(1) + "%" : "–"}</b> · I/O <b class="${level(psiIo, 10, 25)}">${psiIo !== null ? psiIo.toFixed(1) + "%" : "–"}</b>${psiIoFull ? ` (${psiIoFull.toFixed(1)}% full)` : ""}</span>
          <span>Mem <b class="${level(psiMem, 5, 15)}">${psiMem !== null ? psiMem.toFixed(1) + "%" : "–"}</b>${psiMemFull ? ` (${psiMemFull.toFixed(1)}% full)` : ""}</span>`
          : `<span class="muted">Not reported on this host</span>`}</div>
      </div>
    </div>
  </div>`;

  // Filesystems
  let fses = Array.isArray(d.filesystems) ? d.filesystems : [];
  if (!fses.length && num(d.disk_total) > 0) {
    fses = [{ mount: "~ (home)", fs_type: "", total: d.disk_total, available: d.disk_available_min, used_frac: d.disk_used_frac }];
  }
  const fsRows = fses.length
    ? fses.map((f) => {
        const uf = num(f.used_frac) !== null ? f.used_frac * 100 : (f.total > 0 ? ((f.total - f.available) / f.total) * 100 : null);
        const flv = level(uf, 75, 90);
        return `<div class="g-hc-fs">
          <span class="g-hc-mount" title="${esc(f.device || f.mount)}">${esc(f.mount)}${f.fs_type ? ` <i>${esc(f.fs_type)}</i>` : ""}</span>
          <span class="g-hc-fspace">${gb(f.available)} free / ${gb(f.total)}</span>
          <span class="g-hc-fpct ${flv}">${pct(uf)}</span>
        </div>`;
      }).join("")
    : `<div class="g-hc-sub">No filesystem data</div>`;
  const fsSec = `<div class="g-hc-sec"><h3>Filesystems</h3>${fsRows}</div>`;

  // OOM kills
  const ooms = Array.isArray(d.recent_ooms) ? d.recent_ooms : [];
  const minOoms = num(d.oom_kills) || 0;
  const oomSec = (ooms.length || minOoms > 0 || paseoOom > 0)
    ? `<div class="g-hc-sec g-hc-oom">
        <h3 class="lv-crit">OOM kills${minOoms > 0 ? ` (${minOoms} in last min)` : ""}</h3>
        ${ooms.length ? ooms.slice(0, 4).map((o) => `<div class="g-hc-row">
          <span class="mono">${esc(o.comm || "process")} (${esc(o.pid)})</span>
          <span>${gb(o.anon_rss)} · ${esc(fmtAge(ageSec(o.at)))}</span>
        </div>`).join("") : `<div class="g-hc-sub">Kernel recorded ${minOoms} OOM kill(s) in the latest minute.</div>`}
      </div>`
    : `<div class="g-hc-sec g-hc-oom-ok"><span>OOM kills (24h): <b>0</b></span>${d.probe_running === false && d.probe_error !== "unsupported" ? `<span class="g-hc-sub">eBPF probe off</span>` : ""}</div>`;

  // Busiest session agents
  const agents = Array.isArray(d.session_agents) ? d.session_agents : [];
  const agentSec = `<div class="g-hc-sec">
    <h3>Busiest agents in this session <span>(15m)</span></h3>
    ${agents.length
      ? agents.slice(0, 6).map((a) => `<div class="g-hc-row">
          <span class="g-hc-ag"><b>${esc(a.name)}</b> <span class="g-hc-role">${esc(a.role)}</span></span>
          <span class="g-hc-agm">${gb(a.mem_max)} · ${dur(a.cpu_seconds)}${a.processes_max ? ` · ${a.processes_max}p` : ""}</span>
        </div>`).join("")
      : `<div class="g-hc-sub">No agent resource readings in the last 15 minutes.</div>`}
  </div>`;

  return head + stats + ioPsi + fsSec + oomSec + agentSec;
}

function render() {
  const d = current;
  if (!d) return;
  slot.hidden = false;
  const stale = isStale(d);
  const ageText = d.has_reading ? fmtAge(ageSec(d.minute)) : "no data";
  slot.classList.toggle("is-stale", stale);
  slot.classList.toggle("is-open", pinned || hoverOpen);

  let triggerInner = "";
  let overallLv = "lv-ok";

  if (!d.has_reading) {
    overallLv = d.host_online === false ? "lv-crit" : "lv-none";
    triggerInner = `<span class="g-hdot ${overallLv}"></span><span class="g-host-name">${esc(d.host)}</span><span class="g-hm-v muted">no metrics</span>`;
  } else {
    const cpuAvg = num(d.cpu_avg_pct);
    const memPct = num(d.mem_used_frac) !== null ? d.mem_used_frac * 100 : null;
    const diskPct = num(d.disk_used_frac) !== null ? d.disk_used_frac * 100 : null;
    const loadPer = num(d.load1_per_cpu);
    const psiMax = Math.max(num(d.psi_mem_some) ?? 0, num(d.psi_io_some) ?? 0);

    const cpuLv = level(cpuAvg, 70, 90);
    const memLv = level(memPct, 75, 90);
    const diskLv = level(diskPct, 75, 90);
    const loadLv = level(loadPer, 0.75, 1.2);
    const psiLv = level(psiMax, 5, 15);
    const ooms = Array.isArray(d.recent_ooms) ? d.recent_ooms : [];
    const freshOom = (num(d.oom_kills) || 0) > 0 || ooms.some((o) => { const s = ageSec(o.at); return s !== null && s < 900; });
    const olderOom = !freshOom && ooms.length > 0;
    const oomLv = freshOom ? "lv-crit" : olderOom ? "lv-warn" : "lv-ok";
    overallLv = d.host_online === false ? "lv-crit" : worstLevel([cpuLv, memLv, diskLv, loadLv, psiLv, oomLv, stale ? "lv-warn" : "lv-ok"]);

    const fourth = psiMax >= 5
      ? miniMeter("PSI", `${Math.round(psiMax)}%`, psiMax, psiLv)
      : miniMeter("LD", loadPer !== null ? `${loadPer.toFixed(1)}` : "–", loadPer !== null ? Math.min(100, loadPer * 65) : 0, loadLv);

    triggerInner = `<span class="g-hdot ${overallLv}"></span>` +
      `<span class="g-host-name">${esc(d.host)}</span>` +
      `<span class="g-host-meters">` +
      miniMeter("CPU", pct(cpuAvg), cpuAvg, cpuLv) +
      miniMeter("MEM", pct(memPct), memPct, memLv) +
      miniMeter("DSK", pct(diskPct), diskPct, diskLv) +
      fourth +
      `</span>` +
      (stale ? `<span class="g-host-stale">stale</span>` : "");
  }

  const open = pinned || hoverOpen;
  slot.innerHTML = `<button type="button" class="g-host-btn ${overallLv}" aria-expanded="${open}" aria-haspopup="dialog" aria-controls="g-host-card" title="Host resources (${esc(d.host)} · ${esc(ageText)}). Hover or click for details.">${triggerInner}</button>` +
    `<div class="g-host-card" id="g-host-card" role="dialog" aria-label="Host resources for ${esc(d.host)}"${open ? "" : " hidden"}>${renderCardBody(d, stale, ageText)}</div>`;
  if (open) positionCard();
}

function positionCard() {
  const card = slot.querySelector(".g-host-card");
  if (!card || card.hidden) return;
  if (window.matchMedia && window.matchMedia("(max-width: 780px)").matches) {
    card.style.left = "";
    card.style.right = "";
    return;
  }
  if (typeof slot.getBoundingClientRect !== "function" || typeof card.getBoundingClientRect !== "function") return;
  card.style.left = "";
  card.style.right = "0";
  const sr = slot.getBoundingClientRect();
  const cr = card.getBoundingClientRect();
  if (cr.left < 8) {
    card.style.right = "auto";
    card.style.left = `${Math.max(8 - sr.left, 0)}px`;
  }
}

function setOpen(nextPinned, nextHover) {
  pinned = nextPinned;
  hoverOpen = nextHover;
  const open = pinned || hoverOpen;
  slot.classList.toggle("is-open", open);
  const btn = slot.querySelector(".g-host-btn");
  const card = slot.querySelector(".g-host-card");
  if (btn) btn.setAttribute("aria-expanded", String(open));
  if (card) card.hidden = !open;
  if (open) positionCard();
}

async function refresh() {
  if (inflight) { pendingRefresh = true; return; }
  inflight = true;
  try {
    const query = `SELECT to_jsonb(r) FROM grimm_host_resources r WHERE session_id = '${sessionId}'`;
    const r = await fetch("/api/sql", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query }),
    });
    if (!r.ok) return;
    const body = await r.json();
    const row = body && Array.isArray(body.rows) && body.rows[0] ? body.rows[0][0] : null;
    if (row) { current = row; render(); }
  } catch (_) {
    // keep last good snapshot if a transient error occurs
  } finally {
    inflight = false;
    if (pendingRefresh) { pendingRefresh = false; refresh(); }
  }
}

slot.addEventListener("mouseenter", () => {
  if (window.matchMedia && window.matchMedia("(hover: none), (max-width: 780px)").matches) return;
  clearTimeout(closeTimer);
  setOpen(pinned, true);
});
slot.addEventListener("mouseleave", () => {
  clearTimeout(closeTimer);
  closeTimer = setTimeout(() => setOpen(pinned, false), 140);
});
slot.addEventListener("click", (e) => {
  if (e.target && e.target.closest && e.target.closest(".g-hc-close")) {
    e.stopPropagation();
    setOpen(false, false);
    return;
  }
  if (e.target && e.target.closest && e.target.closest(".g-host-btn")) {
    e.stopPropagation();
    setOpen(!pinned, false);
  }
});
document.addEventListener("click", (e) => {
  if ((pinned || hoverOpen) && !slot.contains(e.target)) setOpen(false, false);
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && (pinned || hoverOpen)) setOpen(false, false);
});
document.addEventListener("grimm:host-resources", () => { refresh(); });

// Update relative age text & stale status locally every 10s (zero network requests).
if (typeof setInterval === "function") {
  setInterval(() => { if (current && current.has_reading) render(); }, 10000);
}

refresh();
})();
