package web

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The raw guarantee, from two directions.
//
// It matters more than any other property in this package: the port is
// unauthenticated, and 45k rows in the real ledger predate the usage-object
// allow-list and still hold whole transcript lines. A response carrying raw
// would publish them.

// assertNoRaw fails if a serialised response contains the seeded raw payload.
// Every response test calls it; that is deliberate duplication - the guarantee
// is per-endpoint, and one central test would not notice a new one.
func assertNoRaw(t *testing.T, body []byte) {
	t.Helper()
	if bytes.Contains(body, []byte(rawMarker)) {
		t.Fatalf("a response carried the raw payload:\n%s", body)
	}
	if bytes.Contains(bytes.ToLower(body), []byte(`"raw"`)) {
		t.Fatalf("a response carried a raw field:\n%s", body)
	}
}

// TestNoEndpointServesRaw sweeps every endpoint with every parameter that could
// plausibly be mistaken for an opt-in. There is no such parameter, and this test
// is what keeps it that way.
func TestNoEndpointServesRaw(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	targets := []string{
		"/api/meta",
		"/api/summary",
		"/api/summary?group_by=session",
		"/api/facets",
		"/api/events",
		"/api/events?raw=1",
		"/api/events?include_raw=true",
		"/api/events?include-raw=1",
		"/api/events?with_raw=yes",
		"/api/events?fields=raw",
		"/api/events?limit=1000&cursor=0&raw=true",
	}
	for _, target := range targets {
		rec := get(t, srv, target)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d, want 200 (body: %s)", target, rec.Code, rec.Body.String())
		}
		assertNoRaw(t, rec.Body.Bytes())
	}
}

// TestPackageNeverProjectsRaw parses this package's own source: no non-test file
// may so much as NAME store.WithRaw. A future handler could pass the option
// behind a parameter no test happens to send, and the response checks above
// would never fire; this one fails the moment the call is written.
//
// It reads the syntax tree rather than grepping, so the package documentation is
// free to discuss the option it must never call - and so a build-tagged file
// (embed_webui.go) is checked even in an untagged test run, since the parser
// does not honour build tags.
func TestPackageNeverProjectsRaw(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && id.Name == "WithRaw" {
				t.Errorf("%s names store.WithRaw at %s; no serving path may ask for the audit payload",
					name, fset.Position(id.Pos()))
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files were checked; the guard is watching nothing")
	}
}
