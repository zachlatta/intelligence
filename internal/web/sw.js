// Intelligence service worker: enables PWA installability and delivers Web Push notifications.
// Never caches authenticated HTML or API responses.

self.addEventListener("install", () => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("fetch", (event) => {
  if (event.request.mode !== "navigate") return;
  event.respondWith(
    fetch(event.request).catch(() =>
      new Response("Offline — Intelligence could not reach the server.", {
        status: 503,
        headers: { "Content-Type": "text/plain; charset=utf-8" },
      })
    )
  );
});

self.addEventListener("push", (event) => {
  let payload = { title: "Intelligence", body: "", url: "/" };
  if (event.data) {
    try {
      const parsed = event.data.json();
      if (parsed && typeof parsed === "object") {
        payload = {
          title: typeof parsed.title === "string" && parsed.title ? parsed.title : "Intelligence",
          body: typeof parsed.body === "string" ? parsed.body : "",
          url: typeof parsed.url === "string" && parsed.url ? parsed.url : "/",
          tag: typeof parsed.tag === "string" ? parsed.tag : undefined,
        };
      }
    } catch {
      const text = event.data.text();
      if (text) payload.body = text;
    }
  }
  event.waitUntil(
    self.registration.showNotification(payload.title, {
      body: payload.body,
      icon: "/icon-192.png",
      badge: "/notification-badge.png",
      tag: payload.tag,
      data: { url: payload.url },
    })
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const raw = event.notification.data && typeof event.notification.data.url === "string" ? event.notification.data.url : "/";
  const path = raw.startsWith("/") && !raw.startsWith("//") ? raw : "/";
  const target = new URL(path, self.location.origin).href;

  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
      for (const client of list) {
        if (client.url.startsWith(self.location.origin) && "focus" in client) {
          return client.focus().then((focused) => {
            if (focused && "navigate" in focused && focused.url !== target) {
              return focused.navigate(target);
            }
            return focused;
          });
        }
      }
      if (self.clients.openWindow) {
        return self.clients.openWindow(target);
      }
    })
  );
});
