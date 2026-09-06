package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func serve(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuth_DisabledWhenNoKeys(t *testing.T) {
	a := New(nil)
	if a.Enabled() {
		t.Fatal("Enabled with no keys")
	}
	rec := serve(a.Middleware(okHandler()), "POST", "/v1/pdf/html", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (auth disabled should pass through)", rec.Code)
	}
}

func TestAuth_BlankEntriesIgnored(t *testing.T) {
	if New([]string{" ", "", "\t"}).Enabled() {
		t.Fatal("blank-only key list should be disabled, not accept empty key")
	}
}

func TestAuth_AcceptsBothHeaderForms(t *testing.T) {
	h := New([]string{"secret1"}).Middleware(okHandler())
	for _, hdr := range []map[string]string{
		{"X-API-Key": "secret1"},
		{"Authorization": "Bearer secret1"},
	} {
		if rec := serve(h, "POST", "/v1/pdf/html", hdr); rec.Code != http.StatusOK {
			t.Fatalf("%v: status = %d, want 200", hdr, rec.Code)
		}
	}
}

func TestAuth_RejectsMissingAndWrong(t *testing.T) {
	h := New([]string{"secret1"}).Middleware(okHandler())
	for name, hdr := range map[string]map[string]string{
		"missing":      nil,
		"wrong":        {"X-API-Key": "nope"},
		"empty":        {"X-API-Key": ""},
		"bearer-wrong": {"Authorization": "Bearer nope"},
	} {
		rec := serve(h, "POST", "/v1/pdf/html", hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s: Content-Type = %q, want application/json", name, ct)
		}
		var env struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != "UNAUTHORIZED" {
			t.Fatalf("%s: body = %s (err %v)", name, rec.Body.String(), err)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%s: missing WWW-Authenticate", name)
		}
	}
}

func TestAuth_MultipleKeysForRotation(t *testing.T) {
	h := New([]string{"old", "new"}).Middleware(okHandler())
	for _, k := range []string{"old", "new"} {
		if rec := serve(h, "POST", "/x", map[string]string{"X-API-Key": k}); rec.Code != http.StatusOK {
			t.Fatalf("key %q: status = %d, want 200", k, rec.Code)
		}
	}
}

func TestAuth_ExemptPathsSkipCheck(t *testing.T) {
	h := New([]string{"secret1"}, "/livez", "/readyz").Middleware(okHandler())
	if rec := serve(h, "GET", "/livez", nil); rec.Code != http.StatusOK {
		t.Fatalf("/livez without key: status = %d, want 200", rec.Code)
	}
	if rec := serve(h, "POST", "/v1/pdf/html", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected path without key: status = %d, want 401", rec.Code)
	}
}
