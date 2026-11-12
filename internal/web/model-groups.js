"use strict";

// The pools a login can be assigned to: each pool's longest observed window. IDs do not encode
// billing policy; the server checks a pool's shorter windows live.
function modelGroupPools(windows, provider, login) {
  const pool = (id) => id.slice(0, id.lastIndexOf("/") + 1);
  const rows = windows.filter(
    (w) =>
      w.provider === provider && w.account === login && w.window_seconds > 0,
  );
  return rows
    .filter(
      (w) =>
        !rows.some(
          (other) =>
            pool(other.window_id) === pool(w.window_id) &&
            other.window_seconds > w.window_seconds,
        ),
    )
    .map((w) => w.window_id);
}

// Shared editor behavior. The owning pages supply the controls and templates.
(() => {
  const root = document.querySelector("#model-groups");
  if (!root) return;
  const dialog = document.querySelector("#model-group-editor");
  const manager = document.querySelector("#model-group-manager");
  const token = (g) => "{{" + g.token + "}}";
  const clone = (id) =>
    document.getElementById(id).content.firstElementChild.cloneNode(true);
  const at = (el, key) => el.querySelector("[data-" + key + "]");
  const text = (el, key, value) => {
    at(el, key).textContent = value;
  };
  const options = (el, values, selected) => {
    el.replaceChildren(
      ...values.map(([value, label]) => {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = label;
        option.selected = value === selected;
        return option;
      }),
    );
  };
  let groups = [],
    members = [],
    catalog = [],
    windows = [],
    candidates = [],
    host = "",
    tokenSignature,
    loading;
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
  function load() {
    if (!loading)
      loading = loadData().finally(() => {
        loading = null;
      });
    return loading;
  }
  async function loadData() {
    root.querySelectorAll("[data-new], [data-edit]").forEach((button) => {
      button.disabled = true;
    });
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
        eligibleUntil: m.eligible_until,
        enabled: m.enabled,
      })),
    );
    const hosts = [...new Set(catalog.map((c) => c.host))];
    if (!hosts.includes(host)) host = hosts[0] || "";
    render();
    const signature = JSON.stringify(
      groups.map((g) => [g.id, g.name, g.token]),
    );
    if (signature !== tokenSignature) {
      tokenSignature = signature;
      document.dispatchEvent(new Event("modelgroupschange"));
    }
    root.querySelectorAll("[data-new], [data-edit]").forEach((button) => {
      button.disabled = false;
    });
  }

  window.modelGroups = {
    groups: () => groups,
    open: openGroups,
  };
  document
    .querySelectorAll("[data-model-groups-open]")
    .forEach((button) => (button.onclick = openGroups));
  document
    .querySelectorAll("[data-model-groups-close]")
    .forEach((button) => (button.onclick = () => manager.close()));
  root.querySelector("[data-new]").onclick = () => edit();
  root.querySelector("[data-host]").onchange = (e) => {
    host = e.target.value;
    render();
  };
  async function openGroups() {
    try {
      await load();
      if (manager) manager.showModal();
      else root.scrollIntoView();
    } catch {
      root.querySelector("[data-group-cards]").textContent =
        "Could not load model groups. Please reload.";
    }
  }
  function loginLabel(provider, login) {
    return (
      windows.find((w) => w.provider === provider && w.account === login)
        ?.label || login
    );
  }
  function render() {
    options(
      at(root, "host"),
      [...new Set(catalog.map((c) => c.host))].map((h) => [h, h]),
      host,
    );
    const cards = at(root, "group-cards");
    cards.replaceChildren();
    if (!groups.length) {
      cards.textContent =
        "Create Frontier, Fast, or any other group. Then insert its token into a workflow step.";
      return;
    }
    for (const g of groups) {
      const card = clone("model-group-card"),
        cs = candidates.filter((c) => c.group_id === g.id && c.host === host),
        best = cs.find((c) => c.eligible),
        empty = !g.members.length;
      text(card, "name", g.name);
      text(card, "token", token(g));
      const status = at(card, "status");
      status.textContent = empty
        ? "Empty group"
        : best
          ? "Suggested now"
          : "No eligible model";
      status.classList.add(empty ? "orange" : best ? "blue" : "orange");
      text(
        card,
        "choice",
        empty
          ? "Add a model before using this token."
          : best
            ? best.provider +
              "/" +
              best.model +
              (best.thinking ? " · " + best.thinking : "")
            : "Check logins, meters and quota.",
      );
      text(
        card,
        "meter",
        best
          ? " · " +
              loginLabel(best.provider, best.login) +
              " · meter " +
              Math.max(
                0,
                Math.floor(
                  (Date.now() - Date.parse(best.last_observed)) / 60000,
                ),
              ) +
              " min old"
          : "",
      );
      text(card, "reason", best ? "More quota likely to remain at reset." : "");
      text(
        card,
        "members",
        "Members and alternatives (" + g.members.length + ")",
      );
      const alternatives = at(card, "alternatives");
      if (!cs.length) alternatives.textContent = "No candidates on this host.";
      for (const c of cs) {
        const row = clone("model-group-candidate");
        text(
          row,
          "setting",
          c.provider +
            "/" +
            c.model +
            " · " +
            (c.thinking || "effort in model ID / default") +
            " · " +
            loginLabel(c.provider, c.login),
        );
        text(
          row,
          "status",
          c.eligible
            ? "Available"
            : c.reason || "Quota reserved for recent suggestions",
        );
        alternatives.append(row);
      }
      at(card, "edit").dataset.edit = g.id;
      at(card, "edit").onclick = () => edit(g);
      at(card, "copy").onclick = async (e) => {
        try {
          await navigator.clipboard.writeText(token(g));
          e.target.textContent = "Copied";
        } catch {
          e.target.textContent = "Select token to copy";
        }
      };
      cards.append(card);
    }
  }
  function edit(group) {
    const g = structuredClone(group || { name: "", revision: 0 });
    g.members = structuredClone(members.filter((m) => m.group_id === g.id));
    const form = dialog.querySelector("form"),
      error = at(dialog, "error");
    form.reset();
    error.hidden = true;
    error.textContent = "";
    text(dialog, "editor-title", g.id ? "Edit model group" : "New model group");
    form.elements.name.value = g.name;
    text(
      dialog,
      "used-by",
      g.id
        ? `Used by ${g.used_by || 0} workflows. Edits affect their future suggestions.`
        : "You can create more groups, each with its own token.",
    );
    at(dialog, "delete").hidden = !g.id;
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
      text(
        dialog,
        "token-preview",
        token(g.id ? g : { token: b + "_model" }) +
          (g.id ? "" : " (a suffix is added if already used)"),
      );
    };
    form.elements.name.oninput = preview;
    preview();
    at(dialog, "copy-token").onclick = async () => {
      try {
        await navigator.clipboard.writeText(
          g.token
            ? token(g)
            : at(dialog, "token-preview").textContent.split(" ")[0],
        );
      } catch (e) {
        showError(e);
      }
    };
    function rows() {
      const models = [
          ...new Map(catalog.map((c) => [c.provider + "/" + c.id, c])).values(),
        ],
        target = at(dialog, "rows");
      target.replaceChildren();
      if (!g.members.length) {
        const note = document.createElement("p");
        note.className = "banner note";
        note.textContent = "Empty group. Add a model before using its token.";
        target.append(note);
      }
      g.members.forEach((m, i) => {
        const row = clone("model-group-member");
        row.dataset.row = i;
        const field = (k) => row.querySelector('[data-field="' + k + '"]'),
          cat = models.find(
            (c) => c.provider === m.provider && c.id === m.model,
          ),
          levels = [
            ...new Set(
              catalog
                .filter((c) => c.provider === m.provider && c.id === m.model)
                .flatMap((c) => c.thinking || []),
            ),
          ];
        if (!m.thinking && levels.length)
          m.thinking = cat?.default_thinking || levels[0];
        const logins = [
            ...new Map(
              windows
                .filter((w) => w.provider === m.provider)
                .map((w) => [w.account, w]),
            ).values(),
          ],
          pools = modelGroupPools(windows, m.provider, m.login);
        text(row, "number", "Member " + (i + 1));
        text(
          row,
          "quota-note",
          !m.login
            ? "Choose a plan/login to see its observed quota pools."
            : pools.length
              ? "Pools come from observed usage; their shorter windows are checked too."
              : "No quota windows observed. Keep this entry disabled until usage is available.",
        );
        options(
          field("model"),
          [
            ["", "Choose model…"],
            ...models.map((c) => [
              c.provider + "/" + c.id,
              (c.label || c.id) + " · " + c.provider,
            ]),
          ],
          m.provider + "/" + m.model,
        );
        options(
          field("thinking"),
          (levels.length ? levels : [""]).map((t) => [
            t,
            t || "In model ID / default",
          ]),
          m.thinking,
        );
        options(
          field("login"),
          [
            ["", "Choose login…"],
            ...logins.map((l) => [l.account, l.label || l.account]),
          ],
          m.login,
        );
        field("login").setCustomValidity(
          m.login.trim() ? "" : "Choose a plan/login for this model.",
        );
        options(
          field("longWindow"),
          [["", "Choose observed pool…"], ...pools.map((p) => [p, p])],
          m.longWindow,
        );
        field("enabled").checked = m.enabled;
        field("eligibleUntil").value = m.eligibleUntil
          ? new Date(m.eligibleUntil).toISOString().slice(0, 16)
          : "";
        row.querySelectorAll("[data-field]").forEach(
          (el) =>
            (el.onchange = () => {
              const key = el.dataset.field;
              if (key === "model") {
                const c = models.find(
                  (c) => c.provider + "/" + c.id === el.value,
                );
                if (c)
                  Object.assign(m, {
                    provider: c.provider,
                    model: c.id,
                    thinking: c.default_thinking || "",
                    login: "",
                    longWindow: "",
                  });
                rows();
              } else if (key === "login") {
                m.login = el.value;
                const available = modelGroupPools(windows, m.provider, m.login);
                m.longWindow = available.length === 1 ? available[0] : "";
                if (!available.length) m.enabled = false;
                rows();
              } else if (key === "longWindow") {
                m.longWindow = el.value;
                rows();
              } else if (key === "enabled") m.enabled = el.checked;
              else if (key === "eligibleUntil")
                m.eligibleUntil = el.value
                  ? new Date(el.value + "Z").toISOString()
                  : null;
              else m[key] = el.value;
            }),
        );
        at(row, "up").disabled = i === 0;
        at(row, "up").onclick = () => {
          [g.members[i - 1], g.members[i]] = [g.members[i], g.members[i - 1]];
          rows();
        };
        at(row, "remove").onclick = () => {
          g.members.splice(i, 1);
          rows();
        };
        target.append(row);
      });
    }
    rows();
    at(dialog, "add").onclick = () => {
      g.members.push({
        provider: "",
        model: "",
        thinking: "",
        login: "",
        longWindow: "",
        eligibleUntil: null,
        enabled: true,
      });
      rows();
    };
    at(dialog, "cancel").onclick = () => dialog.close();
    at(dialog, "delete").onclick = async () => {
      if (
        !confirm(
          "Delete this group? Workflows using its token must be changed first.",
        )
      )
        return;
      try {
        await write("/api/model-groups/" + g.id, "DELETE");
        await load();
        dialog.close();
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
        await write(
          "/api/model-groups" + (g.id ? "/" + g.id : ""),
          g.id ? "PUT" : "POST",
          {
            name: form.elements.name.value,
            revision: g.revision,
            members: g.members.map(({ group_id, ...m }) => m),
          },
        );
        await load();
        dialog.close();
      } catch (e) {
        showError(e);
      } finally {
        save.disabled = false;
      }
    };
    dialog.showModal();
  }
  load().catch(() => {
    at(root, "group-cards").textContent =
      "Could not load model groups. Please reload.";
  });
})();
