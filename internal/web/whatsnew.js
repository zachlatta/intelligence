// What's New: every page loads this with its stylesheet. It reads the entries the account has not
// seen through the read-only SQL API (whats_new_unseen). When there are any, the top bar's What's
// New tab shows their count (and says it in its accessible name) and, on other pages, a note under
// the top bar names the newest. Seeing them is a write: POST /api/whats-new/seen with their ids,
// sent when the What's New page shows them (it marks each with data-unseen) or when the note is
// dismissed.
(() => {
  const seen = (ids) => fetch("/api/whats-new/seen", { method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ids }) }).then((r) => { if (!r.ok) throw new Error("mark seen: " + r.status); });

  async function unseen() {
    const r = await fetch("/api/sql", { method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query: "SELECT id, title FROM whats_new_unseen ORDER BY released_on DESC, position" }) });
    if (!r.ok) return []; // signed out (the sign-in page) or the database is away: show nothing
    return (await r.json()).rows.map(([id, title]) => ({ id, title }));
  }

  // the tab shows the count, and says it in its accessible name: "What's New, 3 unseen"
  function setCount(n) {
    const link = document.getElementById("nav-whats-new");
    const count = link?.querySelector(".unseen-count");
    if (!count) return;
    count.hidden = n === 0;
    count.textContent = n === 0 ? "" : String(n);
    if (n === 0) link.removeAttribute("aria-label");
    else link.setAttribute("aria-label", "What's New, " + n + " unseen");
  }

  function note(entries) {
    const top = document.querySelector("header.topbar");
    if (!top || document.querySelector(".whatsnew-note")) return;
    const wrap = document.createElement("div");
    wrap.className = "container";
    const box = document.createElement("div");
    box.className = "whatsnew-note";
    box.setAttribute("role", "status");
    const what = document.createElement("span");
    what.className = "what";
    const b = document.createElement("b");
    b.textContent = "New: " + entries[0].title;
    what.append(b);
    if (entries.length > 1) what.append(" and " + (entries.length - 1) + " more");
    const open = document.createElement("a");
    open.className = "btn sm";
    open.href = "/whats-new";
    open.textContent = "See what's new";
    const dismiss = document.createElement("button");
    dismiss.type = "button";
    dismiss.className = "btn sm ghost";
    dismiss.textContent = "Dismiss";
    dismiss.addEventListener("click", async () => {
      dismiss.disabled = true;
      try {
        await seen(entries.map((e) => e.id));
        wrap.remove();
        setCount(0);
      } catch (err) {
        dismiss.disabled = false;
        dismiss.textContent = "Dismiss (failed, try again)";
      }
    });
    box.append(what, open, dismiss);
    wrap.append(box);
    top.after(wrap);
  }

  async function run() {
    // the What's New page: what it shows unseen is seen now; its "New" marks stay until the next load
    const shown = [...document.querySelectorAll("[data-unseen]")].map((el) => el.dataset.unseen);
    if (shown.length) {
      try { await seen(shown); } catch (err) { console.warn(err); }
    }
    const left = (await unseen()).filter((e) => !shown.includes(e.id));
    setCount(left.length);
    if (left.length && location.pathname !== "/whats-new") note(left);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", run);
  else run();
})();
