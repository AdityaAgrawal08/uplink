package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/argon2"
)

// upgrader configures the WebSocket handshake.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Server holds all in-memory state.
type Server struct {
	cfg      Config
	sessions map[string]*Session
	mu       sync.RWMutex
	limiter  *RateLimiter
}

// NewServer creates a server with the given configuration.
func NewServer(cfg Config) *Server {
	return &Server{
		cfg:      cfg,
		sessions: make(map[string]*Session),
		limiter:  NewRateLimiter(cfg.RateLimitPerSec),
	}
}

// ─── HTTP routing ───────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"sessions": len(s.sessions),
	})
}

// routeSession dispatches based on method + sub-path:
//
//	POST /api/v1/session/create          → createSession
//	POST /api/v1/session/{id}/join       → joinSession (HTTP, returns 200)
//	GET  /api/v1/session/{id}/ws         → wsUpgrade
//	POST /api/v1/session/{id}/leave      → leaveSession (HTTP fallback)
func (s *Server) routeSession(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/session/")
	parts := strings.SplitN(path, "/", 2)
	key := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch {
	case key == "create" && r.Method == http.MethodPost:
		s.handleCreateSession(w, r)
	case r.Method == http.MethodPost && sub == "join":
		s.handleJoinSessionHTTP(w, r, key)
	case r.Method == http.MethodGet && sub == "ws":
		s.handleWSUpgrade(w, r, key)
	case r.Method == http.MethodPost && sub == "leave":
		s.handleLeaveHTTP(w, r, key)
	default:
		http.NotFound(w, r)
	}
}

// ─── Session creation ──────────────────────────────────────────────────────

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password,omitempty"`
		Duration int    `json:"duration,omitempty"` // seconds
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !isValidUsername(req.Username) {
		jsonError(w, http.StatusBadRequest, "username must be 3-20 alphanumeric/underscore chars")
		return
	}
	duration := s.cfg.SessionTTL
	if req.Duration > 0 {
		d := time.Duration(req.Duration) * time.Second
		if d > 0 && d <= 24*time.Hour {
			duration = d
		}
	}

	sessionId := s.generateSessionId()
	now := time.Now()

	var passwordHash string
	if req.Password != "" {
		passwordHash = hashPassword(req.Password)
	}

	session := &Session{
		Id:           sessionId,
		PasswordHash: passwordHash,
		ExpiresAt:    now.Add(duration),
		Users:        make(map[string]*Connection),
		Buffer:       NewRingBuffer(s.cfg.MessageBufSize),
	}

	// Generate ephemeral X25519 server keypair for this session.
	kp, err := GenerateKeyPair()
	if err != nil {
		log.Printf("keypair generation failed: %v", err)
		jsonError(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	session.ServerKeyPair = kp

	s.mu.Lock()
	s.sessions[sessionId] = session
	s.mu.Unlock()

	// Creator is auto-joined (they don't need to call join separately).
	// But we don't have their WebSocket yet — they'll join via wsUpgrade.
	// For HTTP create, we just return the session ID.

	log.Printf("session created: %s by %s (expires %s)", sessionId, req.Username, session.ExpiresAt.Format(time.RFC3339))

	jsonResponse(w, http.StatusCreated, map[string]string{
		"sessionId": sessionId,
	})
}

// generateSessionId generates a 6-digit numeric code.
func (s *Server) generateSessionId() string {
	for i := 0; i < 20; i++ {
		id := genNumericId(6)
		s.mu.RLock()
		_, exists := s.sessions[id]
		s.mu.RUnlock()
		if !exists {
			return id
		}
	}
	// Fallback: use timestamp-based ID
	return time.Now().Format("010204")
}

func genNumericId(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = '0' + b[i]%10
	}
	return string(b)
}

// ─── HTTP join (non-WebSocket, for legacy clients) ─────────────────────────

func (s *Server) handleJoinSessionHTTP(w http.ResponseWriter, r *http.Request, sessionId string) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !isValidUsername(req.Username) {
		jsonError(w, http.StatusBadRequest, "invalid username")
		return
	}

	session := s.getSession(sessionId)
	if session == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}
	if time.Now().After(session.ExpiresAt) {
		jsonError(w, http.StatusGone, "session has expired")
		return
	}
	if session.PasswordHash != "" {
		if req.Password == "" {
			jsonError(w, http.StatusUnauthorized, "password required")
			return
		}
		if !verifyPassword(req.Password, session.PasswordHash) {
			jsonError(w, http.StatusUnauthorized, "incorrect password")
			return
		}
	}

	// For HTTP join, we just validate and return the roster.
	// The actual real-time connection happens via WebSocket.
	jsonResponse(w, http.StatusOK, map[string]any{
		"sessionId":   sessionId,
		"participants": session.ActiveUsernames(),
	})
}

// ─── HTTP leave (fallback for clients without WebSocket) ───────────────────

func (s *Server) handleLeaveHTTP(w http.ResponseWriter, r *http.Request, sessionId string) {
	username := sanitizeHeader(r.Header.Get("X-Uplink-Username"))
	if username == "" {
		jsonError(w, http.StatusBadRequest, "X-Uplink-Username header required")
		return
	}
	session := s.getSession(sessionId)
	if session == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}
	// HTTP leave: look up the live connection for this username (if any).
	session.Mu.RLock()
	conn := session.Users[username]
	session.Mu.RUnlock()
	s.removeUser(session, conn)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── WebSocket upgrade ─────────────────────────────────────────────────────

func (s *Server) handleWSUpgrade(w http.ResponseWriter, r *http.Request, sessionId string) {
	username := sanitizeHeader(r.Header.Get("X-Uplink-Username"))
	if !isValidUsername(username) {
		jsonError(w, http.StatusBadRequest, "valid X-Uplink-Username header required")
		return
	}

	session := s.getSession(sessionId)
	if session == nil {
		jsonError(w, http.StatusNotFound, "session not found")
		return
	}
	if time.Now().After(session.ExpiresAt) {
		jsonError(w, http.StatusGone, "session has expired")
		return
	}

	// Check password from query param (WebSocket can't send custom headers
	// easily on reconnect, so we accept it as a query parameter too).
	password := r.URL.Query().Get("password")
	if session.PasswordHash != "" && password != "" {
		if !verifyPassword(password, session.PasswordHash) {
			jsonError(w, http.StatusUnauthorized, "incorrect password")
			return
		}
	}

	// Upgrade to WebSocket.
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade failed: %v", err)
		return
	}

	conn := &Connection{
		Username: username,
		Conn:     ws,
		Send:     make(chan []byte, 256),
		LastBeat: time.Now(),
	}

	// Register in session (replaces existing connection if same username).
	session.Mu.Lock()
	if old, exists := session.Users[username]; exists {
		// Close old connection.
		close(old.Send)
		_ = old.Conn.Close()
	}
	session.Users[username] = conn
	session.Mu.Unlock()

	// Send welcome with roster and session public key.
	welcome := Outbound{
		Type:          msgTypeWelcome,
		SessionId:     sessionId,
		SessionPubKey: session.ServerKeyPair.PublicB64(),
		Users:         session.ActiveUsernames(),
	}
	_ = ws.WriteJSON(welcome)

	// Broadcast join system event.
	session.SendSystem(username + " joined")

	// Broadcast updated user list.
	session.Broadcast(Outbound{
		Type:  msgTypeUsers,
		Users: session.ActiveUsernames(),
	}, "")

	log.Printf("[%s] %s connected (users: %d)", sessionId, username, len(session.ActiveUsernames()))

	// Start reader and writer goroutines.
	go s.writePump(session, conn)
	s.readPump(session, conn)

	// When readPump returns, the client disconnected.
	s.removeUser(session, conn)
	session.SendSystem(username + " left")
	session.Broadcast(Outbound{
		Type:  msgTypeUsers,
		Users: session.ActiveUsernames(),
	}, "")
	log.Printf("[%s] %s disconnected (users: %d)", sessionId, username, len(session.ActiveUsernames()))
}

// ─── readPump (per-connection) ─────────────────────────────────────────────

func (s *Server) readPump(session *Session, conn *Connection) {
	defer func() {
		_ = conn.Conn.Close()
	}()

	conn.Conn.SetReadLimit(1 << 20) // 1 MB max message
	_ = conn.Conn.SetReadDeadline(time.Now().Add(s.cfg.HeartbeatTimeout * 2))
	conn.Conn.SetPongHandler(func(string) error {
		conn.LastBeat = time.Now()
		_ = conn.Conn.SetReadDeadline(time.Now().Add(s.cfg.HeartbeatTimeout * 2))
		return nil
	})

	for {
		_, raw, err := conn.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[%s] read error from %s: %v", session.Id, conn.Username, err)
			}
			return
		}

		var in Inbound
		if err := jsonUnmarshal(raw, &in); err != nil {
			continue
		}

		switch in.Type {
		case msgTypeJoin:
			s.handleJoin(session, conn, &in)
		case msgTypeChat:
			s.handleChat(session, conn, &in)
		case msgTypeFile:
			s.handleFileMeta(session, conn, &in)
		case msgTypeFileChunk:
			s.handleFileChunk(session, conn, &in)
		case msgTypeFileComplete:
			s.handleFileComplete(session, conn, &in)
		case msgTypeDelete:
			s.handleDelete(session, conn, &in)
		case msgTypeHeartbeat:
			s.handleHeartbeat(session, conn)
		default:
			// Unknown type — ignore.
		}
	}
}

// ─── writePump (per-connection) ────────────────────────────────────────────

func (s *Server) writePump(session *Session, conn *Connection) {
	ticker := time.NewTicker(s.cfg.HeartbeatTimeout / 3)
	defer func() {
		ticker.Stop()
		_ = conn.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-conn.Send:
			_ = conn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = conn.Conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := conn.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}

		case <-ticker.C:
			_ = conn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ─── Message handlers ──────────────────────────────────────────────────────

func (s *Server) handleJoin(session *Session, conn *Connection, in *Inbound) {
	// Client sends its public key after receiving the welcome.
	key := strings.TrimSpace(in.ClientPubKey)
	if key == "" {
		return
	}
	// Validate: must be base64 that decodes to a 32-byte X25519 public key.
	raw, err := base64.RawStdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		safeSend(conn, Outbound{
			Type:    msgTypeError,
			Message: "invalid clientPublicKey (expected base64-encoded 32-byte X25519 key)",
		})
		return
	}
	conn.ClientPubKey = key
	// Distribute the updated key set to everyone so peers can derive shared
	// secrets. (Previously this was a no-op stub, so E2E was never wired up.)
	s.broadcastUserKeys(session)
}

func (s *Server) handleChat(session *Session, conn *Connection, in *Inbound) {
	text := strings.TrimSpace(in.Text)
	if text == "" || len(text) > 500 {
		return
	}

	// Rate limit: max messages per second per user.
	key := session.Id + ":" + conn.Username
	if !s.limiter.Allow(key) {
		safeSend(conn, Outbound{
			Type:    msgTypeError,
			Message: "slow down — rate limited",
		})
		return
	}

	msg := ChatMessage{
		MsgId:     genId(),
		Username:  conn.Username,
		Kind:      "chat",
		Text:      text,
		To:        in.To,
		CreatedAt: time.Now(),
	}
	session.Buffer.Push(msg)

	out := Outbound{
		Type:      "chat",
		MsgId:     msg.MsgId,
		Username:  msg.Username,
		Text:      msg.Text,
		To:        msg.To,
		Kind:      msg.Kind,
		CreatedAt: msg.CreatedAt.UTC().Format(time.RFC3339),
	}

	if in.To != "" {
		// Private message: send only to sender and recipient.
		session.SendTo(in.To, out)
		session.SendTo(conn.Username, out)
	} else {
		// Broadcast.
		session.Broadcast(out, "")
	}
}

func (s *Server) handleFileMeta(session *Session, conn *Connection, in *Inbound) {
	if in.Filename == "" || in.Size <= 0 || in.TotalChunks <= 0 {
		return
	}
	if in.Size > s.cfg.MaxFileSize {
		safeSend(conn, Outbound{
			Type:    msgTypeError,
			Message: "file exceeds maximum size limit",
		})
		return
	}

	msg := ChatMessage{
		MsgId:       in.MsgId,
		Username:    conn.Username,
		Kind:        "file",
		Filename:    in.Filename,
		Size:        in.Size,
		SHA256:      in.SHA256,
		TotalChunks: in.TotalChunks,
		CreatedAt:   time.Now(),
	}
	session.Buffer.Push(msg)

	out := Outbound{
		Type:        "file",
		MsgId:       msg.MsgId,
		Username:    msg.Username,
		Filename:    msg.Filename,
		Size:        msg.Size,
		SHA256:      msg.SHA256,
		TotalChunks: msg.TotalChunks,
		To:          in.To,
		Kind:        "file",
		CreatedAt:   msg.CreatedAt.UTC().Format(time.RFC3339),
	}

	if in.To != "" {
		session.SendTo(in.To, out)
		session.SendTo(conn.Username, out)
	} else {
		session.Broadcast(out, "")
	}
}

func (s *Server) handleFileChunk(session *Session, conn *Connection, in *Inbound) {
	// Relay the encrypted chunk to the appropriate recipients.
	out := Outbound{
		Type:       "file-chunk",
		MsgId:      in.MsgId,
		ChunkIndex: in.ChunkIndex,
		Data:       in.Data,
	}

	if in.To != "" {
		session.SendTo(in.To, out)
	} else {
		session.Broadcast(out, conn.Username)
	}
}

func (s *Server) handleFileComplete(session *Session, conn *Connection, in *Inbound) {
	out := Outbound{
		Type:  "file-complete",
		MsgId: in.MsgId,
	}

	if in.To != "" {
		session.SendTo(in.To, out)
		session.SendTo(conn.Username, out)
	} else {
		session.Broadcast(out, "")
	}
}

func (s *Server) handleDelete(session *Session, conn *Connection, in *Inbound) {
	if in.DeleteMsgId == "" {
		return
	}
	// Broadcast the delete event. Any client can delete their own messages;
	// the server doesn't enforce ownership (trust-based for v1).
	out := Outbound{
		Type:       "delete",
		DeleteMsgId: in.DeleteMsgId,
		Username:   conn.Username,
	}
	session.Broadcast(out, "")
}

func (s *Server) handleHeartbeat(session *Session, conn *Connection) {
	conn.LastBeat = time.Now()
	safeSend(conn, Outbound{
		Type:  "heartbeat-ack",
		Users: session.ActiveUsernames(),
	})
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// safeSend sends a JSON message through the connection's send channel.
// Non-blocking: drops the message if the channel is full (slow consumer).
func safeSend(conn *Connection, msg Outbound) {
	data, err := jsonMarshal(msg)
	if err != nil {
		return
	}
	select {
	case conn.Send <- data:
	default:
	}
}

func (s *Server) getSession(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

func (s *Server) removeUser(session *Session, conn *Connection) {
	if conn == nil {
		return
	}
	session.Mu.Lock()
	// B37 FIX: only remove the map entry if it is still THIS connection.
	// After a reconnect the same username maps to a new Connection; the old
	// connection's readPump returning would otherwise evict the live one.
	cur, ok := session.Users[conn.Username]
	if ok && cur == conn {
		close(conn.Send)
		_ = conn.Conn.Close()
		delete(session.Users, conn.Username)
	}
	empty := len(session.Users) == 0
	session.Mu.Unlock()

	if empty {
		// Schedule session cleanup after a grace period.
		go func() {
			time.Sleep(5 * time.Minute)
			session.Mu.RLock()
			stillEmpty := len(session.Users) == 0
			session.Mu.RUnlock()
			if stillEmpty && time.Now().After(session.ExpiresAt) {
				s.mu.Lock()
				delete(s.sessions, session.Id)
				s.mu.Unlock()
				log.Printf("session %s cleaned up (empty + expired)", session.Id)
			}
		}()
	}
}

func (s *Server) broadcastUserKeys(session *Session) {
	session.Mu.RLock()
	keys := make([]UserKey, 0, len(session.Users))
	for _, c := range session.Users {
		if c.ClientPubKey != "" {
			keys = append(keys, UserKey{Username: c.Username, PublicKey: c.ClientPubKey})
		}
	}
	session.Mu.RUnlock()
	if len(keys) == 0 {
		return
	}
	session.Broadcast(Outbound{Type: msgTypeKeys, Keys: keys}, "")
}

// cleanerLoop periodically sweeps expired sessions.
func (s *Server) cleanerLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		// Clean expired sessions.
		s.mu.Lock()
		for id, sess := range s.sessions {
			sess.Mu.RLock()
			empty := len(sess.Users) == 0
			sess.Mu.RUnlock()
			if empty && now.After(sess.ExpiresAt) {
				delete(s.sessions, id)
				log.Printf("cleaner: removed expired session %s", id)
			}
		}
		s.mu.Unlock()
		// Clean expired rate limiter windows.
		s.limiter.Cleanup()
	}
}

// hashPassword hashes a password with argon2id using a per-password random
// salt, encoded as $argon2id$v=19$m=16384,t=3,p=1$<salt-b64>$<hash-b64>.
func hashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return ""
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 16*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=16384,t=3,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash))
}

func verifyPassword(password, hash string) bool {
	// Format: $argon2id$v=19$m=16384,t=3,p=1$<salt-b64>$<hash-b64>
	parts := strings.SplitN(hash, "$", 6)
	if len(parts) != 6 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	computed := argon2.IDKey([]byte(password), salt, 3, 16*1024, 1, 32)
	return subtle.ConstantTimeCompare(computed, expected) == 1
}
