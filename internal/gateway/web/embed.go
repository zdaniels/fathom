// Package web ships the static HTML/CSS/JS/icons for the gateway's browser
// UI. The old single-file pattern was fine when the UI was 377 LOC of
// inline CSS+JS at one URL; the mobile rewrite is a real PWA with a
// manifest, a service worker, and per-file caching needs (cache-bust JS
// but not the manifest, etc.), so we ship multiple files.
//
// Still zero toolchain — no bundler, no transpile. Source files are the
// shipped artifacts.
package web

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed features.js board.html board.js admin.html admin.js team.css index.html chat.css chat.js settings.html settings.css settings.js manifest.webmanifest sw.js icons
var assets embed.FS

// menubar-icon.png is the same iceberg silhouette the Mac menubar app
// ships, intentionally identical so the mobile spinner reads as "the
// same brand mark you see in the menubar." See icons/menubar-icon.png.

// Assets returns the embedded filesystem rooted at the web/ directory.
// The gateway uses this with http.FileServerFS to serve every shipped
// file at its natural path (/, /chat.css, /icons/icon-192.png, etc.).
func Assets() fs.FS {
	sub, err := fs.Sub(assets, ".")
	if err != nil {
		panic(err) // unreachable — embed succeeded at compile time
	}
	return sub
}

// IndexHTML returns the raw index.html bytes for direct serving (still
// useful as the root document and for the legacy /chat / /chat.html
// aliases the desktop client may have bookmarked).
func IndexHTML() []byte {
	data, _ := assets.ReadFile("index.html")
	return data
}

// AssetBytes returns the bytes for a named asset (relative to the web/
// root, e.g. "chat.css") and an ok flag. Used by the gateway to serve
// the small fixed set of static files without pulling in the full
// http.FileServerFS path-matching machinery (which would also start
// matching arbitrary user-supplied paths).
func AssetBytes(name string) ([]byte, bool) {
	clean := strings.TrimPrefix(name, "/")
	if clean == "" {
		clean = "index.html"
	}
	data, err := assets.ReadFile(clean)
	if err != nil {
		return nil, false
	}
	return data, true
}

// ContentType returns the correct MIME type for one of our embedded
// assets. http.DetectContentType guesses wrong for some of them
// (manifest.webmanifest comes back as text/plain; sw.js MUST be served
// as JS or browsers reject it as a service worker), so we resolve by
// extension explicitly.
func ContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".webmanifest"):
		return "application/manifest+json"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	}
	return "application/octet-stream"
}
