// Silent hosts: every page loads this with its stylesheet. It reads host_silence_current through the
// read-only SQL API: each host with active workflow runs that has not reported for 10 minutes
// (internal/hostwatch). Each shows as a red note under the top bar: which host, for how long, how
// many runs wait on it, the last OOM kills its probe recorded, and whether the phone
// notification went out. The note goes when the host reports again.
(() => {
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
  const span = (ms) => {
    const m = Math.max(0, Math.floor(ms / 60000));
    return m < 60 ? m + " min" : Math.floor(m / 60) + " h" + (m % 60 ? " " + (m % 60) + " min" : "");
  };

  function note(a) {
    const box = el("div", "login-alert broken host-silent");
    box.setAttribute("role", "alert");
    const what = el("div", "what");
    const link = el("a", "", a.host);
    link.href = "/hosts/" + encodeURIComponent(a.host);
    const head = el("b");
    head.append(link, " stopped reporting");
    what.append(head);
    what.append(" " + span(Date.now() - Date.parse(a.last_seen)) + " ago (last heard " + when(a.last_seen) + "). ");
    what.append(a.active_runs === 1 ? "1 run is waiting on it. " : a.active_runs + " runs are waiting on it. ");
    if (a.oom_kills > 0) {
      what.append("Last recorded: " + a.oom_kills + (a.oom_kills === 1 ? " OOM kill" : " OOM kills") +
        (a.last_oom_at ? " up to " + when(a.last_oom_at) : "") + (a.oom_comms ? " (" + a.oom_comms + ")" : "") + ". ");
    } else {
      what.append("No OOM kills recorded. ");
    }
    if (a.notify) {
      if (a.alert_sent_at) what.append(el("span", "story", "You were sent a phone notification " + when(a.alert_sent_at) + "."));
      else if (a.alert_error) what.append(el("span", "story", "The phone notification failed: " + a.alert_error));
    }
    box.append(what);
    return box;
  }

  async function run() {
    const r = await fetch("/api/sql", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query: `SELECT host, last_seen, opened_at, active_runs, oom_kills, last_oom_at, oom_comms, notify,
        alert_sent_at, alert_error FROM host_silence_current ORDER BY last_seen` }) });
    if (!r.ok) return; // signed out, or the database is away: show nothing
    const rows = (await r.json()).rows;
    if (!rows || !rows.length) return;
    const top = document.querySelector("header.topbar");
    if (!top || document.querySelector(".host-alerts")) return;
    const keys = ["host", "last_seen", "opened_at", "active_runs", "oom_kills", "last_oom_at", "oom_comms", "notify", "alert_sent_at", "alert_error"];
    const wrap = el("div", "container host-alerts");
    for (const row of rows) wrap.append(note(Object.fromEntries(keys.map((k, i) => [k, row[i]]))));
    top.after(wrap);
    if (!document.title.startsWith("⚠")) document.title = "⚠ Host silent · " + document.title;
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", () => run().catch(console.warn));
  else run().catch(console.warn);
})();
