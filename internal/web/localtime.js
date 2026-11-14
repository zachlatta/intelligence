// localtime.js: every page shows its times in the reader's own time zone and locale. The server
// writes each time as an instant (<time datetime="…" data-format="…">, or data-datetime on an SVG
// <text>) with UTC fallback text; this rewrites the text with the browser's Intl.DateTimeFormat and
// titles each <time> with the full local instant, also for content added to the page later.
(() => {
  const clock = { hour: "numeric", minute: "2-digit" };
  const FORMATS = {
    time: clock,
    day: { month: "short", day: "numeric" },
    short: { month: "short", day: "numeric", ...clock },
    seconds: { month: "short", day: "numeric", ...clock, second: "2-digit", timeZoneName: "short" },
    weekday: { weekday: "short", month: "short", day: "numeric", ...clock, timeZoneName: "short" },
    minute: { year: "numeric", month: "short", day: "numeric", ...clock, timeZoneName: "short" },
    full: { year: "numeric", month: "short", day: "numeric", ...clock, second: "2-digit", timeZoneName: "short" },
  };
  const made = {};
  const format = (name, d) => (made[name] ||= new Intl.DateTimeFormat(undefined, FORMATS[name] || FORMATS.full)).format(d);
  const SELECTOR = "time[datetime], [data-datetime][data-format]";

  function localize(el) {
    const iso = el.getAttribute("datetime") || el.getAttribute("data-datetime");
    if (!iso || !iso.includes("T") || el.getAttribute("data-localized") === iso) return; // a date alone has no zone
    const d = new Date(iso);
    if (isNaN(d)) return;
    const name = el.getAttribute("data-format");
    if (name) el.textContent = format(name, d);
    if (el.tagName === "TIME") el.title = format("full", d);
    el.setAttribute("data-localized", iso);
  }

  function scan(root) {
    if (!root) return;
    if (root.matches && root.matches(SELECTOR)) localize(root);
    if (root.querySelectorAll) root.querySelectorAll(SELECTOR).forEach(localize);
  }

  globalThis.localTime = { format, scan };
  scan(document);
  if (typeof MutationObserver === "function") {
    new MutationObserver((records) => {
      for (const r of records) for (const n of r.addedNodes || []) if (n.nodeType === 1) scan(n);
    }).observe(document.documentElement, { childList: true, subtree: true });
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", () => scan(document));
})();
