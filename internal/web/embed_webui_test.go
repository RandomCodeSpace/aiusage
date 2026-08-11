//go:build webui

package web

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
)

// TestEmbeddedBuildServesThePage: with the tag set, / is the SPA shell and any
// client route falls back to it, while /api keeps answering JSON.
func TestEmbeddedBuildServesThePage(t *testing.T) {
	if !HasEmbeddedUI() {
		t.Fatal("HasEmbeddedUI() is false under the webui tag")
	}
	srv, _ := newTestServer(t, defaultEvents())

	index := get(t, srv, "/")
	if index.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", index.Code)
	}
	if ct := index.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content-type = %q, want html", ct)
	}
	if index.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("index Cache-Control = %q; a cached shell pins the page to an old build",
			index.Header().Get("Cache-Control"))
	}

	// A client-side route is a path this server has never heard of. Answering it
	// with the shell is what makes a bookmarked deep link work.
	deep := get(t, srv, "/sessions/abc123")
	if deep.Code != http.StatusOK || deep.Body.String() != index.Body.String() {
		t.Errorf("GET /sessions/abc123 = %d, want the index shell", deep.Code)
	}

	// The fallback must not swallow the API: an unknown /api path is JSON.
	if rec := get(t, srv, "/api/nope"); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Errorf("GET /api/nope = %d %q, want a JSON 404",
			rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get(t, srv, "/api/meta"); rec.Code != http.StatusOK {
		t.Errorf("GET /api/meta = %d, want 200", rec.Code)
	}
}

// TestEmbeddedPageCarriesTheCSP: the page is served on an unauthenticated local
// port, and the policy is what keeps an injected string from loading or
// exfiltrating anything. It has to land on the shell AND on the assets - a
// policy that only covers index.html leaves every script response unpoliced.
func TestEmbeddedPageCarriesTheCSP(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	want := map[string]string{
		"default-src":     "'self'",
		"img-src":         "'self' data:",
		"connect-src":     "'self'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
		"form-action":     "'self'",
	}
	for _, target := range []string{"/", "/sessions/abc123", assetPath(t)} {
		rec := get(t, srv, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, rec.Code)
		}
		got := rec.Header().Get("Content-Security-Policy")
		if got == "" {
			t.Fatalf("GET %s carries no Content-Security-Policy", target)
		}
		have := map[string]bool{}
		for _, directive := range strings.Split(got, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
			if want[name] != strings.TrimSpace(value) {
				continue
			}
			have[name] = true
		}
		for name, value := range want {
			if !have[name] {
				t.Errorf("GET %s policy %q is missing %s %s", target, got, name, value)
			}
		}
	}
}

// TestBuiltPageNeedsNothingTheCSPForbids reads the embedded build itself. The
// policy above is only safe because the app has no inline script and no inline
// style; a build that grew one would fail to render under the policy this
// server sends, and the browser would be the first to know.
func TestBuiltPageNeedsNothingTheCSPForbids(t *testing.T) {
	page, err := fs.ReadFile(distFS, "dist/index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}
	html := string(page)

	// <script> is fine only when it is a src= reference; a body between the
	// tags is inline code and 'self' does not allow it.
	for _, chunk := range strings.Split(html, "<script")[1:] {
		open, rest, ok := strings.Cut(chunk, ">")
		if !ok {
			t.Fatalf("unterminated script tag in index.html: %q", chunk)
		}
		if !strings.Contains(open, "src=") {
			t.Errorf("index.html carries an inline script: <script%s>", open)
		}
		if body, _, _ := strings.Cut(rest, "</script>"); strings.TrimSpace(body) != "" {
			t.Errorf("index.html carries script content the policy forbids: %q", body)
		}
	}
	if strings.Contains(html, "<style") {
		t.Error("index.html carries an inline <style> block; the policy forbids it")
	}
	for _, attr := range []string{" onload=", " onclick=", " onerror=", " style=", "javascript:"} {
		if strings.Contains(strings.ToLower(html), attr) {
			t.Errorf("index.html carries %q, which the policy forbids", attr)
		}
	}
}

// assetPath returns one real fingerprinted asset path from the embedded build,
// so the CSP check covers a file served by the file server rather than only the
// hand-written index path.
func assetPath(t *testing.T) string {
	t.Helper()
	entries, err := fs.ReadDir(distFS, "dist/assets")
	if err != nil {
		t.Fatalf("read embedded assets: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			return "/assets/" + e.Name()
		}
	}
	t.Fatal("the embedded build carries no assets")
	return ""
}
