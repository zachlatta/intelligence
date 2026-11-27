// Every page reports how long it took to load (its navigation timing) to POST /api/page-loads,
// once, after its load event. It sends the page's path, never its query; the server stores the
// route pattern that serves it (page_loads).
(() => {
  // signed out (the sign-in page) there is no one to report as
  if (!document.querySelector("nav.topnav")) return;
  const ms = (v) => Math.max(0, Math.round((v || 0) * 100) / 100);
  function report() {
    const nav = performance.getEntriesByType && performance.getEntriesByType("navigation")[0];
    if (!nav || !nav.loadEventEnd) return;
    fetch("/api/page-loads", {
      method: "POST",
      keepalive: true,
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        path: location.pathname,
        navigationType: nav.type || "navigate",
        ttfbMs: ms(nav.responseStart - nav.startTime),
        domContentLoadedMs: ms(nav.domContentLoadedEventEnd - nav.startTime),
        loadMs: ms(nav.loadEventEnd - nav.startTime),
        durationMs: ms(nav.duration),
      }),
    }).catch(() => {});
  }
  // loadEventEnd is set once the load handlers have run, so report on the next turn
  const later = () => setTimeout(report, 0);
  if (document.readyState === "complete") later();
  else addEventListener("load", later, { once: true });
})();
