package web

import (
	"net/http"
	"strings"
	"testing"
)

// TestHostAllowlistRefusesAForeignName is the DNS rebinding defence. A page on
// evil.example can point that name at 127.0.0.1 and then read this API as
// same-origin; the Host header is the one part of that request the page cannot
// forge, so it is what the server checks.
func TestHostAllowlistRefusesAForeignName(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	for _, target := range []string{"/api/meta", "/api/summary", "/api/facets", "/api/events", "/api/ws", "/"} {
		rec := do(t, srv, http.MethodGet, target, "evil.example")
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("GET %s with a foreign Host = %d, want 421", target, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("GET %s refusal content-type = %q, want plain text", target, ct)
		}
		if body := rec.Body.String(); !strings.Contains(body, AllowedHostsFlagName) {
			t.Errorf("GET %s refusal does not name the flag: %q", target, body)
		}
	}

	// The ledger must not leak through the refusal itself.
	rec := do(t, srv, http.MethodGet, "/api/events", "evil.example")
	assertNoRaw(t, rec.Body.Bytes())
	if strings.Contains(rec.Body.String(), "gpt-5") {
		t.Errorf("the refusal body carried ledger data: %q", rec.Body.String())
	}
}

// TestHostAllowlistAcceptsLoopbackOnAnyPort: the port is chosen with --addr and
// is not a secret, so it plays no part in the decision.
func TestHostAllowlistAcceptsLoopbackOnAnyPort(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())

	for _, host := range []string{
		"localhost", "localhost:37800", "LOCALHOST:1234",
		"127.0.0.1", "127.0.0.1:5173",
		"[::1]:37800", "::1", "[0:0:0:0:0:0:0:1]:9",
	} {
		rec := do(t, srv, http.MethodGet, "/api/meta", host)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/meta with Host %q = %d, want 200", host, rec.Code)
		}
	}
}

// TestAllowedHostsFlagExtendsTheSet: a deployment behind a reverse proxy that
// preserves the public Host is served only after its name is listed.
func TestAllowedHostsFlagExtendsTheSet(t *testing.T) {
	path := seedLedger(t, defaultEvents())
	srv, err := New(openReader(t, path), Options{
		DBPath:       path,
		AllowedHosts: []string{"aiusage.example", "  ", "OTHER.Example"},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	for _, host := range []string{"aiusage.example", "aiusage.example:443", "other.example"} {
		if rec := do(t, srv, http.MethodGet, "/api/meta", host); rec.Code != http.StatusOK {
			t.Errorf("GET /api/meta with the configured Host %q = %d, want 200", host, rec.Code)
		}
	}
	// The defaults survive the extension, and everything else is still refused.
	if rec := do(t, srv, http.MethodGet, "/api/meta", "127.0.0.1:37800"); rec.Code != http.StatusOK {
		t.Errorf("loopback = %d after extending the set, want 200", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/meta", "evil.example"); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host = %d, want 421", rec.Code)
	}
	// An empty entry must not become an allowlist member: a request with no
	// Host would then be accepted.
	if rec := do(t, srv, http.MethodGet, "/api/meta", ""); rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("empty Host = %d, want 421", rec.Code)
	}
}

// TestNormalizeHost covers the shapes a Host header actually arrives in.
func TestNormalizeHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"localhost", "localhost"},
		{"LocalHost:8080", "localhost"},
		{"127.0.0.1:37800", "127.0.0.1"},
		{"[::1]:37800", "::1"},
		{"[::1]", "::1"},
		{"::1", "::1"},
		{"0:0:0:0:0:0:0:1", "::1"},
		{"  aiusage.Example  ", "aiusage.example"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := normalizeHost(tc.in); got != tc.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
