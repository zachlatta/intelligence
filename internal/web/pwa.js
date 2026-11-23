// Register the root-scoped service worker for PWA installability and Web Push notifications.
(() => {
  if (!("serviceWorker" in navigator)) return;
  navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch(() => {});
})();
