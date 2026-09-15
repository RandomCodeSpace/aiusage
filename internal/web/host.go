package web

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// The Host allow-list.
//
// Binding loopback is not, by itself, protection. A page on any site can point
// a name it controls at 127.0.0.1 (DNS rebinding) and then fetch this API from
// a document the browser considers same-origin: same scheme, same name, same
// port, so no CORS preflight and no opaque response; the page reads the whole
// ledger. The one thing that page cannot forge is the Host header, because the
// browser sets it from the name it resolved. So every request is matched
// against the names this server answers to, and anything else is refused
// before it reaches a handler.
//
// 421 Misdirected Request is the accurate status: the request is well formed
// and the server simply is not the one it was addressed to. The body names the
// flag, because the other party who lands here is an operator behind a reverse
// proxy, not an attacker.

// AllowedHostsFlagName is the serve flag that extends the allow-list. The
// refusal body names it, so it lives next to the check.
const AllowedHostsFlagName = "--allowed-hosts"

var defaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

type hostSet map[string]bool

func newHostSet(extra []string) hostSet {
	set := make(hostSet, len(defaultAllowedHosts)+len(extra))
	for _, h := range defaultAllowedHosts {
		set[normalizeHost(h)] = true
	}
	for _, h := range extra {
		if n := normalizeHost(h); n != "" {
			set[n] = true
		}
	}
	return set
}

// allows reports whether a Host header value (with or without a port) names
// this server. The port is ignored: it is chosen with --addr, it is not a
// secret, and requiring it would make every port change a configuration change.
func (h hostSet) allows(hostport string) bool {
	n := normalizeHost(hostport)
	return n != "" && h[n]
}

// normalizeHost reduces a Host header to the form the set is keyed by: no
// port, no brackets, lowercase, and IP literals in canonical spelling so ::1
// and 0:0:0:0:0:0:0:1 are one entry.
func normalizeHost(hostport string) string {
	h := strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return strings.ToLower(h)
}

func (s *Server) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.hosts.allows(r.Host) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusMisdirectedRequest)
		fmt.Fprintf(w, "aiusage does not answer to host %q\n\n"+
			"It serves %s by default. A name that reaches it through a reverse proxy\n"+
			"(which preserves the public Host) has to be listed with %s.\n",
			r.Host, strings.Join(defaultAllowedHosts, ", "), AllowedHostsFlagName)
	})
}
