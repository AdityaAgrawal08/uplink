package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startTestServer creates a test HTTP server and returns the Server + base URL.
func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Port = 0
	srv := NewServer(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api/v1/session/", srv.routeSession)

	ts := httptest.NewServer(mux)
	return srv, ts.URL
}

// dialWS connects a WebSocket client to the given session.
func dialWS(t *testing.T, baseURL, sessionId, username string) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) +
		"/api/v1/session/" + sessionId + "/ws"

	header := http.Header{}
	header.Set("X-Uplink-Username", username)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	return conn
}

// readMsg reads and decodes a JSON message from the WebSocket with a timeout.
func readMsg(t *testing.T, conn *websocket.Conn) Outbound {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var out Outbound
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	return out
}

// drainUntil reads messages from the WebSocket until a message matching the
// predicate is found, or timeout. Returns the matching message.
func drainUntil(t *testing.T, conn *websocket.Conn, pred func(Outbound) bool) Outbound {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("drain read: %v", err)
		}
		var msg Outbound
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if pred(msg) {
			return msg
		}
	}
	t.Fatal("drainUntil: timeout waiting for matching message")
	return Outbound{} // unreachable
}

func TestCreateSession(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"alice","duration":600}`
	resp, err := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["sessionId"] == "" {
		t.Fatal("expected sessionId in response")
	}
}

func TestJoinNonexistentSession(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"bob"}`
	resp, err := http.Post(baseURL+"/api/v1/session/999999/join", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCreateAndJoin(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"creator","duration":600}`
	resp, _ := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sessionId := created["sessionId"]

	joinBody := `{"username":"joiner"}`
	joinResp, err := http.Post(baseURL+"/api/v1/session/"+sessionId+"/join", "application/json",
		strings.NewReader(joinBody))
	if err != nil {
		t.Fatal(err)
	}
	defer joinResp.Body.Close()

	if joinResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", joinResp.StatusCode)
	}

	var joinResult map[string]any
	json.NewDecoder(joinResp.Body).Decode(&joinResult)
	if joinResult["sessionId"] != sessionId {
		t.Fatalf("expected same sessionId, got %v", joinResult["sessionId"])
	}
}

func TestWebSocketConnect(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"alice","duration":600}`
	resp, _ := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sessionId := created["sessionId"]

	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) +
		"/api/v1/session/" + sessionId + "/ws"
	header := http.Header{}
	header.Set("X-Uplink-Username", "alice")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("ws connect: %v", err)
	}
	defer conn.Close()

	welcome := readMsg(t, conn)
	if welcome.Type != msgTypeWelcome {
		t.Fatalf("expected welcome, got %s", welcome.Type)
	}
	if welcome.SessionId != sessionId {
		t.Fatalf("expected session %s, got %s", sessionId, welcome.SessionId)
	}
	if len(welcome.Users) != 1 || welcome.Users[0] != "alice" {
		t.Fatalf("expected [alice], got %v", welcome.Users)
	}
	if welcome.SessionPubKey == "" {
		t.Fatal("expected session public key in welcome")
	}
}

func TestWebSocketChat(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"alice","duration":600}`
	resp, _ := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sessionId := created["sessionId"]

	// Alice connects.
	alice := dialWS(t, baseURL, sessionId, "alice")
	defer alice.Close()
	_ = readMsg(t, alice) // welcome

	// Bob connects.
	bob := dialWS(t, baseURL, sessionId, "bob")
	defer bob.Close()
	_ = readMsg(t, bob) // welcome

	// Alice receives: system "bob joined" + users list update.
	// Drain until we see a users message with 2 entries.
	usersMsg := drainUntil(t, alice, func(m Outbound) bool {
		return m.Type == msgTypeUsers && len(m.Users) == 2
	})
	if len(usersMsg.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(usersMsg.Users))
	}

	// Alice sends a chat message.
	err := alice.WriteJSON(map[string]any{
		"type": "chat",
		"text": "hello bob!",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Bob should receive it (may need to drain system messages first).
	chatMsg := drainUntil(t, bob, func(m Outbound) bool {
		return m.Type == "chat"
	})
	if chatMsg.Username != "alice" {
		t.Fatalf("expected alice, got %s", chatMsg.Username)
	}
	if chatMsg.Text != "hello bob!" {
		t.Fatalf("expected 'hello bob!', got %q", chatMsg.Text)
	}
	if chatMsg.MsgId == "" {
		t.Fatal("expected msgId")
	}
	if chatMsg.CreatedAt == "" {
		t.Fatal("expected createdAt")
	}
}

func TestWebSocketPrivateMessage(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"alice","duration":600}`
	resp, _ := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sessionId := created["sessionId"]

	// Three users connect.
	alice := dialWS(t, baseURL, sessionId, "alice")
	defer alice.Close()
	_ = readMsg(t, alice) // welcome

	bob := dialWS(t, baseURL, sessionId, "bob")
	defer bob.Close()
	_ = readMsg(t, bob) // welcome

	charlie := dialWS(t, baseURL, sessionId, "charlie")
	defer charlie.Close()
	_ = readMsg(t, charlie) // welcome

	// Wait until alice sees all 3 users in the roster.
	drainUntil(t, alice, func(m Outbound) bool {
		return m.Type == msgTypeUsers && len(m.Users) == 3
	})

	// Alice sends private message to Bob.
	err := alice.WriteJSON(map[string]any{
		"type": "chat",
		"text": "secret for bob",
		"to":   "bob",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Bob should receive it.
	bobMsg := drainUntil(t, bob, func(m Outbound) bool {
		return m.Type == "chat"
	})
	if bobMsg.Text != "secret for bob" {
		t.Fatalf("bob expected 'secret for bob', got: %q", bobMsg.Text)
	}

	// Charlie should NOT receive the private chat message.
	// First drain any leftover join messages (system, users).
	deadline := time.Now().Add(500 * time.Millisecond)
	gotChat := false
	for time.Now().Before(deadline) {
		charlie.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, raw, err := charlie.ReadMessage()
		if err != nil {
			break // timeout — no more messages, which is what we want
		}
		var msg Outbound
		if json.Unmarshal(raw, &msg) == nil && msg.Type == "chat" {
			gotChat = true
			break
		}
		// It's a system/users message from the join sequence — continue draining.
	}
	if gotChat {
		t.Fatal("charlie should not have received the private message")
	}
}

func TestWebSocketDelete(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"alice","duration":600}`
	resp, _ := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sessionId := created["sessionId"]

	// Two users connect.
	alice := dialWS(t, baseURL, sessionId, "alice")
	defer alice.Close()
	_ = readMsg(t, alice) // welcome

	bob := dialWS(t, baseURL, sessionId, "bob")
	defer bob.Close()
	_ = readMsg(t, bob) // welcome

	// Drain alice's user list update.
	drainUntil(t, alice, func(m Outbound) bool {
		return m.Type == msgTypeUsers
	})

	// Alice sends a chat message.
	alice.WriteJSON(map[string]any{
		"type": "chat",
		"text": "delete me",
	})

	// Bob receives it (drain system messages first).
	chatMsg := drainUntil(t, bob, func(m Outbound) bool {
		return m.Type == "chat"
	})
	msgId := chatMsg.MsgId

	// Alice deletes the message.
	alice.WriteJSON(map[string]any{
		"type":        "delete",
		"deleteMsgId": msgId,
	})

	// Bob should receive the delete event.
	deleteMsg := drainUntil(t, bob, func(m Outbound) bool {
		return m.Type == "delete"
	})
	if deleteMsg.DeleteMsgId != msgId {
		t.Fatalf("expected deleteMsgId %s, got %s", msgId, deleteMsg.DeleteMsgId)
	}
	if deleteMsg.Username != "alice" {
		t.Fatalf("expected alice, got %s", deleteMsg.Username)
	}
}

func TestWebSocketHeartbeat(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"alice","duration":600}`
	resp, _ := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sessionId := created["sessionId"]

	alice := dialWS(t, baseURL, sessionId, "alice")
	defer alice.Close()
	_ = readMsg(t, alice) // welcome

	alice.WriteJSON(map[string]any{"type": "heartbeat"})

	ack := drainUntil(t, alice, func(m Outbound) bool {
		return m.Type == "heartbeat-ack"
	})
	if len(ack.Users) != 1 || ack.Users[0] != "alice" {
		t.Fatalf("expected [alice], got %v", ack.Users)
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", result["status"])
	}
}

func TestWebSocketSessionEnd(t *testing.T) {
	srv, baseURL := startTestServer(t)
	_ = srv

	body := `{"username":"alice","duration":1}`
	resp, _ := http.Post(baseURL+"/api/v1/session/create", "application/json",
		strings.NewReader(body))
	var created map[string]string
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	sessionId := created["sessionId"]

	alice := dialWS(t, baseURL, sessionId, "alice")
	defer alice.Close()
	_ = readMsg(t, alice) // welcome

	// Manually expire the session.
	srv.mu.Lock()
	if s, ok := srv.sessions[sessionId]; ok {
		s.ExpiresAt = time.Now().Add(-time.Second)
	}
	srv.mu.Unlock()

	// Clean expired sessions.
	srv.mu.Lock()
	for id, s := range srv.sessions {
		s.Mu.RLock()
		empty := len(s.Users) == 0
		s.Mu.RUnlock()
		if empty && time.Now().After(s.ExpiresAt) {
			delete(srv.sessions, id)
		}
	}
	srv.mu.Unlock()
}
