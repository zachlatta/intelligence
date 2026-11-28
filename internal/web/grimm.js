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
  t.textContent = msg; t.className = "g-toast " + (bad ? "bad" : "ok"); t.hidden = false;
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
          case "workflow": labels.push({ request: `Requested workflow ${flag(args, "--workflow") || ""}`.trim(), status: "Checked workflow status" }[args[0]] || "Listed workflow runs"); icon = "tool"; break;
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
function managerWorking() { return !isArchived() && ["working", "starting"].includes(S.session.manager_state); }
function isArchived() { return (S.session.status || root?.dataset.status) === "archived"; }

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
    img.addEventListener("click", () => {
      const box = el("div", "g-lightbox"); box.setAttribute("role", "dialog"); box.setAttribute("aria-label", name);
      const big = el("img"); big.src = img.src; big.alt = name;
      box.append(big); box.onclick = () => box.remove(); document.body.append(box);
    });
    holder.append(img);
  } else {
    const f = el("span", "g-file"); f.textContent = name; holder.append(f);
  }
}
function statusHTML(st) {
  if (st === "running") return '<span class="g-st running" aria-label="running"></span>';
  if (st === "failed" || st === "error") return '<span class="g-st error" aria-label="failed">✕</span>';
  if (st === "unknown") return '<span class="g-st unknown" aria-label="unknown">·</span>';
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
      node.dataset.icon = d.icon || "tool";
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
      node = el("div", "", `<div class="g-update" role="button" tabindex="0"><span class="g-uline">⚡ <b>Update</b> ${sums}</span></div><div class="g-update-detail" hidden><pre>${esc(it.body)}</pre></div>`);
      const toggle = () => { const dd = $(".g-update-detail", node); dd.hidden = !dd.hidden; };
      $(".g-update", node).addEventListener("click", toggle);
      $(".g-update", node).addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); } });
      break;
    }
    case "command":
      node = el("div", "g-command" + (data.status === "error" ? " error" : ""), `<div class="chead"><span>${esc(it.body)}</span>${statusHTML(data.status === "error" ? "error" : "done")}</div>${data.output ? `<pre>${esc(data.output)}</pre>` : ""}`);
      break;
    case "divider":
      node = el("div", "g-divider", esc(it.body));
      break;
    case "workflow_request":
      node = window.grimmRuns ? window.grimmRuns.renderCard(it) : el("div", "g-notice", esc(it.body));
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
  items = items || [];
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

// ---------- header, session picker, end dialog, worker cap popover, and manager ----------
function renderSession() {
  const s = S.session;
  if (!s.id) return;
  const archived = s.status === "archived";
  if (root) root.dataset.status = s.status || "active";
  $("#g-repo").textContent = s.title || s.repo;
  $("#g-branch").textContent = s.branch || "";
  const pickerLabel = $("#g-picker-label");
  if (pickerLabel) pickerLabel.textContent = `${s.title || s.repo}${s.host ? " · " + s.host : ""}`;
  $("#g-chip").title = `${s.repo} on ${s.host}\nbranch ${s.branch}${s.workspace_error ? "\nworktree failed: " + s.workspace_error : ""}`;
  $("#g-cap-live").textContent = s.live_workers ?? 0;
  $("#g-cap-max").textContent = s.max_workers;
  $("#g-cap").classList.toggle("full", !archived && (s.live_workers || 0) >= s.max_workers);
  $("#g-cap").disabled = archived;
  $("#g-cap").title = archived
    ? `${s.live_workers || 0} live workers of ${s.max_workers} (session ended).`
    : `${s.live_workers || 0} live workers of this session's cap of ${s.max_workers}. Click to change the cap.`;
  $("#g-end-session").hidden = archived;
  $("#g-archive-all").hidden = archived;
  $("#g-composer").hidden = archived;
  $("#g-ended-banner").hidden = !archived;
  $("#g-directive-form").hidden = archived;
  const branchNote = $("#g-ended-branch-note");
  if (branchNote) branchNote.innerHTML = s.branch ? ` Branch <code>${esc(s.branch)}</code> was kept.` : "";
  if (archived) {
    closeCapPopover(false);
    document.title = `Grimm · ${s.title || s.repo} (ended)`;
    const pill = $("#g-pill"); pill.dataset.state = "offline";
    $("#g-pill-label").textContent = "Session ended";
    pill.title = "This session has ended. Transcripts and roster are read-only.";
    $("#g-typing").hidden = true;
    $("#g-stop").hidden = true;
    const cur = $(`#g-items [data-id="${CSS.escape(lastAssistantID())}"] .g-md`);
    if (cur) cur.classList.remove("cursor");
    return;
  }
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
  const mgrAgent = s.manager_agent_id ? " · agent " + String(s.manager_agent_id).slice(0, 8) : "";
  const mgrTurns = s.manager_turns != null ? ` · ${s.manager_turns} turns` : "";
  pill.title = `Manager: ${s.manager_provider ? s.manager_provider + "/" + s.manager_model : "antigravity/gemini-4-neon-medium"}${s.manager_login ? " on " + s.manager_login : ""}${mgrAgent}${mgrTurns}${s.manager_error ? "\n" + s.manager_error : ""}`;
  $("#g-typing").hidden = !working;
  $("#g-typing-label").textContent = st === "starting" ? "Starting Grimm…" : "Grimm is working" + (s.manager_turn_at ? " · " + since(s.manager_turn_at) : "");
  $("#g-stop").hidden = st !== "working";
  const cur = $(`#g-items [data-id="${CSS.escape(lastAssistantID())}"] .g-md`);
  if (cur) cur.classList.toggle("cursor", st === "working");
}

function openPicker() {
  const pop = $("#g-picker-pop"), btn = $("#g-picker-btn");
  if (!pop || !btn) return;
  closeCapPopover(false);
  pop.hidden = false;
  btn.setAttribute("aria-expanded", "true");
  if (matchMedia("(max-width: 780px)").matches) {
    const r = btn.getBoundingClientRect();
    pop.style.top = Math.round(r.bottom + 6) + "px";
  } else {
    pop.style.top = "";
  }
}
function closePicker(refocusBtn) {
  const pop = $("#g-picker-pop"), btn = $("#g-picker-btn");
  if (!pop || pop.hidden) return;
  pop.hidden = true;
  btn.setAttribute("aria-expanded", "false");
  if (refocusBtn) btn.focus();
}

let endingSession = false;
function openEndDialog() {
  if (isArchived()) return;
  const dlg = $("#g-end-dialog");
  if (!dlg) return;
  closePicker(false);
  closeCapPopover(false);
  const live = S.workers.filter((w) => w.status === "starting" || w.status === "running" || w.status === "idle").length;
  const lead = $("#g-end-lead");
  if (lead) {
    lead.textContent = live === 0
      ? "Stops Grimm's turn (0 live workers), archives its Paseo agent, and removes this session from the active picker."
      : `Stops Grimm's turn and ${live} live ${live === 1 ? "worker" : "workers"}, archives their Paseo agents, and removes this session from the active picker.`;
  }
  const br = $("#g-end-branch");
  if (br) br.textContent = S.session.branch || "grimm/…";
  $("#g-end-error").textContent = "";
  dlg.hidden = false;
  $("#g-end-cancel").focus();
}
function closeEndDialog(refocusBtn) {
  if (endingSession) return;
  const dlg = $("#g-end-dialog");
  if (!dlg || dlg.hidden) return;
  dlg.hidden = true;
  $("#g-end-error").textContent = "";
  if (refocusBtn) $("#g-end-session").focus();
}
async function confirmEndSession() {
  if (endingSession) return;
  const err = $("#g-end-error"), confirmBtn = $("#g-end-confirm"), cancelBtn = $("#g-end-cancel");
  err.textContent = "";
  endingSession = true;
  confirmBtn.disabled = true;
  cancelBtn.disabled = true;
  confirmBtn.textContent = "Ending session…";
  try {
    const r = await api("POST", `/api/grimm/sessions/${S.id}/archive`, {});
    location.href = (r && r.nextUrl) || "/";
  } catch (e) {
    err.textContent = e.message;
    endingSession = false;
    confirmBtn.disabled = false;
    cancelBtn.disabled = false;
    confirmBtn.textContent = "End session";
  }
}

let capSaving = false;
function capLimit() { return +root.dataset.capLimit || 100; }
function openCapPopover() {
  const pop = $("#g-cap-pop"), btn = $("#g-cap"), inp = $("#g-cap-input"), err = $("#g-cap-error");
  if (!pop) return;
  err.textContent = "";
  inp.value = String(S.session.max_workers || 20);
  pop.hidden = false;
  btn.setAttribute("aria-expanded", "true");
  if (matchMedia("(max-width: 780px)").matches) {
    const r = btn.getBoundingClientRect();
    pop.style.top = Math.round(r.bottom + 6) + "px";
  } else {
    pop.style.top = "";
  }
  inp.focus();
  inp.select();
}
function closeCapPopover(refocusBtn) {
  const pop = $("#g-cap-pop"), btn = $("#g-cap");
  if (!pop || pop.hidden) return;
  pop.hidden = true;
  btn.setAttribute("aria-expanded", "false");
  $("#g-cap-error").textContent = "";
  if (refocusBtn) btn.focus();
}
async function saveCapPopover() {
  if (capSaving) return;
  const pop = $("#g-cap-pop"), inp = $("#g-cap-input"), err = $("#g-cap-error"), saveBtn = $("#g-cap-save");
  if (!pop || pop.hidden) return;
  const limit = capLimit();
  const raw = inp.value.trim();
  const n = Number(raw);
  if (!raw || !Number.isInteger(n) || n < 1 || n > limit) {
    err.textContent = `Enter a whole number from 1 to ${limit}.`;
    return;
  }
  err.textContent = "";
  if (n === Number(S.session.max_workers)) {
    closeCapPopover(false);
    return;
  }
  capSaving = true;
  saveBtn.disabled = true;
  try {
    await api("PUT", `/api/grimm/sessions/${S.id}`, { maxWorkers: n });
    S.session.max_workers = n;
    renderSession();
    closeCapPopover(false);
    toast(`Worker cap set to ${n}.`);
  } catch (e) {
    err.textContent = e.message;
  } finally {
    capSaving = false;
    saveBtn.disabled = false;
  }
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
  const archived = isArchived();
  for (const d of S.directives) {
    const li = el("li"); li.value = d.seq; li.textContent = d.text;
    if (!archived) {
      const b = el("button", "", "×"); b.type = "button"; b.title = `Remove directive ${d.seq}`; b.setAttribute("aria-label", `Remove directive ${d.seq}`);
      b.addEventListener("click", async () => {
        if (!confirm(`Remove directive ${d.seq}?\n\n${d.text}`)) return;
        try { await api("DELETE", `/api/grimm/sessions/${S.id}/directives/${d.seq}`); } catch (e) { toast(e.message, true); }
      });
      li.append(b);
    }
    ol.append(li);
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
  const archived = isArchived();
  $("#g-focus-name").textContent = w.name;
  $("#g-focus-sub").textContent = `${w.role} · ${PROVIDER[w.provider] || w.provider} ${w.model} · ${w.login}`;
  const pill = $("#g-focus-status"); pill.className = `g-status ${w.status}`; pill.textContent = w.status;
  $("#g-focus-id").textContent = w.agent_id ? "agent " + w.agent_id.slice(0, 8) : "";
  $("#g-focus-form").hidden = archived;
  $("#g-focus-stop").hidden = archived;
  $("#g-focus-archive").hidden = archived;
  $("#g-focus-archive").disabled = archived || w.status === "archived";
  $("#g-focus-stop").disabled = archived || w.status !== "running";
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
    return `<span class="${failed ? "t-err" : "t-tool"}">[${esc(failed ? "Failed " + d.info.name : d.info.name)}]</span> ${esc(String(what).split("\n")[0].slice(0, 400))}`;
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
  es.onopen = () => { conn.classList.add("on"); conn.title = "Live connection"; conn.setAttribute("aria-label", "Live"); };
  es.onerror = () => { conn.classList.remove("on"); conn.title = "Reconnecting…"; conn.setAttribute("aria-label", "Reconnecting"); };
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
    upsertItems(d.items || [], d.reset);
    if (d.reset) { S.older = !!d.more; $("#g-older").hidden = !S.older; }
  });
  es.addEventListener("bus", (e) => addBus(JSON.parse(e.data).items || []));
  es.addEventListener("activity", (e) => addFeed(JSON.parse(e.data).items || []));
  es.addEventListener("workflow_runs", (e) => { if (window.grimmRuns) window.grimmRuns.update(JSON.parse(e.data).items || []); });
  es.addEventListener("host_resources", (e) => document.dispatchEvent(new CustomEvent("grimm:host-resources", { detail: JSON.parse(e.data) })));
}

// ---------- composer & inline attachment drafts ----------
const MAX_ATTACHMENTS = 8;
const MAX_FILE_BYTES = 4 * 1024 * 1024;
const MAX_TOTAL_BYTES = 8 * 1024 * 1024;
const drafts = [];
let checking = 0;
let sending = false;

function draftStorageKey() { return S.id ? `grimm:draft:${S.id}` : ""; }
function saveTextDraft(val) {
  try {
    const k = draftStorageKey();
    if (!k) return;
    if (val) sessionStorage.setItem(k, val); else sessionStorage.removeItem(k);
  } catch (_) { /* ignore storage quota/privacy errors */ }
}
function loadTextDraft() {
  try {
    const k = draftStorageKey();
    return k ? (sessionStorage.getItem(k) || "") : "";
  } catch (_) { return ""; }
}

function attachStatus(msg, isError) {
  const s = $("#g-attach-status");
  if (!s) return;
  s.textContent = msg || "";
  s.classList.toggle("is-error", !!isError);
}
function showAttachCount() {
  const n = drafts.length;
  if (!n) { attachStatus(""); return; }
  const noun = drafts.every((d) => d.isImage) ? (n === 1 ? "image" : "images") : (n === 1 ? "file" : "files");
  attachStatus(`${n} ${noun} ready · ${MAX_ATTACHMENTS} max per message`);
}
function fileBadge(name) {
  const dot = name.lastIndexOf(".");
  const ext = dot > 0 ? name.slice(dot + 1).replace(/[^a-zA-Z0-9]/g, "").toUpperCase().slice(0, 4) : "";
  return ext || "FILE";
}
function fileLabel(file, detectedType) {
  const raw = (file.name || "").trim();
  if (raw) return raw;
  const ext = { "image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp" }[detectedType || file.type];
  return ext ? `pasted-image.${ext}` : "pasted-file";
}
async function detectImageType(file) {
  try {
    const header = new Uint8Array(await file.slice(0, 12).arrayBuffer());
    const at = (off, ...bytes) => bytes.every((b, i) => header[off + i] === b);
    if (at(0, 0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a)) return "image/png";
    if (at(0, 0xff, 0xd8, 0xff)) return "image/jpeg";
    if (at(0, 0x47, 0x49, 0x46, 0x38) && (header[4] === 0x37 || header[4] === 0x39) && header[5] === 0x61) return "image/gif";
    if (at(0, 0x52, 0x49, 0x46, 0x46) && at(8, 0x57, 0x45, 0x42, 0x50)) return "image/webp";
  } catch (_) { /* unreadable */ }
  return "";
}
function renderDrafts() {
  const list = $("#g-attach-drafts");
  if (!list) return;
  list.hidden = !drafts.length;
  list.replaceChildren();
  drafts.forEach((d, i) => {
    const li = el("li", "g-attach-draft"); li.title = d.name;
    if (d.isImage && d.preview) {
      const img = el("img"); img.src = d.preview; img.alt = d.name;
      li.append(img);
    } else {
      const box = el("div", "g-attach-draft-file", `<b>${esc(fileBadge(d.name))}</b><span>${esc(d.name)}</span>`);
      li.append(box);
    }
    const rm = el("button", "g-draft-remove", "×");
    rm.type = "button"; rm.disabled = sending;
    rm.setAttribute("aria-label", `Remove ${d.name}`);
    rm.addEventListener("click", () => {
      if (sending) return;
      if (d.preview) URL.revokeObjectURL(d.preview);
      drafts.splice(i, 1);
      renderDrafts();
      showAttachCount();
      const buttons = $$(".g-draft-remove", list);
      (buttons[Math.min(i, buttons.length - 1)] || $("#g-attach-btn")).focus();
    });
    li.append(rm);
    list.append(li);
  });
}
async function addFiles(fileList) {
  const incoming = Array.from(fileList || []);
  if (sending || !incoming.length) return;
  checking++;
  $("#g-send").disabled = true;
  const errors = new Set();
  try {
    for (const file of incoming) {
      if (drafts.length >= MAX_ATTACHMENTS) {
        errors.add(`Attach at most ${MAX_ATTACHMENTS} files per message.`);
        break;
      }
      if (!file.size || file.size > MAX_FILE_BYTES) {
        errors.add("Files must be nonempty and 4 MiB or smaller.");
        continue;
      }
      const total = drafts.reduce((sum, d) => sum + d.file.size, 0);
      if (total + file.size > MAX_TOTAL_BYTES) {
        errors.add("Total attachments must be 8 MiB or smaller.");
        continue;
      }
      const imgType = await detectImageType(file);
      const isImage = !!imgType;
      const name = fileLabel(file, imgType);
      const preview = isImage ? URL.createObjectURL(file.type === imgType ? file : file.slice(0, file.size, imgType)) : "";
      drafts.push({ file, name, isImage, preview, reference: null });
    }
    renderDrafts();
    if (errors.size) attachStatus(`${drafts.length} ready. ${[...errors].join(" ")}`, true);
    else showAttachCount();
  } finally {
    $("#g-send").disabled = --checking > 0;
  }
}
function readFileBase64(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("Could not read file."));
    reader.onload = () => resolve(String(reader.result || "").split(",")[1] || "");
    reader.readAsDataURL(file);
  });
}
async function uploadDrafts(rawText) {
  for (let i = 0; i < drafts.length; i++) {
    const d = drafts[i];
    if (d.reference) continue;
    attachStatus(`Uploading ${d.isImage ? "image" : "file"} ${i + 1} of ${drafts.length}…`);
    const data = await readFileBase64(d.file);
    const res = await api("POST", "/api/attachments", { name: d.name, data });
    d.reference = res.reference;
  }
  if (!drafts.length) return rawText;
  const heading = drafts.every((d) => d.isImage) ? "Attached images:" : "Attached files:";
  return (rawText ? rawText + "\n\n" : "") + heading + "\n" + drafts.map((d) => d.reference).join("\n");
}
function setSending(active) {
  sending = active;
  $("#g-send").disabled = active || checking > 0;
  $("#g-attach-btn").disabled = active;
  $("#g-input").readOnly = active;
  $$(".g-draft-remove", $("#g-attach-drafts")).forEach((b) => { b.disabled = active; });
}

function autosize() {
  const t = $("#g-input");
  t.style.height = "auto";
  t.style.height = Math.min(216, t.scrollHeight) + "px";
}
async function send() {
  const input = $("#g-input");
  const raw = input.value.trim();
  if (sending || checking || (!raw && !drafts.length)) return;
  const refocus = document.activeElement === input || document.activeElement === $("#g-send");
  setSending(true);
  try {
    const text = await uploadDrafts(raw);
    if (drafts.length) attachStatus("Sending message…");
    await api("POST", `/api/grimm/sessions/${S.id}/messages`, { text });
    for (const d of drafts) if (d.preview) URL.revokeObjectURL(d.preview);
    drafts.length = 0;
    input.value = "";
    saveTextDraft("");
    autosize();
    renderDrafts();
    attachStatus("");
    scrollBottom(true);
  } catch (e) {
    if (drafts.length) attachStatus(e.message, true);
    else toast(e.message, true);
  } finally {
    setSending(false);
    if (refocus && (document.activeElement === document.body || document.activeElement === input)) input.focus();
  }
}

function bindSession() {
  const composer = $("#g-composer");
  const input = $("#g-input");
  const picker = $("#g-attach-picker");

  // restore any unsent text draft for this session
  const saved = loadTextDraft();
  if (saved) { input.value = saved; autosize(); }

  composer.addEventListener("submit", (e) => { e.preventDefault(); send(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !matchMedia("(pointer: coarse)").matches) { e.preventDefault(); send(); }
  });
  input.addEventListener("input", () => { autosize(); saveTextDraft(input.value); });

  $("#g-attach-btn").addEventListener("click", () => { if (!sending) picker.click(); });
  picker.addEventListener("change", () => { addFiles(picker.files); picker.value = ""; });
  composer.addEventListener("paste", (e) => {
    const cb = e.clipboardData;
    if (!cb) return;
    let files = Array.from(cb.items || []).filter((it) => it.kind === "file").map((it) => it.getAsFile()).filter(Boolean);
    if (!files.length && cb.files && cb.files.length) files = Array.from(cb.files);
    if (!files.length) return;
    e.preventDefault();
    addFiles(files);
  });
  let dragDepth = 0;
  composer.addEventListener("dragenter", (e) => {
    if (!e.dataTransfer?.types.includes("Files")) return;
    e.preventDefault();
    dragDepth++;
    if (!sending) composer.classList.add("is-dragging");
  });
  composer.addEventListener("dragover", (e) => {
    if (!e.dataTransfer?.types.includes("Files")) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = sending ? "none" : "copy";
  });
  composer.addEventListener("dragleave", () => {
    dragDepth = Math.max(0, dragDepth - 1);
    if (!dragDepth) composer.classList.remove("is-dragging");
  });
  composer.addEventListener("drop", (e) => {
    if (!e.dataTransfer?.types.includes("Files")) return;
    e.preventDefault();
    dragDepth = 0;
    composer.classList.remove("is-dragging");
    addFiles(e.dataTransfer.files);
  });
  window.addEventListener("pagehide", () => { for (const d of drafts) if (d.preview) URL.revokeObjectURL(d.preview); });

  $("#g-stop").addEventListener("click", async () => { try { await api("POST", `/api/grimm/sessions/${S.id}/manager/interrupt`, {}); } catch (e) { toast(e.message, true); } });
  $("#g-suggest").addEventListener("click", (e) => {
    const b = e.target.closest("button");
    if (b && !sending) { input.value = b.textContent; saveTextDraft(input.value); autosize(); input.focus(); }
  });
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
    toast("Archiving… (this takes ~20 s per agent, in parallel)");
    try { const r = await api("POST", `/api/grimm/sessions/${S.id}/archive-workers`, {}); toast(`Archived ${r.archived.length}: ${r.archived.join(", ") || "none"}`); } catch (e) { toast(e.message, true); }
  });

  // inline worker cap popover/stepper
  $("#g-cap").addEventListener("click", () => {
    if ($("#g-cap-pop").hidden) openCapPopover(); else closeCapPopover(false);
  });
  $("#g-cap-dec").addEventListener("click", () => {
    const inp = $("#g-cap-input");
    const cur = parseInt(inp.value, 10) || S.session.max_workers || 20;
    inp.value = String(Math.max(1, cur - 1));
    $("#g-cap-error").textContent = "";
    inp.focus();
  });
  $("#g-cap-inc").addEventListener("click", () => {
    const inp = $("#g-cap-input");
    const cur = parseInt(inp.value, 10) || S.session.max_workers || 20;
    inp.value = String(Math.min(capLimit(), cur + 1));
    $("#g-cap-error").textContent = "";
    inp.focus();
  });
  $("#g-cap-save").addEventListener("click", saveCapPopover);
  $("#g-cap-input").addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); saveCapPopover(); }
    else if (e.key === "Escape") { e.preventDefault(); closeCapPopover(true); }
  });
  $("#g-cap-input").addEventListener("input", () => { $("#g-cap-error").textContent = ""; });
  $("#g-cap-pop").addEventListener("focusout", (e) => {
    if ($("#g-cap-pop").hidden) return;
    const next = e.relatedTarget;
    if (next && $("#g-cap-wrap").contains(next)) return;
    // blur outside popover saves valid changes (or closes if unchanged)
    setTimeout(() => {
      if ($("#g-cap-pop").hidden) return;
      if ($("#g-cap-wrap").contains(document.activeElement)) return;
      saveCapPopover();
    }, 0);
  });
  document.addEventListener("mousedown", (e) => {
    if ($("#g-cap-pop").hidden) return;
    if (!$("#g-cap-wrap").contains(e.target)) saveCapPopover();
  });

  // session picker popover
  $("#g-picker-btn").addEventListener("click", () => {
    if ($("#g-picker-pop").hidden) openPicker(); else closePicker(false);
  });
  document.addEventListener("mousedown", (e) => {
    if (!$("#g-picker-pop").hidden && !$("#g-picker-wrap").contains(e.target)) closePicker(false);
  });

  // end session confirm dialog
  $("#g-end-session").addEventListener("click", openEndDialog);
  $("#g-end-cancel").addEventListener("click", () => closeEndDialog(true));
  $("#g-end-confirm").addEventListener("click", confirmEndSession);
  $("#g-end-dialog").addEventListener("click", (e) => {
    if (e.target === $("#g-end-dialog")) closeEndDialog(true);
  });

  $("#g-older").addEventListener("click", loadOlder);
  $("#g-thread").addEventListener("scroll", () => { if ($("#g-thread").scrollTop < 80) loadOlder(); }, { passive: true });
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
    toast(`Archiving ${name}…`);
    try { await api("POST", `/api/grimm/sessions/${S.id}/workers/${encodeURIComponent(name)}/archive`, {}); toast(`Archived ${name}.`); } catch (e) { toast(e.message, true); }
  });
  $("#g-tabs").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) setTab(b.dataset.tab); });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      const lb = $(".g-lightbox");
      if (lb) { lb.remove(); return; }
      if (!$("#g-end-dialog").hidden) { closeEndDialog(true); return; }
      if (!$("#g-picker-pop").hidden) { closePicker(true); return; }
      if (!$("#g-cap-pop").hidden) { closeCapPopover(true); return; }
      if (S.focus && !e.target.closest(".g-box")) { closeFocus(); return; }
    }
    if (e.key === "/" && !isArchived() && (document.activeElement === document.body || !document.activeElement)) { e.preventDefault(); input.focus(); }
  });
  setInterval(() => { renderSession(); if (S.workers.some((w) => w.status === "running")) renderTeam(); }, 1000);
  if (matchMedia("(max-width: 780px)").matches) input.placeholder = "Tell Grimm what to do…";
  connect();
  if (!matchMedia("(pointer: coarse)").matches && !isArchived()) input.focus();
}

function bindNew() {
  const form = $("#g-new"); if (!form) return;
  const host = $("#g-new-host"), list = $("#g-new-repos"), repo = $("#g-new-repo");
  // The repository is the host's desk checkout unless the user typed another; a host without desk
  // leaves it blank to fill in.
  let auto = "";
  const fill = () => {
    const opt = host.selectedOptions[0];
    list.innerHTML = "";
    for (const r of (opt?.dataset.repos || "").split("\n").filter(Boolean)) { const o = document.createElement("option"); o.value = r; list.append(o); }
    const desk = opt?.dataset.desk || "";
    if (repo.value === auto) { repo.value = desk; auto = desk; }
    repo.placeholder = desk ? desk : "/path/to/repo";
  };
  host.addEventListener("change", fill); fill();
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#g-new-error").textContent = ""; $("#g-new-start").disabled = true;
    try {
      const r = await api("POST", "/api/grimm/sessions", { host: host.value, repo: repo.value.trim(), title: $("#g-new-title").value.trim(),
        base: $("#g-new-base").value.trim(), maxWorkers: parseInt($("#g-new-cap").value, 10) || 0 });
      location.href = r.url;
    } catch (err) {
      $("#g-new-error").textContent = err.message; $("#g-new-start").disabled = false;
    }
  });
}

if (root) bindSession(); else bindNew();
// for tests and the console
window.grimmLabels = { describeTool, grimmCalls, md, statusHTML };
})();