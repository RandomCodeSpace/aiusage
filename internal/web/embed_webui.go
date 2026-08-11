//go:build webui

package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The embedded single-page UI.
//
// dist is the Vite build (webui/, built with `npm run build`, output at
// internal/web/dist). It is gitignored and never committed: the release pipeline
// builds it immediately before `go build -tags webui`, so a tagged binary always
// carries assets that match its source. A tagged build with no dist directory
// does not compile, which is the intended failure - a UI build that silently
// shipped an empty page would be worse.
//
// all: is load-bearing. Without it, embed skips files whose names begin with a
// dot or an underscore, and asset pipelines emit both.
//
//go:embed all:dist
var distFS embed.FS

// hasEmbeddedUI is the webui build tag, as a value. buildinfo.HasWebUI is the
// same tag folded into the build identity; internal/cmd has a test that fails
// the build if the two ever disagree.
const hasEmbeddedUI = true

// uiCSP is the Content-Security-Policy served with the page and its assets.
//
// It is this strict because the built app needs nothing more: Vite emits one
// module script and one stylesheet, both same-origin files with fingerprinted
// names, and there is no inline script, no inline style and no third-party
// origin anywhere in it. The app's own traffic - fetches to /api and the live
// WebSocket - is same-origin, and a same-origin wss:// connection satisfies
// connect-src 'self' in every browser that implements CSP3.
//
// The three directives that are not about loading are the ones that matter on
// an unauthenticated local port: frame-ancestors 'none' keeps the dashboard out
// of a stranger's iframe (where it would be same-origin to nothing but still
// rendered), base-uri 'none' stops an injected <base> from re-pointing every
// relative asset URL, and form-action 'self' means a successful injection
// cannot post the page's contents somewhere else.
const uiCSP = "default-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// uiHandler serves the SPA: real files by path, index.html for everything else.
//
// The fallback is what makes client-side routes work. A deep link is a path this
// server has never heard of, and answering it with index.html lets the page
// route itself - the alternative is a 404 on every URL the user bookmarked.
// It is safe here precisely because /api/ is claimed by the mux first: an
// unknown API path returns JSON, never a page.
func uiHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Only reachable if the embed above changed shape, which is a compile
		// -time fact; serving the stub is the safe degradation.
		return stubUIHandler()
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "read-only server: use GET")
			return
		}
		// Set before anything writes: the page and every asset carry the same
		// policy, and a header added after the first write is not a header.
		w.Header().Set("Content-Security-Policy", uiCSP)
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || !exists(sub, name) {
			serveIndex(w, r, sub)
			return
		}
		// Vite fingerprints asset filenames, so a hit is immutable by
		// construction: the content of a given name never changes.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		files.ServeHTTP(w, r)
	})
}

// exists reports whether name is a regular file in the embedded tree.
func exists(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// serveIndex writes the SPA shell. It is never cached: index.html names the
// fingerprinted assets, so a stale copy pins the page to a previous build.
func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	page, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		writeError(w, http.StatusNotFound, "no page in this build")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(page)
}
