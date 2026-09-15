// Fathom service worker.
//
// Goals:
//   - Make the app shell (HTML/CSS/JS/icons/manifest) available offline so
//     opening the PWA without network shows our UI instead of the browser's
//     "no internet" page.
//   - Never cache /api/* responses — those must always hit the gateway live.
//   - Skip waiting on activation so a new build rolls out on next launch
//     instead of needing two reloads.
//
// Cache name embeds a version. Bump on every shipped change so old caches
// get cleaned up in `activate`. The CACHE_VERSION number is the truth —
// if you forget to bump it, users may see stale UI; if in doubt, bump it.

const CACHE_VERSION = "v28"; // Fathom app shell
const CACHE_NAME = "fathom-shell-" + CACHE_VERSION;
// Relative paths so this works whether the gateway is served from /
// (LAN/Tailscale/Tunnel) or from /c/{relay_id}/ (relay.fantazm.ai).
const SHELL_ASSETS = [
  "./",
  "chat.css",
  "chat.js",
  "settings.html",
  "settings.css",
  "settings.js",
  "board.html",
  "board.js",
  "admin.html",
  "admin.js",
  "team.css",
  "features.js",
  "manifest.webmanifest",
  "icons/icon-192.png",
  "icons/icon-512.png",
  "icons/apple-touch-icon.png",
  "icons/menubar-icon.png", // spinner glyph — must be in offline shell
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => cache.addAll(SHELL_ASSETS)).then(() => self.skipWaiting())
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE_NAME).map((k) => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);

  // Never cache API requests — they must always hit the live gateway.
  if (url.pathname.startsWith("/api/")) return;

  // Cache-first for shell assets; fall back to network and stash on success.
  event.respondWith(
    caches.match(req).then((cached) => {
      if (cached) return cached;
      return fetch(req).then((res) => {
        if (res.ok && url.origin === self.location.origin) {
          const copy = res.clone();
          caches.open(CACHE_NAME).then((c) => c.put(req, copy));
        }
        return res;
      }).catch(() => {
        // Offline + not cached + not an API call — fall back to the shell
        // so SPA navigations still land somewhere usable.
        if (req.mode === "navigate") return caches.match("/");
        return new Response("Offline", { status: 503, statusText: "Offline" });
      });
    })
  );
});
