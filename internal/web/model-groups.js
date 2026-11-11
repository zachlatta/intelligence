// Shared group editor and token chips. Reads use SQL; all changes use JSON APIs.
"use strict";
(() => {
  const esc = (s) =>
    String(s ?? "").replace(
      /[&<>"']/g,
      (c) =>
        ({
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          '"': "&quot;",
          "'": "&#39;",
        })[c],
    );
  const token = (g) => "{{" + g.token + "}}";
  let groups = [],
    members = [],
    catalog = [],
    windows = [],
    candidates = [],
    host = "";
  async function sql(query) {
    const r = await fetch("/api/sql", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query, limit: 10000 }),
    });
    const b = await r.json();
    if (!r.ok) throw Error(b.error);
    if (b.truncated)
      throw Error("Too many model entries to show. Narrow the catalog first.");
    return b.rows.map((row) =>
      Object.fromEntries(b.columns.map((c, i) => [c.name, row[i]])),
    );
  }
  async function write(path, method, body) {
    const r = await fetch(path, {
      method,
      headers: { "Content-Type": "application/json" },
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!r.ok) throw Error(await r.text());
    return r.status === 204 ? null : r.json();
  }
  const root = document.createElement("section");
  root.id = "model-groups";
  root.className = "card";
  const pageHead = document.querySelector("main .page-head");
  if (!pageHead || location.pathname.startsWith("/workflow-runs/")) return;
  const plans = location.pathname === "/usage";
  if (plans) pageHead.after(root);
  else {
    const launch = document.createElement("button");
    launch.className = "btn sm";
    launch.type = "button";
    launch.textContent = "Model groups";
    (pageHead.querySelector(".actions") || pageHead).append(launch);
    const sheet = document.createElement("dialog");
    sheet.style.width = "min(58rem,94vw)";
    sheet.append(root);
    document.body.append(sheet);
    launch.onclick = openGroups;
    const close = document.createElement("button");
    close.className = "btn sm";
    close.textContent = "Close";
    close.style.margin = "1rem";
    close.onclick = () => sheet.close();
    sheet.append(close);
  }
  const dialog = document.createElement("dialog");
  dialog.style.cssText =
    "width:min(64rem,94vw);max-height:90vh;overflow:auto;border:1px solid var(--line-strong);border-radius:6px;background:var(--panel);color:var(--fg);padding:1rem";
  document.body.append(dialog);
  async function load() {
    [groups, catalog, windows, candidates] = await Promise.all([
      sql(
        "SELECT g.id,g.name,g.token,g.revision,coalesce((SELECT jsonb_agg(to_jsonb(m) ORDER BY position) FROM model_group_members m WHERE m.group_id=g.id),'[]'::jsonb) AS members,(SELECT count(*) FROM workflows w JOIN workflow_versions v ON v.workflow_id=w.id AND v.version=w.version WHERE w.deleted_at IS NULL AND position('{{'||g.token||'}}' in v.definition::text)>0) AS used_by FROM model_groups g WHERE g.deleted_at IS NULL ORDER BY g.name,g.id",
      ),
      sql(
        "SELECT DISTINCT host,provider,id,label,thinking,default_thinking FROM provider_models ORDER BY provider,id,host",
      ),
      sql(
        "SELECT a.provider,a.account,a.label,c.window_id,c.window_seconds FROM usage_accounts a LEFT JOIN usage_current c ON c.provider=a.provider AND c.account=a.account ORDER BY a.provider,a.account,c.window_id",
      ),
      sql(
        "SELECT group_id,host,provider,model,thinking,login,eligible,reason,score,last_observed,position FROM model_group_candidates ORDER BY eligible DESC,score DESC NULLS LAST,position",
      ),
    ]);
    // A group's revision and editable members must come from the same SQL snapshot.
    members = groups.flatMap((g) =>
      g.members.map((m) => ({
        group_id: m.group_id,
        provider: m.provider,
        model: m.model,
        thinking: m.thinking,
        login: m.login,
        longWindow: m.long_window,
        gateWindows: m.gate_windows,
        eligibleUntil: m.eligible_until,
        enabled: m.enabled,
      })),
    );
    const hosts = [...new Set(catalog.map((c) => c.host))];
    if (!hosts.includes(host)) host = hosts[0] || "";
    render();
    chips();
  }
  async function openGroups() {
    try {
      // Refresh shared membership, revisions and usage counts whenever the manager opens.
      await load();
      const sheet = root.closest("dialog");
      if (sheet) sheet.showModal();
      else root.scrollIntoView();
    } catch {
      root.textContent = "Could not load model groups. Please reload.";
    }
  }
  function chips() {
    document.querySelectorAll("[data-model-token-chips]").forEach((el) => {
      const signature = groups.map((g) => g.id + g.token + g.name).join("|");
      if (el.dataset.loaded === signature) return;
      el.dataset.loaded = signature;
      el.innerHTML =
        '<p class="meta">Model groups · insert a token or <button type="button" class="btn sm" data-manage>edit groups</button></p><div style="display:flex;gap:.4rem;flex-wrap:wrap">' +
        groups
          .map(
            (g) =>
              `<button type="button" class="btn sm" data-token="${esc(token(g))}" title="${esc(g.name)}">${esc(token(g))}</button>`,
          )
          .join("") +
        "</div>";
      el.querySelector("[data-manage]").onclick = openGroups;
      el.querySelectorAll("[data-token]").forEach(
        (b) =>
          (b.onclick = () => {
            const t = el.parentElement.querySelector(
              'textarea[data-s="instructions"]',
            );
            if (!t) return;
            t.setRangeText(
              b.dataset.token,
              t.selectionStart,
              t.selectionEnd,
              "end",
            );
            t.focus();
            t.dispatchEvent(new Event("input", { bubbles: true }));
          }),
      );
    });
  }
  new MutationObserver(chips).observe(document.querySelector("main"), {
    subtree: true,
    childList: true,
  });
  function loginLabel(provider, login) {
    return (
      windows.find((w) => w.provider === provider && w.account === login)
        ?.label || login
    );
  }
  function render() {
    root.innerHTML = `<div class="card-head"><h2>Model groups</h2><span class="grow"></span><select aria-label="Preview host" style="width:auto" data-host>${[...new Set(catalog.map((c) => c.host))].map((h) => `<option ${h === host ? "selected" : ""}>${esc(h)}</option>`).join("")}</select><button type="button" class="btn sm" data-new>+ New group</button></div><div class="card-body"><p class="meta">Each group has its own token. Shared edits apply to future suggestions; renames keep the token.</p>${
      groups.length
        ? groups
            .map((g) => {
              const cs = candidates.filter(
                  (c) => c.group_id === g.id && c.host === host,
                ),
                best = cs.find((c) => c.eligible),
                empty = !members.some((m) => m.group_id === g.id);
              return `<section class="card"><div class="card-head"><b>${esc(g.name)}</b><code>${esc(token(g))}</code><span class="grow"></span><button type="button" class="btn sm" data-copy="${esc(token(g))}">Copy</button><button type="button" class="btn sm" data-edit="${g.id}">Edit</button></div><div class="card-body"><p>${empty ? '<span class="badge orange">Empty group</span> <span class="muted">Add a model before using this token.</span>' : best ? `<span class="badge blue">Suggested now</span> ${esc(best.provider + "/" + best.model)}${best.thinking ? " · " + esc(best.thinking) : ""}<span class="meta"> · ${esc(loginLabel(best.provider, best.login))} · meter ${Math.max(0, Math.floor((Date.now() - Date.parse(best.last_observed)) / 60000))} min old</span><br><span class="meta">More quota likely to remain at reset.</span>` : '<span class="muted">No eligible model. Check logins, meters and quota.</span>'}</p><details><summary class="meta">Members and alternatives (${members.filter((m) => m.group_id === g.id).length})</summary>${cs.length ? cs.map((c) => `<p>${esc(c.provider + "/" + c.model)} · ${esc(c.thinking || "effort in model ID / default")} · ${esc(loginLabel(c.provider, c.login))} <span class="badge">${esc(c.eligible ? "Available" : c.reason || "Quota reserved for recent suggestions")}</span></p>`).join("") : "<p>No candidates on this host.</p>"}</details></div></section>`;
            })
            .join("")
        : '<p class="muted">Create Frontier, Fast, or any other group. Then insert its token into a workflow step.</p>'
    }</div>`;
    root.querySelector("[data-new]").onclick = () => edit();
    root.querySelector("[data-host]").onchange = (e) => {
      host = e.target.value;
      render();
    };
    root
      .querySelectorAll("[data-edit]")
      .forEach(
        (b) =>
          (b.onclick = () => edit(groups.find((g) => g.id === b.dataset.edit))),
      );
    root.querySelectorAll("[data-copy]").forEach(
      (b) =>
        (b.onclick = async () => {
          try {
            await navigator.clipboard.writeText(b.dataset.copy);
            b.textContent = "Copied";
          } catch {
            b.textContent = "Select token to copy";
          }
        }),
    );
  }
  function edit(group) {
    const g = structuredClone(group || { name: "", revision: 0 });
    g.members = structuredClone(members.filter((m) => m.group_id === g.id));
    dialog.innerHTML = `<h2>${g.id ? "Edit" : "New"} model group</h2><form><label>Name<input name="name" maxlength="100" required value="${esc(g.name)}"></label><p class="meta">Token: <code data-token-preview></code> <button type="button" class="btn sm" data-copy-token>Copy</button><br>Generated on creation. Kept on rename so existing workflows still work.</p><div data-rows></div><button type="button" class="btn sm" data-add>+ Add model</button><p class="meta">Each login is a separate plan. Models sharing a quota pool share its allocation penalty. Inactive logins stay visible but are not suggested.</p><p class="banner" data-error hidden></p><p class="meta">${g.id ? `Used by ${g.used_by || 0} workflows. Edits affect their future suggestions.` : "You can create more groups, each with its own token."}</p><div class="form-actions"><button class="btn primary" type="submit">Save group</button><button class="btn" type="button" data-cancel>Cancel</button>${g.id ? '<button class="btn danger" type="button" data-delete>Delete group</button>' : ""}</div></form>`;
    const form = dialog.querySelector("form"),
      error = dialog.querySelector("[data-error]");
    const showError = (e) => {
      error.hidden = false;
      error.textContent = e.message;
    };
    const preview = () => {
      let b = form.elements.name.value
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "_")
        .replace(/^_+|_+$/g, "")
        .slice(0, 48)
        .replace(/_+$/g, "");
      if (!/^[a-z]/.test(b)) b = "group_" + b;
      b = b.replace(/_+$/g, "");
      dialog.querySelector("[data-token-preview]").textContent =
        token(g.id ? g : { token: b + "_model" }) +
        (g.id ? "" : " (a suffix is added if already used)");
    };
    form.elements.name.oninput = preview;
    preview();
    dialog.querySelector("[data-copy-token]").onclick = async () => {
      try {
        await navigator.clipboard.writeText(
          g.token
            ? token(g)
            : dialog
                .querySelector("[data-token-preview]")
                .textContent.split(" ")[0],
        );
      } catch (e) {
        showError(e);
      }
    };
    function quotaWindows(m) {
      const prefix =
        m.provider === "antigravity"
          ? m.model.startsWith("gemini-")
            ? "gemini/"
            : "claude-gpt/"
          : "";
      const observed = windows.filter(
        (w) =>
          w.provider === m.provider &&
          w.account === m.login &&
          w.window_id &&
          w.window_id.startsWith(prefix),
      );
      return observed.length
        ? observed
        : ["7d", "5h"].map((id) => ({ window_id: prefix + id }));
    }
    function rows() {
      const models = [
        ...new Map(catalog.map((c) => [c.provider + "/" + c.id, c])).values(),
      ];
      dialog.querySelector("[data-rows]").innerHTML =
        g.members
          .map((m, i) => {
            const cat = models.find(
                (c) => c.provider === m.provider && c.id === m.model,
              ),
              levels = [
                ...new Set(
                  catalog
                    .filter(
                      (c) => c.provider === m.provider && c.id === m.model,
                    )
                    .flatMap((c) => c.thinking || []),
                ),
              ];
            const logins = [
              ...new Map(
                windows
                  .filter((w) => w.provider === m.provider)
                  .map((w) => [w.account, w]),
              ).values(),
            ];
            if (!m.thinking && levels.length)
              m.thinking = cat?.default_thinking || levels[0];
            const ws = quotaWindows(m).filter((w) =>
              w.window_id.endsWith("7d"),
            );
            return `<fieldset data-row="${i}"><legend>Member ${i + 1}</legend><div class="fields"><div><label>Model<select data-field="model">${!cat ? `<option value="">Choose model…</option>` : ""}${models.map((c) => `<option value="${esc(c.provider + "/" + c.id)}" ${c.provider === m.provider && c.id === m.model ? "selected" : ""}>${esc(c.label || c.id)} · ${esc(c.provider)}</option>`).join("")}</select></label></div><div><label>Thinking<select data-field="thinking">${[...(levels.length ? levels : [""]), ...(m.thinking && !levels.includes(m.thinking) ? [m.thinking] : [])].map((t) => `<option value="${esc(t)}" ${t === m.thinking ? "selected" : ""}>${esc(t || (levels.length ? "Provider default" : "In model ID / default"))}</option>`).join("")}</select></label></div><div><label>Plan / login<select data-field="login" required><option value="">Choose login…</option>${logins.map((l) => `<option value="${esc(l.account)}" ${l.account === m.login ? "selected" : ""}>${esc(l.label || l.account)}</option>`).join("")}</select></label></div></div><details><summary class="meta">Quota and plan options</summary><div class="fields"><label>Weekly quota pool<select data-field="longWindow"><option value="">Choose pool…</option>${ws.map((w) => `<option ${w.window_id === m.longWindow ? "selected" : ""}>${esc(w.window_id)}</option>`).join("")}</select></label><label>Short-window gates (comma separated)<input data-field="gateWindows" value="${esc((m.gateWindows || []).join(", "))}" placeholder="5h"></label></div><label>Plan ends (optional, UTC)<input type="datetime-local" data-field="eligibleUntil" value="${esc(m.eligibleUntil?.slice(0, 16) || "")}"></label><label class="check"><input type="checkbox" data-field="enabled" ${m.enabled ? "checked" : ""}>Enabled</label></details><div style="display:flex;gap:.4rem;margin-top:.7rem"><button type="button" class="btn sm" data-up ${i === 0 ? "disabled" : ""}>Move up</button><button type="button" class="btn sm" data-remove>Remove</button></div></fieldset>`;
          })
          .join("") ||
        '<p class="banner note">Empty group. Add a model before using its token.</p>';
      dialog.querySelectorAll("[data-row]").forEach((row) => {
        const i = +row.dataset.row,
          m = g.members[i];
        row
          .querySelector("[data-field=login]")
          .setCustomValidity(
            m.login.trim() ? "" : "Choose a plan/login for this model.",
          );
        row.querySelectorAll("[data-field]").forEach(
          (el) =>
            (el.onchange = () => {
              const field = el.dataset.field;
              if (field === "model") {
                const c = models.find(
                  (c) => c.provider + "/" + c.id === el.value,
                );
                if (c) {
                  m.provider = c.provider;
                  m.model = c.id;
                  m.thinking = c.default_thinking || "";
                  m.login = "";
                  m.longWindow = "";
                  m.gateWindows = [];
                }
                rows();
              } else if (field === "login") {
                m.login = el.value;
                const ws = quotaWindows(m);
                m.longWindow =
                  ws.find((w) => w.window_id.endsWith("7d"))?.window_id || "";
                m.gateWindows = ws
                  .filter(
                    (w) =>
                      w.window_id.endsWith("5h") &&
                      w.window_id.split("/").slice(0, -1).join("/") ===
                        m.longWindow.split("/").slice(0, -1).join("/"),
                  )
                  .map((w) => w.window_id);
                rows();
              } else if (field === "gateWindows")
                m[field] = el.value
                  .split(",")
                  .map((s) => s.trim())
                  .filter(Boolean);
              else if (field === "enabled") m[field] = el.checked;
              else if (field === "eligibleUntil")
                m[field] = el.value
                  ? new Date(el.value + "Z").toISOString()
                  : null;
              else m[field] = el.value;
            }),
        );
        row.querySelector("[data-remove]").onclick = () => {
          g.members.splice(i, 1);
          rows();
        };
        row.querySelector("[data-up]").onclick = () => {
          [g.members[i - 1], g.members[i]] = [g.members[i], g.members[i - 1]];
          rows();
        };
      });
    }
    rows();
    dialog.querySelector("[data-add]").onclick = () => {
      g.members.push({
        provider: "",
        model: "",
        thinking: "",
        login: "",
        longWindow: "",
        gateWindows: [],
        eligibleUntil: null,
        enabled: true,
      });
      rows();
    };
    dialog.querySelector("[data-cancel]").onclick = () => dialog.close();
    if (g.id)
      dialog.querySelector("[data-delete]").onclick = async () => {
        if (
          !confirm(
            "Delete this group? Workflows using its token must be changed first.",
          )
        )
          return;
        try {
          await write("/api/model-groups/" + g.id, "DELETE");
          dialog.close();
          await load();
        } catch (e) {
          showError(e);
        }
      };
    form.onsubmit = async (e) => {
      e.preventDefault();
      if (g.members.some((m) => !m.login.trim())) {
        showError(new Error("Choose a plan/login for each model."));
        return;
      }
      const save = form.querySelector('[type="submit"]');
      save.disabled = true;
      try {
        const payload = {
          name: form.elements.name.value,
          revision: g.revision,
          members: g.members.map(({ group_id, ...m }) => m),
        };
        await write(
          "/api/model-groups" + (g.id ? "/" + g.id : ""),
          g.id ? "PUT" : "POST",
          payload,
        );
        dialog.close();
        await load();
      } catch (e) {
        showError(e);
      } finally {
        save.disabled = false;
      }
    };
    dialog.showModal();
  }
  load().catch((e) => {
    root.textContent = "Model groups: " + e.message;
  });
})();
