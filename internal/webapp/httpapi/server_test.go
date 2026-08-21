package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dbehnke/opusref/internal/webapp/auth"
	"github.com/dbehnke/opusref/internal/webapp/store"
)

type blockingSocketWriter struct {
	mu      sync.Mutex
	writes  [][]byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingSocketWriter) Write(_ context.Context, _ websocket.MessageType, data []byte) error {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	w.mu.Lock()
	w.writes = append(w.writes, append([]byte(nil), data...))
	w.mu.Unlock()
	return nil
}
func (w *blockingSocketWriter) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.writes...)
}

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

func TestReadinessIsCachedAndIdenticalAcrossSurfaces(t *testing.T) {
	state, err := store.Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	hash, _ := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	_, _ = state.CreateUser(context.Background(), store.CreateUser{Username: "admin", Role: store.RoleAdmin, PasswordHash: hash})
	var healthy atomic.Bool
	var checks atomic.Int32
	server := New(Config{OpenAccess: true, Argon2: auth.DefaultParams(), ReadyCheck: func() bool { checks.Add(1); return healthy.Load() }}, state)
	before := checks.Load()
	for index := 0; index < 3; index++ {
		if server.statusData()["ready"] != false {
			t.Fatal("public status was ready while dependency cache was false")
		}
	}
	if checks.Load() != before {
		t.Fatal("public status performed a synchronous dependency probe")
	}
	healthy.Store(true)
	server.RefreshReadiness(context.Background())
	if server.statusData()["ready"] != true {
		t.Fatal("public status did not use refreshed readiness")
	}
	w := httptest.NewRecorder()
	server.readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("readyz=%d", w.Code)
	}
	server.BeginDrain()
	if server.statusData()["ready"] != false {
		t.Fatal("draining status remained ready")
	}
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

func TestMediaOutputQueueTracksRetainedHeadAfterPartialDrain(t *testing.T) {
	queue := newMediaOutputQueue(8, 100)
	first := socketOutput{kind: websocket.MessageBinary, data: []byte{1}, timestamp: 100}
	second := socketOutput{kind: websocket.MessageBinary, data: []byte{2}, timestamp: 200}
	if !queue.enqueue(first) || !queue.enqueue(second) {
		t.Fatal("setup enqueue failed")
	}
	item := <-queue.items
	queue.taken(item)
	if !queue.enqueue(socketOutput{kind: websocket.MessageBinary, data: []byte{3}, timestamp: 24_101}) {
		t.Fatal("departed queue head was retained in time accounting")
	}
}
func TestPlaybackGenerationRetiresQueuedAndWriterHeldMedia(t *testing.T) {
	queue := newMediaOutputQueue(2, 100)
	first := socketOutput{kind: websocket.MessageBinary, data: []byte{1}, playback: true, generation: 1, sequence: 7}
	second := socketOutput{kind: websocket.MessageBinary, data: []byte{2}, playback: true, generation: 1, sequence: 8}
	if !queue.enqueue(first) || !queue.enqueue(second) {
		t.Fatal("playback queue setup failed")
	}
	held := <-queue.items
	queue.taken(held)
	queue.discard()
	if currentPlaybackOutput(held, 2) {
		t.Fatal("writer-held media survived the pause generation barrier")
	}
	resumed := socketOutput{kind: websocket.MessageBinary, data: []byte{3}, playback: true, generation: 2, sequence: 7}
	if !currentPlaybackOutput(resumed, 2) || resumed.sequence != held.sequence {
		t.Fatal("ordinary resume did not preserve the packet sequence")
	}
	seek := socketOutput{kind: websocket.MessageBinary, data: []byte{4}, playback: true, generation: 3, sequence: 0}
	if currentPlaybackOutput(resumed, 3) || !currentPlaybackOutput(seek, 3) || seek.sequence != 0 {
		t.Fatal("seek did not retire the prior epoch and reset its sequence")
	}
}

func TestBlockedWriterDoesNotSendRetiredPlaybackAfterLifecycleResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &blockingSocketWriter{started: make(chan struct{}), release: make(chan struct{})}
	controls := make(chan socketOutput, 2)
	live := newMediaOutputQueue(1, 100)
	playback := newMediaOutputQueue(2, 100)
	var generation atomic.Uint64
	generation.Store(1)
	if !live.enqueue(socketOutput{kind: websocket.MessageBinary, data: []byte("live"), timestamp: 1}) {
		t.Fatal("live setup failed")
	}
	done := make(chan error, 1)
	go func() {
		done <- writeSocketOutputs(ctx, writer, controls, live, playback, &generation, func(socketOutput) {})
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not block")
	}
	if !playback.enqueue(socketOutput{kind: websocket.MessageBinary, data: []byte("old-1"), playback: true, generation: 1}) || !playback.enqueue(socketOutput{kind: websocket.MessageBinary, data: []byte("old-2"), playback: true, generation: 1}) {
		t.Fatal("playback queue was not full")
	}
	generation.Store(2)
	playback.discard()
	controls <- socketOutput{kind: websocket.MessageText, data: []byte("paused")}
	if !playback.enqueue(socketOutput{kind: websocket.MessageBinary, data: []byte("new"), playback: true, generation: 2}) {
		t.Fatal("new playback enqueue failed")
	}
	close(writer.release)
	deadline := time.Now().Add(time.Second)
	for len(writer.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	writes := writer.snapshot()
	if len(writes) != 3 || string(writes[0]) != "live" || string(writes[1]) != "paused" || string(writes[2]) != "new" {
		t.Fatalf("write order=%q", writes)
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

func TestRateLimitedIPCannotFillUsernameLimiter(t *testing.T) {
	server, state := newTestServer(t)
	defer state.Close()
	for index := 0; index < 4100; index++ {
		body := fmt.Sprintf(`{"username":"fresh-%d","password":"wrong password value"}`, index)
		request := httptest.NewRequest(http.MethodPost, "https://radio.example.test/api/v1/auth/login", strings.NewReader(body))
		request.RemoteAddr = "192.0.2.1:1234"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://radio.example.test")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		response := httptest.NewRecorder()
		server.PublicHandler().ServeHTTP(response, request)
		if index >= 5 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d returned %d", index, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "https://radio.example.test/api/v1/auth/login", strings.NewReader(`{"username":"unrelated-fresh","password":"wrong password value"}`))
	request.RemoteAddr = "198.51.100.1:1234"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://radio.example.test")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	response := httptest.NewRecorder()
	server.PublicHandler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("fresh source was starved: %d", response.Code)
	}
	now := time.Now()
	for _, category := range []string{"ws_ip", "passkey_ip", "passkey_account", "admin"} {
		if !server.limiter.Allow(category, "unrelated", 1, time.Minute, now) {
			t.Fatalf("%s was starved by fresh login usernames", category)
		}
	}
}

func TestSensitiveRouteFailuresWriteRedactedAuditRecords(t *testing.T) {
	server, state := newTestServer(t)
	defer state.Close()
	user, err := state.FindUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	session := store.Session{ID: "session", UserID: user.ID, Username: user.Username, Role: store.RoleAdmin}
	target := user.ID
	if err = state.InsertRecording(context.Background(), target, "WEB", "N0CALL", "", target+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		action string
		call   func(http.ResponseWriter, *http.Request, store.Session)
		path   string
	}{
		{"logout", server.logout, "/api/v1/auth/logout"},
		{"reauth_password", server.reauthPassword, "/api/v1/me/reauth/password"},
		{"password_change", server.changePassword, "/api/v1/me/password"},
		{"session_revoke", server.deleteSession, "/api/v1/me/sessions/other"},
		{"reauth_passkey", server.passkeyReauthVerify, "/api/v1/me/reauth/passkey/verify"},
		{"passkey_enroll", server.passkeyEnrollVerify, "/api/v1/me/passkeys/verify"},
		{"passkey_rename", server.renamePasskey, "/api/v1/me/passkeys/key"},
		{"passkey_delete", server.deletePasskey, "/api/v1/me/passkeys/key"},
		{"recording_delete", server.deleteRecording, "/api/v1/admin/recordings/" + target},
		{"passkeys_clear", server.clearAccountPasskeys, "/api/v1/admin/accounts/" + target + "/passkeys"},
		{"account_update", server.updateAccount, "/api/v1/admin/accounts/" + target},
		{"password_reset", server.resetAccountPassword, "/api/v1/admin/accounts/" + target + "/password"},
		{"sessions_revoke", server.revokeAccountSessions, "/api/v1/admin/accounts/" + target + "/sessions/revoke"},
		{"account_delete", server.deleteAccount, "/api/v1/admin/accounts/" + target},
		{"account_create", server.createAccount, "/api/v1/admin/accounts"},
	}
	for _, test := range tests {
		r := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"password":"must-not-appear"}`))
		r.SetPathValue("id", target)
		test.call(httptest.NewRecorder(), r, session)
	}
	events, err := state.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, test := range tests {
		want[test.action] = true
	}
	for _, event := range events {
		if want[event.Action] {
			if event.Outcome != "failure" || event.Details != "{}" || strings.Contains(event.Details, "must-not-appear") {
				t.Fatalf("unsafe audit event: %+v", event)
			}
			delete(want, event.Action)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing failure audits: %v", want)
	}
}

func TestPasskeyLimiterUsesSharedIPAndAccountBuckets(t *testing.T) {
	server, state := newTestServer(t)
	defer state.Close()
	r := httptest.NewRequest(http.MethodPost, "https://radio.example.test/api/v1/me/passkeys/options", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	for index := 0; index < 10; index++ {
		if !server.allowPasskeyAttempt(r, "account-a") {
			t.Fatalf("attempt %d was limited early", index+1)
		}
	}
	if server.allowPasskeyAttempt(r, "account-a") {
		t.Fatal("shared passkey IP/account bucket allowed attempt 11")
	}
	r.RemoteAddr = "192.0.2.11:1234"
	if server.allowPasskeyAttempt(r, "account-a") {
		t.Fatal("new IP bypassed the account bucket")
	}
	if !server.allowPasskeyAttempt(r, "account-b") {
		t.Fatal("independent account and IP were limited")
	}
}

func TestAuditWriteFailureHasFixedTelemetryAndOperatorEvent(t *testing.T) {
	server, state := newTestServer(t)
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	server.audit(context.Background(), "password_change", "failure", "", "", "")
	if server.telemetry.value("opusrefweb_audit_writes_total", "failure") != 1 || server.telemetry.value("opusrefweb_db_errors_total", "audit") != 1 {
		t.Fatal("audit write failure telemetry was not incremented")
	}
	events := server.telemetry.recent()
	if len(events) != 1 || events[0].Kind != "audit_failure" || strings.Contains(events[0].Message, "password") {
		t.Fatalf("unsafe or missing operator event: %+v", events)
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

func TestGlobalWebSocketLimitIsExactly250(t *testing.T) {
	server, state := newTestServer(t)
	defer state.Close()
	server.cfg.MaxWebSockets = 250
	for index := 0; index < 250; index++ {
		if !server.acquireSocket("") {
			t.Fatalf("socket %d was rejected", index+1)
		}
	}
	if server.acquireSocket("") {
		t.Fatal("socket 251 was accepted")
	}
	for index := 0; index < 250; index++ {
		server.releaseSocket("")
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

func TestAdminEventsEndpointReturnsBoundedNewestFirstContract(t *testing.T) {
	server, state := newTestServer(t)
	defer state.Close()
	admin, err := state.FindUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _, err := state.CreateSession(context.Background(), admin.ID, time.Now(), 12*time.Hour, 7*24*time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 300; index++ {
		server.telemetry.event("rate_limit", "warning", "A fixed operator message.")
	}
	request := httptest.NewRequest(http.MethodGet, "https://radio.example.test/api/v1/admin/events", nil)
	request.AddCookie(&http.Cookie{Name: CookieName, Value: raw})
	response := httptest.NewRecorder()
	server.PublicHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
	}
	var envelope struct {
		APIVersion int `json:"api_version"`
		Data       struct {
			Items []operatorEvent `json:"items"`
		} `json:"data"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	items := envelope.Data.Items
	if envelope.APIVersion != 1 || len(items) != 256 || items[0].ID != 300 || items[len(items)-1].ID != 45 {
		t.Fatalf("version=%d items=%d first=%+v last=%+v", envelope.APIVersion, len(items), items[0], items[len(items)-1])
	}
	if items[0].Time.IsZero() || items[0].Kind != "rate_limit" || items[0].Severity != "warning" || items[0].Message != "A fixed operator message." {
		t.Fatalf("event=%+v", items[0])
	}
	unauthorized := httptest.NewRecorder()
	server.PublicHandler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "https://radio.example.test/api/v1/admin/events", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
}

func TestSessionExplainsForcedPasswordChangeState(t *testing.T) {
	server, state := newTestServer(t)
	defer state.Close()
	hash, err := auth.HashPassword("temporary marble nebula orchard", auth.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	user, err := state.CreateUser(context.Background(), store.CreateUser{Username: "temporary", Role: store.RoleUser, PasswordHash: hash, PasswordChangeRequired: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _, err := state.CreateSession(context.Background(), user.ID, time.Now(), 12*time.Hour, 7*24*time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://radio.example.test/api/v1/session", nil)
	request.AddCookie(&http.Cookie{Name: CookieName, Value: raw})
	response := httptest.NewRecorder()
	server.PublicHandler().ServeHTTP(response, request)
	var envelope struct {
		Data struct {
			Authenticated        bool   `json:"authenticated"`
			Username             string `json:"username"`
			ForcedPasswordChange bool   `json:"forced_password_change"`
			CSRFToken            string `json:"csrf_token"`
		} `json:"data"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !envelope.Data.Authenticated || envelope.Data.Username != "temporary" || !envelope.Data.ForcedPasswordChange || envelope.Data.CSRFToken == "" {
		t.Fatalf("status=%d session=%+v", response.Code, envelope.Data)
	}
}
