// Package auth is the v1 slice of auth-and-tenancy (CAPABILITY-MAP.md):
// shared static API-key authentication as HTTP middleware. It is
// deliberately the smallest useful thing — one or more pre-shared keys,
// accepted on every conversion endpoint, verified in constant time — not
// per-key rate limiting, usage metering, or a key store. Those remain
// auth-and-tenancy's later slices; this exists so the service is not open to
// anyone who can reach its port, which every deployment guide currently
// warns about in bold.
//
// Keys are compared with crypto/subtle so a wrong key cannot be recovered by
// timing the response. Multiple keys are supported purely so a key can be
// rotated without downtime (add the new one, migrate callers, drop the old).
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Authenticator verifies a request carries one of a fixed set of API keys.
// The zero value is a no-op middleware (Enabled reports false) so tests and
// local runs can opt out explicitly.
type Authenticator struct {
	keys   [][]byte
	exempt map[string]bool
}

// New builds an Authenticator for the given keys. Blank entries are ignored,
// so splitting an unset env var yields a disabled Authenticator rather than
// one that accepts "". exemptPaths (typically the health probes) skip the
// check entirely — a load balancer must be able to probe without a key.
func New(keys []string, exemptPaths ...string) *Authenticator {
	a := &Authenticator{exempt: make(map[string]bool, len(exemptPaths))}
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			a.keys = append(a.keys, []byte(k))
		}
	}
	for _, p := range exemptPaths {
		a.exempt[p] = true
	}
	return a
}

// Enabled reports whether any key is configured. When false, Middleware
// passes every request through untouched.
func (a *Authenticator) Enabled() bool { return a != nil && len(a.keys) > 0 }

// Middleware returns a handler wrapper enforcing the key check. It is placed
// inside the request-logging middleware in cmd/api so a rejected request is
// still logged with its request id.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.exempt[r.URL.Path] || a.valid(presentedKey(r)) {
			next.ServeHTTP(w, r)
			return
		}
		// One message for "missing" and "wrong" alike — telling a caller
		// which one it was only helps an attacker.
		w.Header().Set("WWW-Authenticate", `Bearer realm="pdf"`)
		writeUnauthorized(w)
	})
}

// presentedKey reads the key from X-API-Key, or from an Authorization:
// Bearer header — the two forms pdf-service-saas and its callers already
// use.
func presentedKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// valid reports whether candidate matches any configured key. Every key is
// compared (no early return) and the comparison itself is constant-time, so
// neither which key matched nor how far a near-miss got is observable by
// timing.
func (a *Authenticator) valid(candidate string) bool {
	if candidate == "" {
		return false
	}
	cb := []byte(candidate)
	matched := 0
	for _, k := range a.keys {
		matched |= subtle.ConstantTimeCompare(cb, k)
	}
	return matched == 1
}

// errorEnvelope here is intentionally the same shape as internal/api's
// errorEnvelope — one JSON error contract across the whole service — but
// duplicated rather than imported to keep auth free of a dependency on the
// HTTP layer it wraps.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    "UNAUTHORIZED",
			"message": "a valid API key is required (X-API-Key or Authorization: Bearer)",
		},
	})
}
