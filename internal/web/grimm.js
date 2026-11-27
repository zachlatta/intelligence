// Grimm, the home page. The page reads rows of the grimm_* views (the same ones /api/sql reads):
// the stream (/api/grimm/sessions/{id}/stream, server-sent events woken by the server's change
// hub) pushes them as they change, and a worker's live log streams the same way. Every change
// goes through a JSON route under /api/grimm/. No framework, no build step.
"use strict";
(() => {
const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => Array.from(el.querySelectorAll(sel));

const PROVIDER = { claude: "Claude", codex: "Codex", antigravity: "Antigravity" };
const ICONS = { hire: "＋", say: "✉", peek: "👁", wait: "⏳", stop: "■", archive: "⌫", roster: "☰", quota: "◔", directive: "§",
  bus: "⇄", shell: "$", read: "📄", edit: "✎", search: "⌕", tool: "•" };

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function el(tag, cls, html) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (html !== undefined) e.innerHTML = html;
  return e;
}
function shortTime(iso) {
  return iso ? new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "";
}
function since(iso) {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}
function provClass(p) { return ["claude", "codex", "antigravity"].includes(p) ? "p-" + p : "p-other"; }

let toastTimer;
function toast(msg, bad) {
  let t = $("#g-toast");
  if (!t) { t = el("div", "g-toast"); t.id = "g-toast"; t.setAttribute("role", "status"); document.body.append(t); }
  t.textContent = msg; t.className = "g-toast" + (bad ? " bad" : ""); t.hidden = false;
  clearTimeout(toastTimer); toastTimer = setTimeout(() => (t.hidden = true), 4200);
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
  const r = await fetch(path, opts);
  if (r.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname); throw new Error("signed out"); }
  let data = {};
  const text = await r.text();
  try { data = text ? JSON.parse(text) : {}; } catch (_) { data = { error: text }; }
  if (!r.ok) throw new Error(data.error || data.message || `${r.status} ${r.statusText}`);
  return data;
}
async function sql(query) {
  const r = await fetch("/api/sql", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ query }) });
  const body = await r.json();
  if (!r.ok) throw new Error(body.error || r.statusText);
  return body.rows || [];
}

// ---------- markdown: escape first, then a small safe subset ----------
const LIST_RE = /^\s*([-*+]|\d+[.)])\s+/;
function md(src, names) {
  const blocks = [];
  const text = String(src ?? "").replace(/```([\w-]*)\n?([\s\S]*?)```/g, (_, lang, code) => {
    blocks.push(`<pre><code>${esc(code.replace(/\n$/, ""))}</code></pre>`);
    return `\u0000${blocks.length - 1}\u0000`;
  });
  const nameRe = names && names.length ? new RegExp(`\\b(${names.map((n) => n.replace(/[-]/g, "\\-")).join("|")})\\b`, "g") : null;
  const inline = (s) => {
    const codes = [];
    s = esc(s).replace(/`([^`]+)`/g, (_, c) => { codes.push(`<code>${c}</code>`); return `\u0001${codes.length - 1}\u0001`; });
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/(^|[^*\w])\*([^*\n]+)\*(?!\w)/g, "$1<em>$2</em>")
      .replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>')
      .replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, '$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>');
    if (nameRe) s = s.split(/(<[^>]+>)/).map((p) => (p.startsWith("<") ? p : p.replace(nameRe, (m) => chipHTML(m)))).join("");
    return s.replace(/\u0001(\d+)\u0001/g, (_, i) => codes[+i]);
  };
  const lines = text.split("\n");
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    let m;
    if (/^\u0000\d+\u0000$/.test(line.trim())) { out.push(line.trim()); i++; continue; }
    if (/^\s*$/.test(line)) { i++; continue; }
    if ((m = line.match(/^(#{1,4})\s+(.*)$/))) { out.push(`<h${m[1].length + 1}>${inline(m[2])}</h${m[1].length + 1}>`); i++; continue; }
    if (LIST_RE.test(line)) {
      const start = i;
      while (i < lines.length && (LIST_RE.test(lines[i]) || /^\s{2,}\S/.test(lines[i]))) i++;
      out.push(renderList(lines.slice(start, i), inline));
      continue;
    }
    if (/^\|.*\|\s*$/.test(line) && i + 1 < lines.length && /^\|[\s:|-]+\|\s*$/.test(lines[i + 1])) {
      const row = (l) => l.trim().replace(/^\||\|$/g, "").split("|").map((c) => c.trim());
      const head = row(line); i += 2;
      const rows = [];
      while (i < lines.length && /^\|.*\|\s*$/.test(lines[i])) { rows.push(row(lines[i])); i++; }
      out.push(`<table><thead><tr>${head.map((h) => `<th>${inline(h)}</th>`).join("")}</tr></thead><tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${inline(c)}</td>`).join("")}</tr>`).join("")}</tbody></table>`);
      continue;
    }
    if (/^>\s?/.test(line)) {
      const q = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) { q.push(lines[i].replace(/^>\s?/, "")); i++; }
      out.push(`<blockquote>${inline(q.join(" "))}</blockquote>`);
      continue;
    }
    const para = [];
    while (i < lines.length && !/^\s*$/.test(lines[i]) && !/^(#{1,4})\s/.test(lines[i]) && !LIST_RE.test(lines[i]) && !/^\u0000\d+\u0000$/.test(lines[i].trim())) { para.push(lines[i]); i++; }
    out.push(`<p>${para.map(inline).join("<br>")}</p>`);
  }
  return out.join("").replace(/\u0000(\d+)\u0000/g, (_, n) => blocks[+n]);
}
function renderList(lines, inline) {
  const indent = (l) => l.match(/^\s*/)[0].length;
  const base = indent(lines[0]);
  const ordered = /^\s*\d+[.)]\s+/.test(lines[0]);
  const items = [];
  for (const l of lines) {
    if (LIST_RE.test(l) && indent(l) <= base + 1) items.push({ text: l.replace(LIST_RE, ""), sub: [] });
    else if (items.length) {
      const it = items[items.length - 1];
      if (LIST_RE.test(l) || it.sub.length) it.sub.push(l); else it.text += " " + l.trim();
    }
  }
  const html = items.map((it) => `<li>${inline(it.text)}${it.sub.length ? renderList(it.sub, inline) : ""}</li>`).join("");
  return ordered ? `<ol>${html}</ol>` : `<ul>${html}</ul>`;
}

// ---------- tool-call labels: what the manager did, in plain words ----------
function splitShell(s) {
  const out = []; let cur = ""; let q = "";
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (q) { if (c === q) q = ""; else cur += c; continue; }
    if (c === "'" || c === '"') { q = c; continue; }
    if (/\s/.test(c)) { if (cur) { out.push(cur); cur = ""; } continue; }
    cur += c;
  }
  if (cur) out.push(cur);
  return out;
}
function grimmCalls(command) {
  const calls = [];
  for (const part of String(command || "").split(/\s*(?:&&|\|\||;|\n|\|)\s*/)) {
    const toks = splitShell(part.trim());
    for (let i = 0; i < toks.length; i++) {
      if (/(^|\/)(intelligence|grimm)$/.test(toks[i])) {
        let j = i + 1;
        if (toks[i].endsWith("intelligence") && toks[j] === "grimm") j++;
        if (toks[j] === "ctl" || toks[j] === "bus") {
          const rest = toks.slice(j).filter((t, k, a) => t !== "--state-dir" && a[k - 1] !== "--state-dir");
          calls.push(rest);
          break;
        }
      }
    }
  }
  return calls;
}
function flag(toks, name) {
  for (let i = 0; i < toks.length; i++) {
    if (toks[i] === name && i + 1 < toks.length) return toks[i + 1];
    if (toks[i].startsWith(name + "=")) return toks[i].slice(name.length + 1);
  }
  return "";
}
function positionals(toks) {
  const out = []; const skip = new Set(["--lines", "--timeout", "--from", "--file", "--role", "--name", "--provider", "--to", "--kind", "--limit"]);
  for (let i = 0; i < toks.length; i++) {
    if (skip.has(toks[i])) { i++; continue; }
    if (toks[i].startsWith("--") || toks[i] === "-" || toks[i].startsWith("<<")) continue;
    out.push(toks[i]);
  }
  return out;
}
function toolInfo(item) {
  const d = (item && item.detail) || {};
  const input = d.input || {};
  const command = d.command || input.command || "";
  const output = typeof d.output === "string" ? d.output : d.output != null ? JSON.stringify(d.output, null, 2) : (typeof d.content === "string" ? d.content : "");
  const name = item.name || d.type || "tool";
  const shell = d.type === "shell" || !!command;
  return { name, command, output, input, detail: d, shell, status: item.status || "running", error: item.error };
}
function describeTool(item) {
  const t = toolInfo(item);
  let icon = "tool"; const workers = []; let label = "";
  if (t.shell) {
    const calls = grimmCalls(t.command);
    if (calls.length) {
      const hired = [...String(t.output || "").matchAll(/^hired ([\w-]+) \(([^)]+)\) on ([\w.-]+\/[\w.-]+)/gm)].map((m) => ({ name: m[1], role: m[2], spec: m[3] }));
      let labels = [];
      for (const call of calls) {
        const verb = call[1] || ""; const args = call.slice(2);
        if (call[0] === "bus") { labels.push(verb === "read" ? "Read the message board" : `Posted to ${flag(args, "--to") || "the board"} on the bus`); icon = "bus"; continue; }
        const pos = positionals(args);
        switch (verb) {
          case "hire": labels.push(`Hiring ${flag(args, "--name") || "a " + (flag(args, "--role") || "worker")}`); icon = "hire"; break;
          case "say": labels.push(`Messaged ${pos[0] || "a worker"}`); workers.push(pos[0]); icon = "say"; break;
          case "peek": labels.push(`Checked on ${pos[0] || "a worker"}`); workers.push(pos[0]); icon = "peek"; break;
          case "result": labels.push(`Read ${pos[0] || "a worker"}'s report`); workers.push(pos[0]); icon = "peek"; break;
          case "stop": labels.push(`Stopped ${pos[0] || "a worker"}`); workers.push(pos[0]); icon = "stop"; break;
          case "wait": labels.push(`Waiting for ${pos.join(", ") || "all running workers"}`); workers.push(...pos); icon = "wait"; break;
          case "archive": labels.push(pos[0] === "all" ? "Archived everyone" : `Archived ${pos[0] || "a worker"}`); icon = "archive"; break;
          case "roster": labels.push("Checked the roster"); icon = "roster"; break;
          case "quota": labels.push("Checked plan headroom"); icon = "quota"; break;
          case "directive": labels.push({ add: "Added a standing directive", remove: "Removed a directive" }[args[0]] || "Read the directives"); icon = "directive"; break;
          default: labels.push("grimm " + call.join(" ").slice(0, 80));
        }
      }
      if (hired.length) labels = ["Hired " + hired.map((h) => `${h.name} (${h.role}) on ${h.spec}`).join(", ")].concat(labels.filter((l) => !l.startsWith("Hiring")));
      label = [...new Set(labels)].join("; ");
    } else {
      label = (t.input && t.input.description) || (t.detail && t.detail.description) || `Ran ${String(t.command).split("\n")[0].slice(0, 90) || "a command"}`;
      icon = "shell";
    }
  } else if (t.detail.type === "read" || /^read$/i.test(t.name)) {
    label = "Read " + (t.detail.filePath || t.input.file_path || ""); icon = "read";
  } else if (t.detail.type === "edit" || t.detail.type === "write" || /^(edit|write)$/i.test(t.name)) {
    label = "Edited " + (t.detail.filePath || t.input.file_path || ""); icon = "edit";
  } else if (t.detail.type === "search" || /^(grep|glob)$/i.test(t.name)) {
    label = "Searched for " + JSON.stringify(String(t.detail.query || t.input.pattern || "").slice(0, 60)); icon = "search";
  } else if (t.detail.type === "unknown" && t.status === "running") {
    label = /^bash$/i.test(t.name) ? "Running a command…" : `Running ${t.name}…`; icon = /^bash$/i.test(t.name) ? "shell" : "tool";
  } else {
    label = t.name;
  }
  return { label, icon, workers: workers.filter(Boolean), info: t };
}

// ---------- state ----------
const root = $("#grimm");
const S = {
  id: root ? root.dataset.session : "",
  session: {}, workers: [], directives: [], quota: [],
  items: new Map(), order: [], bus: [], feed: [],
  focus: null, focusTab: "log", focusES: null, focusEntries: new Map(), focusWorker: null,
  es: null, pending: [],
};
function workerByName(name) { return S.workers.find((w) => w.name === name); }
function knownNames() { return S.workers.map((w) => w.name).filter((n) => n.length > 2); }
function chipHTML(name) {
  const w = workerByName(name);
  return `<span class="g-wchip ${w ? provClass(w.provider) : "p-other"}" data-worker="${esc(name)}"><i></i>${esc(name)}</span>`;
}
function managerWorking() { return ["working", "starting"].includes(S.session.manager_state); }

// ---------- thread ----------
const ATTACH = /\[attachment:([0-9a-f-]{36})\]/g;
const attachmentCache = new Map();
function attachmentsOf(body) { return [...String(body || "").matchAll(ATTACH)].map((m) => m[1]); }
function humanText(body) {
  return String(body || "").replace(/\n*Attached (images|files):\n(\[attachment:[0-9a-f-]{36}\]\n?)+\s*$/, "").replace(ATTACH, "").trim();
}
async function loadAttachment(id, holder) {
  if (!attachmentCache.has(id)) {
    attachmentCache.set(id, sql(`SELECT name, media_type, CASE WHEN media_type LIKE 'image/%' AND octet_length(data) <= 4194304 THEN encode(data, 'base64') ELSE '' END
      FROM attachments WHERE id = '${id}'`).then((rows) => rows[0] || null).catch(() => null));
  }
  const row = await attachmentCache.get(id);
  if (!row) { holder.append(el("span", "g-file", "attachment")); return; }
  const [name, type, data] = row;
  if (data) {
    const img = el("img"); img.alt = name; img.src = `data:${type};base64,${data}`; img.loading = "lazy";
    img.addEventListener("click", () => { const box = el("div", "g-lightbox"); const big = el("img"); big.src = img.src; big.alt = name; box.append(big); box.onclick = () => box.remove(); document.body.append(box); });
    holder.append(img);
  } else {
    const f = el("span", "g-file"); f.textContent = name; holder.append(f);
  }
}
function statusHTML(st) {
  if (st === "running") return '<span class="g-st running" aria-label="running"></span>';
  if (st === "failed" || st === "error") return '<span class="g-st error" aria-label="failed">✕</span>';
  return '<span class="g-st done" aria-label="done">✓</span>';
}
function renderItem(it, isLast) {
  const names = knownNames();
  let node;
  const data = it.data || {};
  switch (it.kind) {
    case "human": {
      node = el("div", "g-msg human", `<span class="g-avatar h">Z</span><div class="g-msg-body"></div>`);
      const body = $(".g-msg-body", node);
      const text = humanText(it.body);
      if (text) { const b = el("div", "g-bubble" + (it.delivered_at ? "" : " pending")); b.textContent = text; b.title = it.delivered_at ? "" : "Waiting for Grimm to start"; body.append(b); }
      const ids = attachmentsOf(it.body);
      if (ids.length) { const th = el("div", "g-thumbs"); body.append(th); ids.forEach((id) => loadAttachment(id, th)); }
      break;
    }
    case "assistant_message":
      node = el("div", "g-msg grimm", `<span class="g-avatar g">G</span><div class="g-msg-body"><div class="g-name">Grimm</div><div class="g-md${isLast && managerWorking() ? " cursor" : ""}">${md(it.body, names)}</div></div>`);
      break;
    case "tool_call": {
      const d = describeTool(it.item || {});
      let label = esc(d.label);
      if (names.length) label = label.replace(new RegExp(`\\b(${names.map((n) => n.replace(/[-]/g, "\\-")).join("|")})\\b`, "g"), (m) => chipHTML(m));
      node = el("div", "g-tool", `<div class="g-tool-row"><span class="g-tool-ico">${ICONS[d.icon] || ICONS.tool}</span><span class="g-tool-label">${label}</span>${statusHTML(d.info.status)}</div>
        <div class="g-tool-detail">${d.info.command ? `<pre class="cmd">${esc(d.info.command)}</pre>` : `<pre class="cmd">${esc(JSON.stringify(d.info.input || d.info.detail || {}, null, 2))}</pre>`}${d.info.output ? `<pre>${esc(d.info.output)}</pre>` : ""}</div>`);
      if (S.items.get(it.item_id)?._open) node.classList.add("open");
      $(".g-tool-row", node).addEventListener("click", (e) => {
        if (e.target.closest(".g-wchip")) return;
        node.classList.toggle("open");
        const cur = S.items.get(it.item_id); if (cur) cur._open = node.classList.contains("open");
      });
      break;
    }
    case "update": {
      const sums = (data.summaries || []).map(esc).join(" · ");
      node = el("div", "", `<div class="g-update" role="button" tabindex="0">⚡ <b>Update</b> ${sums}</div><div class="g-update-detail" hidden><pre>${esc(it.body)}</pre></div>`);
      $(".g-update", node).addEventListener("click", () => { const dd = $(".g-update-detail", node); dd.hidden = !dd.hidden; });
      break;
    }
    case "command":
      node = el("div", "g-command" + (data.status === "error" ? " error" : ""), `<div class="chead"><span>${esc(it.body)}</span>${statusHTML(data.status === "error" ? "error" : "done")}</div>${data.output ? `<pre>${esc(data.output)}</pre>` : ""}`);
      break;
    case "divider":
      node = el("div", "g-divider", esc(it.body));
      break;
    default:
      node = el("div", "g-notice" + (data.level === "error" ? " error" : ""), md(it.body));
  }
  node.dataset.id = it.item_id;
  return node;
}
function nearBottom() { const t = $("#g-thread"); return t.scrollHeight - t.scrollTop - t.clientHeight < 160; }
function scrollBottom(force) { const t = $("#g-thread"); if (force || nearBottom()) requestAnimationFrame(() => { t.scrollTop = t.scrollHeight; }); }
function lastAssistantID() {
  for (let i = S.order.length - 1; i >= 0; i--) {
    const it = S.items.get(S.order[i]);
    if (it.kind === "assistant_message") return it.item_id;
    if (it.kind === "human") return "";
  }
  return "";
}
function upsertItems(items, reset) {
  const stick = reset || (nearBottom() && !loadingOlder);
  const box = $("#g-items");
  if (reset) {
    const open = new Set([...S.items.values()].filter((x) => x._open).map((x) => x.item_id));
    S.items = new Map(); S.order = []; box.innerHTML = "";
    items.forEach((it) => { if (open.has(it.item_id)) it._open = true; });
  }
  for (const it of items) {
    const prev = S.items.get(it.item_id);
    if (prev && prev._open) it._open = true;
    if (!prev) S.order.push(it.item_id);
    S.items.set(it.item_id, it);
  }
  if (!reset && items.length) {
    S.order.sort((a, b) => {
      const x = S.items.get(a), y = S.items.get(b);
      return x.at < y.at ? -1 : x.at > y.at ? 1 : a < b ? -1 : a > b ? 1 : 0;
    });
  }
  const last = lastAssistantID();
  const touched = new Set(items.map((i) => i.item_id));
  for (const id of S.order) {
    const it = S.items.get(id);
    let node = box.querySelector(`[data-id="${CSS.escape(id)}"]`);
    if (node && !touched.has(id) && !(it.kind === "assistant_message")) continue;
    if (node && it.kind === "assistant_message" && !touched.has(id)) { $(".g-md", node)?.classList.toggle("cursor", id === last && managerWorking()); continue; }
    if (node && it.kind === "assistant_message") {
      const m = $(".g-md", node); m.innerHTML = md(it.body, knownNames()); m.classList.toggle("cursor", id === last && managerWorking()); continue;
    }
    const fresh = renderItem(it, id === last);
    if (node) node.replaceWith(fresh); else box.append(fresh);
  }
  // keep DOM order equal to S.order
  let prevNode = null;
  for (const id of S.order) {
    const node = box.querySelector(`[data-id="${CSS.escape(id)}"]`);
    if (!node) continue;
    if (prevNode ? prevNode.nextSibling !== node : box.firstChild !== node) box.insertBefore(node, prevNode ? prevNode.nextSibling : box.firstChild);
    prevNode = node;
  }
  $("#g-empty").hidden = S.items.size > 0;
  if (stick) scrollBottom(true);
}

// ---------- older thread items, read through /api/sql a page at a time ----------
const sqlText = (v) => "'" + String(v).replace(/'/g, "''") + "'";
let loadingOlder = false;
async function loadOlder() {
  if (loadingOlder || !S.older || !S.order.length) return;
  const oldest = S.items.get(S.order[0]);
  if (!/^[mt][0-9.]+$/.test(oldest.item_id) || !/^[0-9T:.+\- Z]+$/.test(oldest.at)) return;
  loadingOlder = true;
  const btn = $("#g-older"); btn.disabled = true; btn.textContent = "Loading…";
  try {
    const q = root.dataset.olderSql.replace(":session", sqlText(S.id)).replace(":at", sqlText(oldest.at)).replace(":item", sqlText(oldest.item_id));
    const rows = await sql(q);
    const t = $("#g-thread");
    const fromBottom = t.scrollHeight - t.scrollTop;
    upsertItems(rows.map((r) => r[0]), false);
    t.scrollTop = t.scrollHeight - fromBottom; // keep what was in view where it was
    S.older = rows.length >= 200;
  } catch (e) {
    toast("Could not load older messages: " + e.message, true);
  } finally {
    loadingOlder = false; btn.disabled = false; btn.textContent = "Load older messages"; btn.hidden = !S.older;
  }
}

// ---------- header and manager ----------
function renderSession() {
  const s = S.session;
  if (!s.id) return;
  $("#g-repo").textContent = s.title || s.repo;
  $("#g-branch").textContent = s.branch || "";
  $("#g-chip").title = `${s.repo} on ${s.host}\nbranch ${s.branch}${s.workspace_error ? "\nworktree failed: " + s.workspace_error : ""}`;
  $("#g-cap-live").textContent = s.live_workers ?? 0;
  $("#g-cap-max").textContent = s.max_workers;
  $("#g-cap").classList.toggle("full", (s.live_workers || 0) >= s.max_workers);
  $("#g-cap").title = `${s.live_workers || 0} live workers of this session's cap of ${s.max_workers}. Click to change the cap.`;
  document.title = `Grimm · ${s.title || s.repo}`;
  const st = s.manager_state || "offline";
  const pill = $("#g-pill"); pill.dataset.state = st;
  const working = st === "working" || st === "starting";
  let label = { idle: "Grimm is idle", working: "Grimm is working", starting: s.workspace_id ? "Starting Grimm…" : "Making the worktree…",
    offline: s.manager_error ? "Grimm could not start" : "Grimm starts on your first message" }[st] || st;
  if (st === "offline" && !s.workspace_id && !s.workspace_error) label = "Making the worktree…";
  if (s.workspace_error) label = "Worktree failed";
  if (st === "working" && s.manager_turn_at) label += " · " + since(s.manager_turn_at);
  if (s.pending_updates && !working) label += ` · ${s.pending_updates} update${s.pending_updates === 1 ? "" : "s"} queued`;
  if (s.host_online === false) label += ` · ${s.host} offline`;
  $("#g-pill-label").textContent = label;
  pill.title = `Manager: ${s.manager_provider ? s.manager_provider + "/" + s.manager_model : "claude/claude-opus-5-5"}${s.manager_login ? " on " + s.manager_login : ""}${s.manager_error ? "\n" + s.manager_error : ""}`;
  $("#g-typing").hidden = !working;
  $("#g-typing-label").textContent = st === "starting" ? "Starting Grimm…" : "Grimm is working" + (s.manager_turn_at ? " · " + since(s.manager_turn_at) : "");
  $("#g-stop").hidden = st !== "working";
  const cur = $(`#g-items [data-id="${CSS.escape(lastAssistantID())}"] .g-md`);
  if (cur) cur.classList.toggle("cursor", st === "working");
}

// ---------- team ----------
function activityLine(w) {
  const a = w.activity;
  if (!a || typeof a !== "object") return "";
  if (a.type === "tool_call") return describeTool(a).label;
  if (a.type === "reasoning") return "Thinking…";
  if (a.type === "assistant_message") return String(a.text || "").replace(/\s+/g, " ").slice(0, 200);
  return "";
}
function workerCard(w) {
  const status = w.status;
  const running = status === "running";
  const act = running ? activityLine(w) : "";
  const node = el("div", `g-card ${provClass(w.provider)}`, `<div class="g-card-top"><span class="g-card-name">${esc(w.name)}</span><span class="g-card-role">${esc(w.role)}</span><span class="g-status ${esc(status)}">${esc(status)}</span></div>
    <div class="g-card-spec"><span class="g-dot ${provClass(w.provider)}"></span>${esc(PROVIDER[w.provider] || w.provider)} · ${esc(w.model)} · ${esc(String(w.login || "").split("@")[0])}${running ? " · " + since(w.turn_at) : ""}</div>
    ${act ? `<div class="g-card-act">${esc(act)}</div>` : ""}${w.task ? `<div class="g-card-task">${esc(w.task.replace(/\s+/g, " ").slice(0, 220))}</div>` : ""}`);
  node.dataset.worker = w.name;
  node.tabIndex = 0;
  node.setAttribute("role", "button");
  node.setAttribute("aria-label", `${w.name}, ${w.role}, ${status}`);
  return node;
}
function renderTeam() {
  const live = S.workers.filter((w) => w.status !== "archived");
  const arch = S.workers.filter((w) => w.status === "archived");
  const r = $("#g-roster"); r.innerHTML = "";
  if (!live.length) r.append(el("p", "muted small", arch.length ? "No active workers. Archived ones are below." : "No workers yet. Grimm hires them when it needs them."));
  live.forEach((w) => r.append(workerCard(w)));
  $("#g-archived").hidden = !arch.length;
  $("#g-archived-count").textContent = `(${arch.length})`;
  const ra = $("#g-roster-archived"); ra.innerHTML = "";
  arch.forEach((w) => ra.append(workerCard(w)));
  const working = live.filter((w) => w.status === "running" || w.status === "starting").length;
  $("#g-team-summary").textContent = live.length ? `${working} working · ${live.length - working} idle` : "";
  $("#g-team-count").textContent = live.length ? String(live.length) : "";
  if (S.focus) renderFocusHead();
}

// ---------- plans ----------
function renderQuota() {
  const list = $("#g-quota");
  const cands = S.quota || [];
  if (!cands.length) { list.innerHTML = '<p class="muted small">No plans on this host: add models to a model group on <a href="/usage">Plans</a>.</p>'; return; }
  list.innerHTML = "";
  let shown = false;
  for (const c of cands) {
    const next = c.eligible && !shown; if (next) shown = true;
    const rem = c.remaining_pct == null ? null : +c.remaining_pct;
    const exp = c.expected_remaining_pct == null ? null : +c.expected_remaining_pct;
    const node = el("div", `g-q ${provClass(c.provider)}${next ? " next" : ""}${c.eligible ? "" : " skip"}`,
      `<div class="g-q-top"><span class="g-dot"></span><span>${esc(PROVIDER[c.provider] || c.provider)}</span>${next ? '<span class="g-next">next hire</span>' : ""}<span class="g-pct">${rem == null ? "n/a" : rem.toFixed(0) + "%"}</span></div>
       <div class="g-q-acct" title="${esc(c.model + " · " + c.login + " · " + c.long_window)}">${esc(c.model)}${c.thinking ? " (" + esc(c.thinking) + ")" : ""} · ${esc(c.login)} · ${esc(c.long_window)}</div>
       <div class="g-bar-meter" title="Remaining ${rem ?? "?"}% · pace line at ${exp == null ? "?" : exp.toFixed(0)}% (the share of the window still to come)"><i style="width:${Math.max(0, Math.min(100, rem ?? 0))}%"></i>${exp == null ? "" : `<b style="left:calc(${Math.max(0, Math.min(100, exp))}% - 1px)"></b>`}</div>
       <div class="g-q-foot">${c.eligible ? `<span>score ${c.score == null ? "n/a" : (c.score >= 0 ? "+" : "") + (+c.score).toFixed(1)}${c.spread_penalty ? ` (−${(+c.spread_penalty).toFixed(0)} spread)` : ""}</span><span>${c.live_workers ? c.live_workers + " hired" : ""}</span>` : `<span class="why" title="${esc(c.reason)}">${esc(c.reason)}</span>`}</div>`);
    list.append(node);
  }
  $("#g-quota-meta").textContent = `Live from Intelligence's model groups on ${S.session.host || "the host"} · ${shortTime(new Date().toISOString())} · ahead of pace = higher score`;
}

// ---------- directives, bus, activity ----------
function renderDirectives() {
  const ol = $("#g-directives"); ol.innerHTML = "";
  for (const d of S.directives) {
    const li = el("li"); li.value = d.seq; li.textContent = d.text;
    const b = el("button", "", "×"); b.type = "button"; b.title = `Remove directive ${d.seq}`; b.setAttribute("aria-label", `Remove directive ${d.seq}`);
    b.addEventListener("click", async () => {
      if (!confirm(`Remove directive ${d.seq}?\n\n${d.text}`)) return;
      try { await api("DELETE", `/api/grimm/sessions/${S.id}/directives/${d.seq}`); } catch (e) { toast(e.message, true); }
    });
    li.append(b); ol.append(li);
  }
}
function busLi(m) {
  const li = el("li", `k-${esc(m.kind)}`, `<div class="route"><b>${esc(m.sender)}</b> → <b>${esc(m.recipient)}</b><span class="kind">${esc(m.kind)}</span> · ${shortTime(m.created_at)}</div><div class="body"></div>`);
  $(".body", li).textContent = m.body;
  li.addEventListener("click", () => li.classList.toggle("open"));
  return li;
}
function addBus(items) {
  const ul = $("#g-bus");
  const stick = ul.scrollHeight - ul.scrollTop - ul.clientHeight < 60;
  for (const m of items) {
    if (S.bus.some((x) => x.id === m.id)) continue;
    S.bus.push(m); ul.append(busLi(m));
  }
  while (S.bus.length > 300) { S.bus.shift(); ul.firstChild?.remove(); }
  if (stick) ul.scrollTop = ul.scrollHeight;
  if (S.focus && S.focusTab === "bus") renderFocusBody();
}
function addFeed(items) {
  for (const f of items) if (!S.feed.some((x) => x.id === f.id)) S.feed.push(f);
  S.feed = S.feed.slice(-120);
  const ul = $("#g-feed"); ul.innerHTML = "";
  for (const f of S.feed.slice().reverse()) {
    const li = el("li", "", `<time>${shortTime(f.at)}</time><span class="${f.level === "warn" ? "warn" : ""}"></span>`);
    $("span", li).textContent = f.text; ul.append(li);
  }
}

// ---------- worker focus ----------
function openFocus(name) {
  if (!workerByName(name)) return;
  if (S.focus !== name) { S.focusEntries = new Map(); S.focusWorker = null; }
  S.focus = name;
  $("#g-focus").hidden = false;
  if (matchMedia("(max-width: 780px)").matches) setTab("team");
  setFocusTab("log");
  renderFocusHead();
  if (S.focusES) S.focusES.close();
  const es = new EventSource(`/api/grimm/sessions/${S.id}/workers/${encodeURIComponent(name)}/stream`);
  S.focusES = es;
  es.addEventListener("worker", (e) => { S.focusWorker = JSON.parse(e.data); if (S.focusTab !== "log") renderFocusBody(); });
  es.addEventListener("entries", (e) => {
    const d = JSON.parse(e.data);
    if (d.reset) S.focusEntries = new Map();
    for (const x of d.items) S.focusEntries.set(x.seq_start, x);
    if (S.focusTab === "log") renderFocusBody();
  });
  const card = document.querySelector(`.g-card[data-worker="${CSS.escape(name)}"]`);
  if (card) { card.classList.remove("flash"); void card.offsetWidth; card.classList.add("flash"); }
  $("#g-focus-input").focus({ preventScroll: true });
}
function closeFocus() {
  S.focus = null; $("#g-focus").hidden = true;
  if (S.focusES) { S.focusES.close(); S.focusES = null; }
}
function renderFocusHead() {
  const w = workerByName(S.focus);
  if (!w) return;
  $("#g-focus-name").textContent = w.name;
  $("#g-focus-sub").textContent = `${w.role} · ${PROVIDER[w.provider] || w.provider} ${w.model} · ${w.login}`;
  const pill = $("#g-focus-status"); pill.className = `g-status ${w.status}`; pill.textContent = w.status;
  $("#g-focus-id").textContent = w.agent_id ? "agent " + w.agent_id.slice(0, 8) : "";
  $("#g-focus-archive").disabled = w.status === "archived";
  $("#g-focus-stop").disabled = w.status !== "running";
}
function setFocusTab(tab) {
  S.focusTab = tab;
  $$("#g-focus-tabs button").forEach((b) => b.classList.toggle("active", b.dataset.ftab === tab));
  renderFocusBody(true);
}
function logLine(e) {
  const it = e.item || {};
  if (e.kind === "tool_call") {
    const d = describeTool(it);
    const what = d.info.command || d.info.detail.filePath || d.info.detail.query || "";
    const failed = d.info.status === "failed";
    return `<span class="${failed ? "t-err" : "t-tool"}">[${esc(failed ? "Failed " + d.info.name : d.info.name)}]</span> ${esc(String(what).split("\n")[0].slice(0, 400))}${d.info.output && !failed ? "" : ""}`;
  }
  if (e.kind === "reasoning") return `<span class="t-think">[Thinking] ${esc(String(e.text || "").slice(0, 600))}</span>`;
  if (e.kind === "user_message") return `<span class="t-tool">[Message]</span> ${esc(String(e.text || "").slice(0, 600))}`;
  if (e.kind === "assistant_message") return `<span class="t-msg">${esc(e.text || "")}</span>`;
  return `<span class="t-think">[${esc(e.kind)}]</span>`;
}
function renderFocusBody(jump) {
  const body = $("#g-focus-body");
  const w = S.focusWorker || workerByName(S.focus) || {};
  const stick = jump || body.scrollHeight - body.scrollTop - body.clientHeight < 80;
  if (S.focusTab === "log") {
    const entries = [...S.focusEntries.values()].sort((a, b) => a.seq_start - b.seq_start);
    body.innerHTML = entries.length ? `<pre class="g-log">${entries.map(logLine).join("\n")}</pre>` : `<p class="muted">${w.agent_id ? "No activity yet." : "Starting…"}</p>`;
    if (stick) body.scrollTop = body.scrollHeight;
    return;
  }
  if (S.focusTab === "reply") body.innerHTML = `<div class="g-md">${w.last_response ? md(w.last_response, knownNames()) : '<p class="muted">No final reply yet. It appears when the worker finishes its turn.</p>'}</div>`;
  else if (S.focusTab === "task") body.innerHTML = `<div class="g-md">${md(w.task || "")}</div><p class="muted small">Hired ${esc(shortTime(w.created_at))} · ${esc(w.provider)}/${esc(w.model)} · plan ${esc(w.login)} (${esc(w.long_window)}) · score at hire ${w.score == null ? "n/a" : (+w.score).toFixed(1)}</p>`;
  else {
    const mine = S.bus.filter((m) => m.sender === w.name || m.recipient === w.name || m.recipient === "all");
    const ul = el("ul", "g-bus"); mine.forEach((m) => ul.append(busLi(m)));
    body.innerHTML = ""; body.append(mine.length ? ul : el("p", "muted", "No messages yet."));
  }
  if (jump) body.scrollTop = 0;
}

function setTab(tab) {
  root.dataset.tab = tab;
  $$("#g-tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
}

// ---------- the stream ----------
function connect() {
  const es = new EventSource(`/api/grimm/sessions/${S.id}/stream`);
  S.es = es;
  const conn = $("#g-conn");
  es.onopen = () => { conn.classList.add("on"); conn.setAttribute("aria-label", "Live"); };
  es.onerror = () => { conn.classList.remove("on"); conn.setAttribute("aria-label", "Reconnecting"); };
  es.addEventListener("state", (e) => {
    const d = JSON.parse(e.data);
    const was = S.session.manager_state;
    const before = knownNames().join(",");
    S.session = d.session; S.workers = d.workers; S.directives = d.directives; S.quota = d.quota;
    if (knownNames().join(",") !== before) {
      for (const it of S.items.values()) {
        if (it.kind !== "assistant_message" && it.kind !== "tool_call") continue;
        const node = $(`#g-items [data-id="${CSS.escape(it.item_id)}"]`);
        if (node) node.replaceWith(renderItem(it, it.item_id === lastAssistantID()));
      }
    }
    renderSession(); renderTeam(); renderQuota(); renderDirectives();
    if (was !== S.session.manager_state) upsertItems([], false);
  });
  es.addEventListener("thread", (e) => {
    const d = JSON.parse(e.data);
    upsertItems(d.items, d.reset);
    if (d.reset) { S.older = !!d.more; $("#g-older").hidden = !S.older; }
  });
  es.addEventListener("bus", (e) => addBus(JSON.parse(e.data).items));
  es.addEventListener("activity", (e) => addFeed(JSON.parse(e.data).items));
}

// ---------- composer ----------
function autosize() {
  const t = $("#g-input");
  t.style.height = "auto";
  t.style.height = Math.min(216, t.scrollHeight) + "px";
}
let picker = null, sending = false;
async function send() {
  const input = $("#g-input");
  const raw = input.value.trim();
  if (sending || (!raw && !(picker && picker.hasFiles()))) return;
  sending = true; $("#g-send").disabled = true;
  try {
    const text = picker ? await picker.prompt(raw) : raw;
    await api("POST", `/api/grimm/sessions/${S.id}/messages`, { text });
    input.value = ""; autosize();
    if (picker) picker.clear();
    scrollBottom(true);
  } catch (e) {
    toast(e.message, true);
  } finally {
    sending = false; $("#g-send").disabled = false;
  }
}

function bindSession() {
  const input = $("#g-input");
  if (typeof imageAttachments === "function") picker = imageAttachments(input, $("#g-box"), { panel: $("#g-attach"), action: $("#g-actions") });
  // the picker's button goes first in the composer's actions
  const actions = $("#g-actions"); const pick = actions.querySelector(".attachment-actions");
  if (pick) actions.prepend(pick);
  $("#g-composer").addEventListener("submit", (e) => { e.preventDefault(); send(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !matchMedia("(pointer: coarse)").matches) { e.preventDefault(); send(); }
  });
  input.addEventListener("input", autosize);
  $("#g-stop").addEventListener("click", async () => { try { await api("POST", `/api/grimm/sessions/${S.id}/manager/interrupt`, {}); } catch (e) { toast(e.message, true); } });
  $("#g-suggest").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) { input.value = b.textContent; autosize(); input.focus(); } });
  $("#g-quota-refresh").addEventListener("click", async () => {
    $("#g-quota-meta").textContent = "Refreshing…";
    try {
      const rows = await sql(`SELECT to_jsonb(q) FROM grimm_quota q WHERE session_id = '${S.id}' ORDER BY eligible DESC, score DESC NULLS LAST, provider, model, login`);
      S.quota = rows.map((r) => r[0]); renderQuota();
    } catch (e) { toast(e.message, true); }
  });
  $("#g-archive-all").addEventListener("click", async () => {
    const live = S.workers.filter((w) => w.status !== "archived");
    if (!live.length) return toast("No workers to archive.");
    if (!confirm(`Archive all ${live.length} worker(s)? Files in the worktree stay.`)) return;
    try { const r = await api("POST", `/api/grimm/sessions/${S.id}/archive-workers`, {}); toast(`Archived ${r.archived.length}: ${r.archived.join(", ") || "none"}`); } catch (e) { toast(e.message, true); }
  });
  $("#g-cap").addEventListener("click", async () => {
    const limit = +root.dataset.capLimit || 100;
    const v = prompt(`Live-worker cap for this session (1–${limit}):`, S.session.max_workers || 20);
    if (v === null) return;
    const n = parseInt(v, 10);
    if (!(n >= 1 && n <= limit)) return toast(`The cap is 1 to ${limit}.`, true);
    try { await api("PUT", `/api/grimm/sessions/${S.id}`, { maxWorkers: n }); toast(`Worker cap set to ${n}.`); } catch (e) { toast(e.message, true); }
  });
  $("#g-older").addEventListener("click", loadOlder);
  $("#g-thread").addEventListener("scroll", () => { if ($("#g-thread").scrollTop < 80) loadOlder(); }, { passive: true });
  $("#g-sessions").addEventListener("change", (e) => { location.href = "/grimm/" + e.target.value; });
  $("#g-directive-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const box = $("#g-directive-input"); const v = box.value.trim(); if (!v) return;
    try { await api("POST", `/api/grimm/sessions/${S.id}/directives`, { text: v }); box.value = ""; } catch (err) { toast(err.message, true); }
  });
  document.addEventListener("click", (e) => {
    const chip = e.target.closest(".g-wchip, .g-card");
    if (chip && chip.dataset.worker) { e.preventDefault(); openFocus(chip.dataset.worker); }
  });
  document.addEventListener("keydown", (e) => {
    const card = e.target.closest && e.target.closest(".g-card");
    if (card && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); openFocus(card.dataset.worker); }
  });
  $("#g-focus-close").addEventListener("click", closeFocus);
  $("#g-focus-tabs").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) setFocusTab(b.dataset.ftab); });
  $("#g-focus-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const box = $("#g-focus-input"); const v = box.value.trim(); if (!v || !S.focus) return;
    try { await api("POST", `/api/grimm/sessions/${S.id}/workers/${encodeURIComponent(S.focus)}/message`, { text: v }); box.value = ""; toast(`Sent to ${S.focus}.`); } catch (err) { toast(err.message, true); }
  });
  $("#g-focus-stop").addEventListener("click", async () => { try { const r = await api("POST", `/api/grimm/sessions/${S.id}/workers/${encodeURIComponent(S.focus)}/stop`, {}); toast(r.message); } catch (e) { toast(e.message, true); } });
  $("#g-focus-archive").addEventListener("click", async () => {
    const name = S.focus; if (!name || !confirm(`Archive ${name}? Its files stay.`)) return;
    try { await api("POST", `/api/grimm/sessions/${S.id}/workers/${encodeURIComponent(name)}/archive`, {}); toast(`Archived ${name}.`); } catch (e) { toast(e.message, true); }
  });
  $("#g-tabs").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) setTab(b.dataset.tab); });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && S.focus && !e.target.closest(".g-box")) closeFocus();
    if (e.key === "/" && (document.activeElement === document.body || !document.activeElement)) { e.preventDefault(); input.focus(); }
  });
  setInterval(() => { renderSession(); if (S.workers.some((w) => w.status === "running")) renderTeam(); }, 1000);
  if (matchMedia("(max-width: 780px)").matches) input.placeholder = "Tell Grimm what to do…";
  connect();
  if (!matchMedia("(pointer: coarse)").matches) input.focus();
}

function bindNew() {
  const form = $("#g-new"); if (!form) return;
  const host = $("#g-new-host"), list = $("#g-new-repos");
  const fill = () => {
    const opt = host.selectedOptions[0];
    list.innerHTML = "";
    for (const r of (opt?.dataset.repos || "").split("\n").filter(Boolean)) { const o = document.createElement("option"); o.value = r; list.append(o); }
  };
  host.addEventListener("change", fill); fill();
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#g-new-error").textContent = ""; $("#g-new-start").disabled = true;
    try {
      const r = await api("POST", "/api/grimm/sessions", { host: host.value, repo: $("#g-new-repo").value.trim(), title: $("#g-new-title").value.trim(),
        base: $("#g-new-base").value.trim(), maxWorkers: parseInt($("#g-new-cap").value, 10) || 0 });
      location.href = r.url;
    } catch (err) {
      $("#g-new-error").textContent = err.message; $("#g-new-start").disabled = false;
    }
  });
}

if (root) bindSession(); else bindNew();
// for tests and the console
window.grimmLabels = { describeTool, grimmCalls, md };
})();
