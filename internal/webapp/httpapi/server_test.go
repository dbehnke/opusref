package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
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

func TestLocalHTTPSAndWSSHandshake(t *testing.T) {
	state, err := store.Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	server := New(Config{OpenAccess: true, Argon2: auth.DefaultParams()}, state)
	tlsServer := httptest.NewUnstartedServer(server.PublicHandler())
	server.cfg.PublicOrigin = "https://" + tlsServer.Listener.Addr().String()
	tlsServer.StartTLS()
	defer tlsServer.Close()
	wssURL := "wss" + strings.TrimPrefix(tlsServer.URL, "https") + "/api/v1/ws"
	header := http.Header{}
	header.Set("Origin", server.cfg.PublicOrigin)
	conn, _, err := websocket.Dial(context.Background(), wssURL, &websocket.DialOptions{HTTPClient: tlsServer.Client(), HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello := []byte(`{"api_version":1,"type":"hello","request_id":"h1","body":{"audio":{"encoder":true,"decoder":true,"context_rate":48000},"csrf_token":""}}`)
	if err = conn.Write(context.Background(), websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	_, response, err := conn.Read(context.Background())
	if err != nil || !strings.Contains(string(response), `"type":"hello_ok"`) {
		t.Fatalf("response=%s err=%v", response, err)
	}
}

func TestMediaOutputQueueEnforcesBytesAndHalfSecondSpan(t *testing.T) {
	queue := newMediaOutputQueue(8, 10)
	first := socketOutput{kind: websocket.MessageBinary, data: make([]byte, 6), timestamp: 100}
	if !queue.enqueue(first) {
		t.Fatal("first media was rejected")
	}
	if queue.enqueue(socketOutput{kind: websocket.MessageBinary, data: make([]byte, 5), timestamp: 200}) {
		t.Fatal("byte limit was not enforced")
	}
	if queue.enqueue(socketOutput{kind: websocket.MessageBinary, data: []byte{1}, timestamp: 24_101}) {
		t.Fatal("time-span limit was not enforced")
	}
	item := <-queue.items
	queue.taken(item)
	if !queue.enqueue(socketOutput{kind: websocket.MessageBinary, data: make([]byte, 10), timestamp: 24_101}) {
		t.Fatal("queue accounting did not release written bytes")
	}
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

func TestPasskeyOptionsResponseIsFlattened(t *testing.T) {
	result := passkeyOptionsResponse("ceremony", map[string]any{"publicKey": map[string]any{"rpId": "radio.example.test"}})
	if result["ceremony_id"] != "ceremony" || result["publicKey"] == nil || result["options"] != nil {
		t.Fatalf("response=%+v", result)
	}
}

func TestRecordingQueryUsesOpaqueValidatedCursorAndFilters(t *testing.T) {
	stamp := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	cursor := encodeRecordingCursor(stamp, "recording-id")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/recordings?limit=200&callsign=n0call&status=partial&cursor="+cursor, nil)
	query, err := recordingQuery(r)
	if err != nil {
		t.Fatal(err)
	}
	if query.Limit != 200 || query.Source != "N0CALL" || query.Status != "partial" || query.BeforeID != "recording-id" {
		t.Fatalf("query=%+v", query)
	}
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/recordings?cursor=not-base64", nil)
	if _, err = recordingQuery(bad); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestAdminCursorIsEndpointBound(t *testing.T) {
	cursor := encodeAdminCursor(adminCursor{Kind: "accounts", Username: "alice", ID: "user-id"})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?limit=25&cursor="+cursor, nil)
	limit, decoded, err := adminPageRequest(r, "accounts")
	if err != nil || limit != 25 || decoded.ID != "user-id" {
		t.Fatalf("limit=%d cursor=%+v err=%v", limit, decoded, err)
	}
	if _, _, err = adminPageRequest(r, "audit"); err == nil {
		t.Fatal("cross-endpoint cursor accepted")
	}
}
