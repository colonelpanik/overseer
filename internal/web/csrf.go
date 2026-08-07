package web

import (
	"net"
	"net/http"
	"strings"
)

// requireSameOrigin protects a state-changing handler from being driven by a
// hostile page open in the operator's own browser.
//
// The dashboard's forms are plain application/x-www-form-urlencoded POSTs,
// which the fetch spec classifies as a CORS "simple request": a cross-origin
// page can submit one with no preflight and no CORS headers involved at all,
// so the browser just sends it. The attacker cannot read the response, but
// against `overseer serve` that does not matter — the response is thrown
// away while the daemon has already started a `claude --permission-mode
// bypassPermissions` run on whatever goal the hostile page chose.
//
// Two independent checks close this:
//
//   - Sec-Fetch-Site, sent by every modern browser on same-site and
//     cross-site requests alike, rejects the cross-origin form post outright.
//   - The Host header check stops DNS rebinding: a page cannot forge what
//     Host header the browser sends, even when it has tricked the browser
//     into resolving some attacker-controlled name to 127.0.0.1.
//
// Neither is a full CSRF token, but a token would need the dashboard to hand
// one out and every form to carry it forward; these two checks close the
// same hole using only information the browser already attaches to the
// request, and reject a request that has neither with no loss of legitimate
// traffic — a browser navigating to the dashboard itself always sets
// Sec-Fetch-Site to same-origin (or omits it only on browsers old enough to
// predate the header, which also predate fetch metadata as an attack
// surface).
func (s *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if !hostAllowed(r.Host, s.cfg.ListenAddr) {
			http.Error(w, "unrecognized Host header", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// hostAllowed reports whether reqHost — the Host header on an incoming
// request — plausibly names the server's own configured listener.
//
// The port must always match. For a specific bind address (the common case:
// 127.0.0.1) the hostname must too, though loopback's usual spellings
// (127.0.0.1, localhost, ::1) are treated as interchangeable since they all
// reach the same listener. A wildcard bind (0.0.0.0, ::, or no host at all)
// has no single "real" hostname to pin to — the operator chose to accept
// connections addressed to any of this machine's names — so only the port is
// checked in that case; DNS rebinding cannot be closed by hostname alone
// against a listener that was deliberately opened to everything.
func hostAllowed(reqHost, listenAddr string) bool {
	lHost, lPort, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// A bare port, or something else unparsable: fail closed rather than
		// silently accepting every Host header.
		return strings.EqualFold(reqHost, listenAddr)
	}
	rHost, rPort, err := net.SplitHostPort(reqHost)
	if err != nil {
		return false
	}
	if rPort != lPort {
		return false
	}
	rHost = strings.ToLower(rHost)
	switch strings.ToLower(lHost) {
	case "", "0.0.0.0", "::":
		return true
	case "127.0.0.1", "localhost", "::1":
		return rHost == "127.0.0.1" || rHost == "localhost" || rHost == "::1"
	default:
		return rHost == strings.ToLower(lHost)
	}
}
