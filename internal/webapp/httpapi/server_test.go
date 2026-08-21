package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/auth"
	"github.com/dbehnke/opusref/internal/webapp/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	_, err = s.CreateUser(context.Background(), store.CreateUser{Username: "alice", Role: store.RoleAdmin, PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{PublicOrigin: "https://radio.example.test", OpenAccess: true, SessionIdle: 12 * time.Hour, SessionAbsolute: 7 * 24 * time.Hour, MaxSessions: 3, Argon2: auth.DefaultParams()}, s), s
}
func TestSecurityHeadersAndMetricsIsolation(t *testing.T) {
	server, s := newTestServer(t)
	defer s.Close()
	for _, path := range []string{"/healthz", "/api/v1/session", "/missing"} {
		r := httptest.NewRequest(http.MethodGet, "https://radio.example.test"+path, nil)
		w := httptest.NewRecorder()
		server.PublicHandler().ServeHTTP(w, r)
		if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("missing security headers for %s", path)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	server.PublicHandler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("public metrics returned %d", w.Code)
	}
	w = httptest.NewRecorder()
	server.MonitorHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("monitor metrics returned %d", w.Code)
	}
}
func TestLoginIsSameOriginAndNonEnumerating(t *testing.T) {
	server, s := newTestServer(t)
	defer s.Close()
	login := func(username, password, origin string) *httptest.ResponseRecorder {
		body := `{"username":"` + username + `","password":"` + password + `"}`
		r := httptest.NewRequest(http.MethodPost, "https://radio.example.test/api/v1/auth/login", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		w := httptest.NewRecorder()
		server.PublicHandler().ServeHTTP(w, r)
		return w
	}
	for _, user := range []string{"unknown", "alice"} {
		w := login(user, "wrong password value", "https://radio.example.test")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", user, w.Code)
		}
		var response map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		if response["code"] != "authentication_failed" {
			t.Fatalf("enumerating response: %s", w.Body.String())
		}
	}
	if w := login("alice", "quiet marble nebula orchard", "https://evil.test"); w.Code != http.StatusForbidden {
		t.Fatalf("cross origin returned %d", w.Code)
	}
	if w := login("alice", "quiet marble nebula orchard", "https://radio.example.test"); w.Code != http.StatusOK || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
}
func TestChannelIDUsesCanonicalLosslessDecimal(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{{"9007199254740993", true}, {"18446744073709551615", true}, {"0", false}, {"01", false}, {"18446744073709551616", false}, {"9.1", false}} {
		got, err := parseChannelID(tc.value)
		if tc.ok && (err != nil || got == 0) {
			t.Fatalf("%q rejected: %v", tc.value, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q accepted", tc.value)
		}
	}
}
