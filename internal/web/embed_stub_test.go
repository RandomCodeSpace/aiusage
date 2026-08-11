//go:build !webui

package web

import (
	"net/http"
	"strings"
	"testing"
)

// TestStubBuildHasNoUI: a default build serves the API and says plainly that it
// has no page, rather than 404ing anonymously or pretending to serve one.
func TestStubBuildHasNoUI(t *testing.T) {
	if HasEmbeddedUI() {
		t.Fatal("HasEmbeddedUI() is true in an untagged build")
	}

	srv, _ := newTestServer(t, defaultEvents())
	for _, target := range []string{"/", "/index.html", "/sessions/abc"} {
		rec := get(t, srv, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 in a build with no UI", target, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "no embedded web ui") {
			t.Errorf("GET %s body = %q, want it to name the missing capability", target, body)
		}
	}

	// The API is unaffected: only the page is missing.
	if rec := get(t, srv, "/api/meta"); rec.Code != http.StatusOK {
		t.Errorf("GET /api/meta = %d in a stub build, want 200", rec.Code)
	}
}
