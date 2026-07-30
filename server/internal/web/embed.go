package web

import "embed"

// dist holds the Vite production build (or a committed placeholder).
//
// Build integration:
//
//	make web   # runs npm ci && npm run build in ../../web, copies into dist/
//
// When dist/ only contains the placeholder index.html, go build still works so
// backend-only contributors are not blocked. The UI then shows a "run make web"
// message instead of the real shell.
//
//go:embed all:dist
var distFS embed.FS
