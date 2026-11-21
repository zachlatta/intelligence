// Broken logins: every page loads this with its stylesheet. It reads login_alerts through the
// read-only SQL API: each login a host's agents could not use (an expired or revoked Claude or Codex
// sign-in), and each whose sign-in ends within 5 days. Each shows as a note under the top bar that
// says which login on which host, what it broke and what Intelligence did about it, and how to fix
// it: the steps, the command to copy (login_alerts.command: for a login cswap manages, the whole
// safe sequence, which signs in inside a throwaway CLAUDE_CONFIG_DIR and imports into cswap, never
// over the live login; otherwise only the first step, labelled so), and Mark fixed
// (POST /api/login-incidents/{id}/resolve) for when the host's meter cannot tell that it works again. On the Plans page it also fills the
// history of interrupted agents (#login-history) from agent_interruptions.
(() => {
  const sql = async (query) => {
    const r = await fetch("/api/sql", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query }) });
    if (!r.ok) return null; // signed out, or the database is away: show nothing
    return (await r.json()).rows;
  };
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  const when = (iso) => {
    const d = new Date(iso);
    return isNaN(d) ? "" : d.toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
  };
  const plural = (n, one, many) => n + " " + (n === 1 ? one : many);
  const providerName = (p) => ({ claude: "Claude", codex: "Codex", muse: "Muse", antigravity: "Antigravity" })[p] || p;

  // What the agents went through, in a sentence.
  function story(a) {
    if (!a.interrupted) return a.source === "meter" ? "The host's meter says it has no working sign-in; no agent has hit it yet." : "";
    const parts = [];
    if (a.recovered) parts.push(a.recovered + " resumed on a working login");
    if (a.waiting) parts.push(a.waiting + " waiting for a working login");
    if (a.stuck) parts.push(a.stuck + " left for you to resume");
    return plural(a.interrupted, "agent turn", "agent turns") + " failed on it" + (parts.length ? ": " + parts.join(", ") + "." : ".");
  }

  function note(a) {
    const broken = a.kind === "broken";
    const box = el("div", "login-alert " + (broken ? "broken" : "expiring"));
    box.setAttribute("role", broken ? "alert" : "status");
    const what = el("div", "what");
    const head = el("b", "", broken
      ? providerName(a.provider) + " login " + a.label + " on " + a.host + " stopped working"
      : providerName(a.provider) + " login " + a.label + " on " + a.host + " ends " + when(a.expires_at));
    what.append(head);
    if (broken) what.append(" since " + when(a.since) + ": " + (a.reason === "login_expired" ? "its sign-in expired." : a.reason === "logged_out" ? "the host is signed out of it." : "the provider refused it."));
    else what.append(". Renew it before then, or every agent using it stops.");
    const s = broken ? story(a) : "";
    if (s) what.append(el("span", "story", " " + s));
    const how = el("details", "fix");
    how.append(el("summary", "", "How to fix"), el("p", "", a.fix));
    if (a.command) {
      const pre = el("pre", "cmd");
      pre.append(el("code", "", a.command));
      how.append(pre);
    }
    if (broken && a.message) how.append(el("p", "said", (a.source === "meter" ? "The host's meter said: " : "The provider said: ") + a.message));
    what.append(how);
    box.append(what);
    const cmd = a.command;
    if (cmd) {
      const label = a.first_step ? "Copy first step" : "Copy command";
      const copy = el("button", "btn sm", label);
      if (a.first_step) copy.title = "Only the first step: see How to fix";
      copy.type = "button";
      copy.addEventListener("click", async () => {
        try { await navigator.clipboard.writeText(cmd); copy.textContent = "Copied"; } catch (err) { copy.textContent = cmd; }
      });
      box.append(copy);
    }
    if (broken && a.incident_id) {
      const fixed = el("button", "btn sm ghost", "Mark fixed");
      fixed.type = "button";
      fixed.title = "You signed it in again: resume what waited for it";
      fixed.addEventListener("click", async () => {
        fixed.disabled = true;
        const r = await fetch("/api/login-incidents/" + encodeURIComponent(a.incident_id) + "/resolve", { method: "POST",
          headers: { "Content-Type": "application/json" }, body: JSON.stringify({ note: "marked fixed from " + location.pathname }) });
        if (r.ok) { box.remove(); return; }
        fixed.disabled = false;
        fixed.textContent = "Mark fixed (failed, try again)";
      });
      box.append(fixed);
    }
    return box;
  }

  async function alerts() {
    const rows = await sql(`SELECT kind, incident_id, host, provider, login, label, reason, message, fix, command, first_step, source,
      since, expires_at, interrupted, waiting, recovered, stuck FROM login_alerts ORDER BY kind, since`);
    if (!rows || !rows.length) return;
    const top = document.querySelector("header.topbar");
    if (!top || document.querySelector(".login-alerts")) return;
    const wrap = el("div", "container login-alerts");
    const keys = ["kind", "incident_id", "host", "provider", "login", "label", "reason", "message", "fix", "command", "first_step",
      "source", "since", "expires_at", "interrupted", "waiting", "recovered", "stuck"];
    let broken = 0;
    for (const row of rows) {
      const a = Object.fromEntries(keys.map((k, i) => [k, row[i]]));
      if (a.kind === "broken") broken++;
      wrap.append(note(a));
    }
    top.after(wrap);
    if (broken && !document.title.startsWith("⚠")) document.title = "⚠ Login broken · " + document.title;
  }

  const statusWords = { waiting: "Waiting for a working login", resumed: "Resumed", parent_notified: "Handed to its parent",
    superseded: "Carried on by itself", gone: "Agent gone", gave_up: "Gave up: resume it by hand", expired: "Gave up after a day" };

  async function history() {
    const table = document.getElementById("login-history");
    if (!table) return;
    const rows = await sql(`SELECT x.at, x.host, x.provider, coalesce(nullif(i.login, ''), '?'), coalesce(nullif(x.title, ''), x.agent_id),
      x.model, x.status, x.outcome, x.run_id::text, x.agent_id, i.resolved_at
      FROM agent_interruptions x LEFT JOIN login_incidents i ON i.id = x.incident_id ORDER BY x.at DESC LIMIT 50`);
    if (!rows || !rows.length) return;
    const body = table.querySelector("tbody");
    for (const [at, host, provider, login, who, model, status, outcome, run, agent, fixed] of rows) {
      const tr = el("tr");
      const agentCell = el("td");
      const link = el("a", "", who);
      link.href = run ? "/workflow-runs/" + encodeURIComponent(run) : "/hosts/" + encodeURIComponent(host) + "/agents/" + encodeURIComponent(agent);
      agentCell.append(link);
      if (model) agentCell.append(el("span", "muted", " " + provider + "/" + model));
      const st = el("td");
      st.append(el("span", "badge " + (status === "waiting" ? "orange" : status === "gave_up" || status === "expired" ? "red" : ""), statusWords[status] || status));
      if (outcome) st.title = outcome;
      tr.append(el("td", "", when(at)), el("td", "", host), el("td", "mono", login + (fixed ? "" : " (broken)")), agentCell, st);
      body.append(tr);
    }
    table.closest("section").hidden = false;
  }

  async function run() {
    await Promise.all([alerts().catch(console.warn), history().catch(console.warn)]);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", run);
  else run();
})();
