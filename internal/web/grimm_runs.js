// Live workflow run approval & monitoring cards in the Grimm thread.
// State comes from the Grimm SSE stream's `workflow_runs` event (rows of grimm_workflow_run_status).
// Approve and Reject call the existing browser-only proposal routes (/api/workflow-proposals/{id}/approve|reject);
// Edit input calls /api/grimm/sessions/{id}/workflow-requests/{pid}/input (also browser-only).
"use strict";
(() => {
const runsByProposal = new Map();
const editState = new Map(); // proposal_id -> { open, draft, saving, error }
const busyState = new Map(); // proposal_id -> string ("Approving…", "Rejecting…")
const expandedInputs = new Set();

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function sessionID() {
  const root = document.getElementById("grimm");
  return root ? root.dataset.session || "" : "";
}

function shortTime(iso) {
  return iso ? new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "";
}

async function postJSON(path, body) {
  const r = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (r.status === 401) {
    location.href = "/login?next=" + encodeURIComponent(location.pathname);
    throw new Error("signed out");
  }
  const text = await r.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch (_) { data = { error: text }; }
  if (!r.ok) throw new Error(data.error || data.message || `${r.status} ${r.statusText}`);
  return data;
}

function statusMeta(r) {
  if (!r) return { cls: "pending", label: "Pending approval" };
  if (r.run_id && r.run_status) {
    const map = {
      queued: ["queued", "Queued"],
      finding_project: ["running", "Finding project"],
      starting: ["running", "Starting"],
      running: ["running", "Running"],
      waiting: ["waiting", "Waiting for you"],
      stalled: ["warn", "Stalled"],
      succeeded: ["done", "Succeeded"],
      failed: ["error", "Failed"],
      canceled: ["muted", "Canceled"],
    };
    const m = map[r.run_status] || ["muted", r.run_status];
    return { cls: m[0], label: m[1] };
  }
  const pmap = {
    pending: ["pending", "Pending approval"],
    applied: ["running", "Approved"],
    rejected: ["muted", "Rejected"],
    withdrawn: ["muted", "Withdrawn"],
    expired: ["muted", "Expired"],
    stale: ["warn", "Stale (workflow changed)"],
  };
  const m = pmap[r.proposal_status] || ["muted", r.proposal_status || "Pending"];
  return { cls: m[0], label: m[1] };
}

function stepBadge(s) {
  const cls = `gw-step gw-step-${esc(s.outcome || "pending")}` + (s.kind === "gate" || s.kind === "signoff" ? " gw-step-gate" : "");
  const icon = { passed: "✓", skipped: "–", current: "●", waiting: "?", failed: "✕", stalled: "!", canceled: "×", pending: "○" }[s.outcome] || "○";
  const kindTag = s.kind === "gate" ? "gate" : s.kind === "signoff" ? "sign-off" : "";
  return `<span class="${cls}" title="${esc(s.title)}${kindTag ? " (" + kindTag + ")" : ""}: ${esc(s.outcome)}"><i>${icon}</i>${esc(s.title || s.id)}${kindTag ? `<small>${kindTag}</small>` : ""}</span>`;
}

function populateCard(node, it, r) {
  const data = (it && it.data) || {};
  const pid = (r && r.proposal_id) || data.proposalId || "";
  const wfName = (r && r.workflow_name) || data.workflowName || "Workflow";
  const host = (r && r.host) || data.host || "";
  const summary = (r && r.summary) || (it && it.body) || "";
  const input = (r && r.input) || "";
  const st = statusMeta(r);
  const busy = busyState.get(pid) || "";
  const ed = editState.get(pid);
  const isPending = !r || r.proposal_status === "pending";

  node.className = `gw-card gw-st-${st.cls}`;
  node.dataset.proposalId = pid;

  let headRight = `<span class="gw-badge gw-badge-${st.cls}"><i></i>${esc(st.label)}</span>`;
  let links = "";
  if (r && r.run_id) {
    links = `<a class="gw-link" href="/workflow-runs/${esc(r.run_id)}">Open run →</a><a class="gw-link muted" href="/workflows">Workflows</a>`;
  } else if (pid) {
    links = `<a class="gw-link muted" href="/workflow-proposals/${esc(pid)}">Review proposal →</a>`;
  }

  let bodyHTML = "";
  if (summary) {
    bodyHTML += `<div class="gw-summary">${esc(summary)}</div>`;
  }

  // Input preview or inline editor
  if (ed && ed.open && isPending) {
    bodyHTML += `<div class="gw-edit">
      <label class="gw-edit-label">Task input</label>
      <textarea class="gw-edit-area" rows="5" ${ed.saving ? "disabled" : ""}>${esc(ed.draft)}</textarea>
      ${ed.error ? `<div class="gw-err" role="alert">${esc(ed.error)}</div>` : ""}
      <div class="gw-actions">
        <button type="button" class="btn sm primary gw-save-edit" ${ed.saving ? "disabled" : ""}>${ed.saving ? "Saving…" : "Save input"}</button>
        <button type="button" class="btn sm ghost gw-cancel-edit" ${ed.saving ? "disabled" : ""}>Cancel</button>
      </div>
    </div>`;
  } else if (input) {
    const long = input.length > 260 || input.split("\n").length > 4;
    const open = expandedInputs.has(pid);
    const shown = long && !open ? input.slice(0, 260) + "…" : input;
    bodyHTML += `<div class="gw-input-wrap"><pre class="gw-input">${esc(shown)}</pre>${long ? `<button type="button" class="gw-more">${open ? "Show less" : "Show full input"}</button>` : ""}</div>`;
  }

  // Live run progress: current step, gates, waiting/sign-off callout, error
  if (r && r.run_id) {
    if (r.step_count > 0 && r.run_step >= 0) {
      const idx = Math.min(r.run_step + 1, r.step_count);
      const kindSuffix = r.step_kind && r.step_kind !== "step" ? ` · ${r.step_kind === "signoff" ? "sign-off" : r.step_kind}` : "";
      bodyHTML += `<div class="gw-progress">
        <span class="gw-step-now">Step ${idx}/${r.step_count}: <b>${esc(r.step_title || r.step_id)}</b>${esc(kindSuffix)}</span>
        ${r.run_updated_at ? `<span class="gw-time">${esc(shortTime(r.run_updated_at))}</span>` : ""}
      </div>`;
    }
    const steps = Array.isArray(r.steps) ? r.steps : [];
    if (steps.length) {
      bodyHTML += `<div class="gw-steps">${steps.map(stepBadge).join("")}</div>`;
    }
    if (r.run_status === "waiting") {
      const heading = r.require_human_signoff || r.step_kind === "signoff" ? "Sign-off waiting on you" : "Question waiting on you";
      bodyHTML += `<div class="gw-callout gw-callout-wait">
        <div class="gw-callout-head"><b>${heading}</b><a class="btn sm primary" href="/workflow-runs/${esc(r.run_id)}">${r.require_human_signoff || r.step_kind === "signoff" ? "Review & sign off" : "Reply on run"}</a></div>
        ${r.run_note ? `<pre class="gw-callout-note">${esc(r.run_note)}</pre>` : (r.waiting_on ? `<p class="gw-callout-note">${esc(r.waiting_on)}</p>` : "")}
      </div>`;
    }
    if (r.run_error) {
      bodyHTML += `<div class="gw-callout gw-callout-err"><b>${r.run_status === "stalled" ? "Stalled" : "Error"}:</b> ${esc(r.run_error)}</div>`;
    }
  } else if (r && r.proposal_status === "stale") {
    bodyHTML += `<div class="gw-callout gw-callout-err">The workflow definition changed after this request. Ask Grimm to request it again.</div>`;
  }

  // Pending actions
  if (isPending && !(ed && ed.open) && pid) {
    bodyHTML += `<div class="gw-actions">
      <button type="button" class="btn sm primary gw-approve" ${busy ? "disabled" : ""}>${busy === "Approving…" ? "Approving…" : "Approve & start"}</button>
      <button type="button" class="btn sm ghost gw-edit-btn" ${busy ? "disabled" : ""}>Edit input</button>
      <button type="button" class="btn sm danger gw-reject" ${busy ? "disabled" : ""}>${busy === "Rejecting…" ? "Rejecting…" : "Reject"}</button>
    </div>`;
  }
  if (ed && ed.actionError) {
    bodyHTML += `<div class="gw-err" role="alert">${esc(ed.actionError)}</div>`;
  }

  node.innerHTML = `<div class="gw-head">
      <div class="gw-title-wrap">
        <span class="gw-ico" aria-hidden="true">⚡</span>
        <span class="gw-title">${esc(wfName)}</span>
        ${host ? `<code class="gw-host">${esc(host)}</code>` : ""}
      </div>
      ${headRight}
    </div>
    <div class="gw-body">${bodyHTML}</div>
    <div class="gw-foot">
      <span class="gw-meta">${r && r.decided_at ? `Decided ${esc(shortTime(r.decided_at))}` : (r && r.created_at ? `Requested ${esc(shortTime(r.created_at))}` : "Workflow request")}</span>
      <div class="gw-links">${links}</div>
    </div>`;

  // Wire buttons
  const moreBtn = node.querySelector(".gw-more");
  if (moreBtn) {
    moreBtn.addEventListener("click", () => {
      if (expandedInputs.has(pid)) expandedInputs.delete(pid); else expandedInputs.add(pid);
      populateCard(node, it, runsByProposal.get(pid) || r);
    });
  }
  const appBtn = node.querySelector(".gw-approve");
  if (appBtn) {
    appBtn.addEventListener("click", async () => {
      busyState.set(pid, "Approving…");
      editState.delete(pid);
      populateCard(node, it, runsByProposal.get(pid) || r);
      try {
        await postJSON(`/api/workflow-proposals/${encodeURIComponent(pid)}/approve`, {});
      } catch (e) {
        editState.set(pid, { open: false, actionError: e.message });
      } finally {
        busyState.delete(pid);
        populateCard(node, it, runsByProposal.get(pid) || r);
      }
    });
  }
  const rejBtn = node.querySelector(".gw-reject");
  if (rejBtn) {
    rejBtn.addEventListener("click", async () => {
      busyState.set(pid, "Rejecting…");
      editState.delete(pid);
      populateCard(node, it, runsByProposal.get(pid) || r);
      try {
        await postJSON(`/api/workflow-proposals/${encodeURIComponent(pid)}/reject`, {});
      } catch (e) {
        editState.set(pid, { open: false, actionError: e.message });
      } finally {
        busyState.delete(pid);
        populateCard(node, it, runsByProposal.get(pid) || r);
      }
    });
  }
  const editBtn = node.querySelector(".gw-edit-btn");
  if (editBtn) {
    editBtn.addEventListener("click", () => {
      editState.set(pid, { open: true, draft: input, saving: false, error: "" });
      populateCard(node, it, runsByProposal.get(pid) || r);
      const ta = node.querySelector(".gw-edit-area");
      if (ta) ta.focus();
    });
  }
  const cancelBtn = node.querySelector(".gw-cancel-edit");
  if (cancelBtn) {
    cancelBtn.addEventListener("click", () => {
      editState.delete(pid);
      populateCard(node, it, runsByProposal.get(pid) || r);
    });
  }
  const saveBtn = node.querySelector(".gw-save-edit");
  if (saveBtn) {
    saveBtn.addEventListener("click", async () => {
      const ta = node.querySelector(".gw-edit-area");
      const val = ta ? ta.value.trim() : "";
      if (!val) {
        editState.set(pid, { open: true, draft: ta ? ta.value : "", saving: false, error: "Input cannot be empty." });
        populateCard(node, it, runsByProposal.get(pid) || r);
        return;
      }
      editState.set(pid, { open: true, draft: val, saving: true, error: "" });
      populateCard(node, it, runsByProposal.get(pid) || r);
      try {
        await postJSON(`/api/grimm/sessions/${encodeURIComponent(sessionID())}/workflow-requests/${encodeURIComponent(pid)}/input`, { input: val });
        editState.delete(pid);
        const cur = runsByProposal.get(pid);
        if (cur) cur.input = val;
      } catch (e) {
        editState.set(pid, { open: true, draft: val, saving: false, error: e.message });
      }
      populateCard(node, it, runsByProposal.get(pid) || r);
    });
  }
}

const threadItemsByProposal = new Map();

function renderCard(it) {
  const pid = (it && it.data && it.data.proposalId) || "";
  if (pid) threadItemsByProposal.set(pid, it);
  const node = document.createElement("div");
  populateCard(node, it, runsByProposal.get(pid));
  return node;
}

function update(items) {
  for (const r of items || []) {
    if (!r || !r.proposal_id) continue;
    runsByProposal.set(r.proposal_id, r);
    const node = document.querySelector(`.gw-card[data-proposal-id="${CSS.escape(r.proposal_id)}"]`);
    if (node) {
      // Do not clobber an active inline edit textarea while typing.
      const ed = editState.get(r.proposal_id);
      if (ed && ed.open && r.proposal_status === "pending") continue;
      populateCard(node, threadItemsByProposal.get(r.proposal_id), r);
    }
  }
}

window.grimmRuns = { renderCard, update };
})();
