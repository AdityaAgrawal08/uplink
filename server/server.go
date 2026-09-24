package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/argon2"
)

// upgrader configures the WebSocket handshake.
//
// CheckOrigin: same-origin policy for browsers. Default allow-all preserves
// the relay's native-client behavior (non-browser WS has no Origin), but a
// hostile web page could otherwise drive a victim's browser to join rooms.
// Set UPLINK_ALLOWED_ORIGINS (comma-separated, e.g.
// "https://app.example.com") to enforce an allowlist; empty means allow-all
// with this warning logged at startup.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     checkWSOrigin,
}

func checkWSOrigin(r *http.Request) bool {
	allow := os.Getenv("UPLINK_ALLOWED_ORIGINS")
	if strings.TrimSpace(allow) == "" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client
	}
	for _, a := range strings.Split(allow, ",") {
		if strings.TrimSpace(a) != "" && origin == strings.TrimSpace(a) {
			return true
		}
	}
	return false
}

// Server holds all in-memory state.
type Server struct {
	cfg      Config
	sessions map[string]*Session
	mu       sync.RWMutex
	limiter  *RateLimiter
	// pendingKeys stashes pubkeys supplied via HTTP create/join
	// ("sessionId\x00username" → pubkey) until the WS upgrade registers
	// the live Connection. Prevents silent pubkey drops.
	pendingKeys map[string]string
}

// NewServer creates a server with the given configuration.
func NewServer(cfg Config) *Server {
	return &Server{
		cfg:         cfg,
		sessions:    make(map[string]*Session),
		limiter:     NewRateLimiter(cfg.RateLimitPerSec),
		pendingKeys: make(map[string]string),
	}
}

// ─── HTTP routing ───────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	n := len(s.sessions)
	s.mu.RUnlock()
	jsonResponse(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sessions": n,
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
		Pubkey   string `json:"pubkey,omitempty"`
		PubKey   string `json:"publicKey,omitempty"`
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
		var err error
		passwordHash, err = hashPassword(req.Password)
		if err != nil {
			log.Printf("password hashing failed: %v", err)
			jsonError(w, http.StatusInternalServerError, "password setup failed")
			return
		}
	}

	session := &Session{
		Id:           sessionId,
		PasswordHash: passwordHash,
		ExpiresAt:    now.Add(duration),
		Users:        make(map[string]*Connection),
		Uploads:      make(map[string]*announcedUpload),
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

	// Validate optional pubkey (never silently drop): stash for the WS
	// upgrade if well-formed, reject if malformed.
	if pk := firstNonEmpty(req.Pubkey, req.PubKey); pk != "" {
		if _, err := decodeClientPubkey(strings.TrimSpace(pk)); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid pubkey (expected base64 32-byte key)")
			return
		}
		s.mu.Lock()
		s.pendingKeys[sessionId+"\x00"+req.Username] = strings.TrimSpace(pk)
		s.mu.Unlock()
	}

	// Atomic reserve: generate + insert under one Lock so concurrent
	// creates cannot collide on the same 6-digit ID.
	s.mu.Lock()
	for {
		if _, exists := s.sessions[sessionId]; !exists {
			s.sessions[sessionId] = session
			break
		}
		sessionId = drawSessionId()
		session.Id = sessionId
	}
	s.mu.Unlock()

	// Creator is auto-joined (they don't need to call join separately).
	// But we don't have their WebSocket yet — they'll join via wsUpgrade.
	// For HTTP create, we just return the session ID.

	log.Printf("session created: %s by %s (expires %s)", sessionId, req.Username, session.ExpiresAt.Format(time.RFC3339))

	jsonResponse(w, http.StatusCreated, map[string]string{
		"sessionId": sessionId,
	})
}

// drawSessionId draws one candidate 6-digit code (lock-free).
func drawSessionId() string {
	return genNumericId(6)
}

// generateSessionId returns an unused code, reserving it atomically is done
// by the caller (see handleCreateSession which generates + inserts under one
// Lock). Kept for tests and single-threaded callers.
func (s *Server) generateSessionId() string {
	for i := 0; i < 50; i++ {
		id := drawSessionId()
		s.mu.RLock()
		_, exists := s.sessions[id]
		s.mu.RUnlock()
		if !exists {
			return id
		}
	}
	// Exhausted retries: keep drawing from crypto/rand (never a
	// date-derived fallback that collides for a full minute).
	for {
		id := drawSessionId()
		s.mu.RLock()
		_, exists := s.sessions[id]
		s.mu.RUnlock()
		if !exists {
			return id
		}
	}
}

func genNumericId(n int) string {
	// Rejection-sample each digit to avoid modulo bias (256 % 10 != 0).
	digits := make([]byte, n)
	for i := range digits {
		for {
			var b [1]byte
			if _, err := rand.Read(b[:]); err != nil {
				// crypto/rand failure is effectively impossible here
				// (getrandom on modern kernels); fall back to time-nano
				// mixing rather than returning a colliding constant.
				digits[i] = byte(time.Now().UnixNano()%10) + '0'
				break
			}
			if b[0] < 250 { // 250 is the largest multiple of 10 below 256
				digits[i] = '0' + b[0]%10
				break
			}
		}
	}
	return string(digits)
}

// ─── HTTP join (non-WebSocket, for legacy clients) ─────────────────────────

func (s *Server) handleJoinSessionHTTP(w http.ResponseWriter, r *http.Request, sessionId string) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password,omitempty"`
		Pubkey   string `json:"pubkey,omitempty"`
		PubKey   string `json:"publicKey,omitempty"`
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
		// Rate-limit auth attempts (argon2id cost) per session.
		if !s.limiter.Allow("auth:" + sessionId) {
			jsonError(w, http.StatusTooManyRequests, "slow down — rate limited")
			return
		}
		if req.Password == "" {
			jsonError(w, http.StatusUnauthorized, "password required")
			return
		}
		if !verifyPassword(req.Password, session.PasswordHash) {
			jsonError(w, http.StatusUnauthorized, "incorrect password")
			return
		}
	}

	if pk := firstNonEmpty(req.Pubkey, req.PubKey); pk != "" {
		pk = strings.TrimSpace(pk)
		if _, err := decodeClientPubkey(pk); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid pubkey (expected base64 32-byte key)")
			return
		}
		s.mu.Lock()
		s.pendingKeys[sessionId+"\x00"+req.Username] = pk
		s.mu.Unlock()
	}

	// For HTTP join, we just validate and return the roster.
	// The actual real-time connection happens via WebSocket.
	jsonResponse(w, http.StatusOK, map[string]any{
		"sessionId":    sessionId,
		"participants": session.ActiveUsernames(),
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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
	if conn == nil {
		jsonError(w, http.StatusNotFound, "no active connection for user")
		return
	}
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
	// NOTE: query strings can land in proxy/access logs; prefer the
	// X-Uplink-Password header when the client can set it.
	password := r.URL.Query().Get("password")
	if password == "" {
		password = r.Header.Get("X-Uplink-Password")
	}
	if session.PasswordHash != "" {
		if password == "" {
			jsonError(w, http.StatusUnauthorized, "password required")
			return
		}
		// Rate-limit authentication attempts per session to blunt CPU-DoS
		// via argon2id (16 MiB x 3) on every join attempt.
		if !s.limiter.Allow("auth:" + sessionId) {
			jsonError(w, http.StatusTooManyRequests, "slow down — rate limited")
			return
		}
		if !verifyPassword(password, session.PasswordHash) {
			jsonError(w, http.StatusUnauthorized, "incorrect password")
			return
		}
	}

	// Enforce MaxSessionUsers (allow same-username reconnect to replace).
	session.Mu.RLock()
	_, isReconnect := session.Users[username]
	nUsers := len(session.Users)
	session.Mu.RUnlock()
	if !isReconnect && s.cfg.MaxSessionUsers > 0 && nUsers >= s.cfg.MaxSessionUsers {
		jsonError(w, http.StatusServiceUnavailable, "session is full")
		return
	}

	// Upgrade to WebSocket.
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade failed: %v", err)
		return
	}

	conn := newConnection(username, ws)
	// Carry the pubkey supplied at HTTP join/create time (if any) so the
	// WS-only join frame is not the sole key path. Clients encode padded
	// StdEncoding; accept both dialects.
	if pk := strings.TrimSpace(r.Header.Get("X-Uplink-Pubkey")); pk != "" {
		if _, err := decodeClientPubkey(pk); err == nil {
			conn.ClientPubKey = pk
		}
	} else {
		s.mu.Lock()
		if pk, ok := s.pendingKeys[sessionId+"\x00"+username]; ok {
			conn.ClientPubKey = pk
			delete(s.pendingKeys, sessionId+"\x00"+username)
		}
		s.mu.Unlock()
	}

	// Register in session (replaces existing connection if same username).
	// Never close(old.Send) here: Send is writer-owned. Signal the old
	// writer via shutdown() and let it close Send on exit.
	session.Mu.Lock()
	if old, exists := session.Users[username]; exists {
		old.shutdown()
	}
	session.Users[username] = conn
	session.Mu.Unlock()

	// Send welcome with roster, session public key, and recent history so
	// the RingBuffer is actually consumed by late-joiners. History rides
	// inside the welcome frame (not as separate chat frames) so existing
	// frame sequencing (welcome → system/users) is preserved.
	history := session.Buffer.Snapshot()
	welcome := Outbound{
		Type:          msgTypeWelcome,
		SessionId:     sessionId,
		SessionPubKey: session.ServerKeyPair.PublicB64(),
		Users:         session.ActiveUsernames(),
		History:       history,
	}
	_ = ws.WriteJSON(welcome)

	// Broadcast join system event.
	session.SendSystem(username + " joined")

	// Broadcast updated user list.
	session.Broadcast(Outbound{
		Type:  msgTypeUsers,
		Users: session.ActiveUsernames(),
	}, "")
	// If this connection arrived with a key, distribute immediately so E2E
	// does not depend solely on the WS join frame.
	if conn.ClientPubKey != "" {
		s.broadcastUserKeys(session)
	}

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
			safeSend(conn, Outbound{
				Type:    msgTypeError,
				Message: "invalid message encoding",
			})
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
		// Single owner of close(Send): mutually exclusive with trySend
		// via SendMu so a send can never race this close.
		conn.SendMu.Lock()
		if !conn.closed {
			conn.closed = true
			close(conn.Send)
		}
		conn.SendMu.Unlock()
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

		case <-conn.done:
			return

		case <-ticker.C:
			_ = conn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ─── Message handlers ──────────────────────────────────────────────────────

// decodeClientPubkey accepts both padded StdEncoding (what every CLI/
// landing client sends) and RawStdEncoding, returning 32 raw bytes.
func decodeClientPubkey(key string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(key); err == nil && len(raw) == 32 {
		return raw, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("expected base64-encoded 32-byte X25519 key")
	}
	return raw, nil
}

func (s *Server) handleJoin(session *Session, conn *Connection, in *Inbound) {
	// Client sends its public key after receiving the welcome.
	key := strings.TrimSpace(in.ClientPubKey)
	if key == "" {
		return
	}
	// Validate: must be base64 that decodes to a 32-byte X25519 public key.
	if _, err := decodeClientPubkey(key); err != nil {
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
	if text == "" {
		return
	}
	if len(text) > 500 {
		safeSend(conn, Outbound{
			Type:    msgTypeError,
			Message: "message exceeds 500 byte limit",
		})
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
		safeSend(conn, Outbound{Type: msgTypeError, Message: "invalid file metadata"})
		return
	}
	if in.TotalChunks > 4096 {
		safeSend(conn, Outbound{Type: msgTypeError, Message: "too many chunks"})
		return
	}
	if in.Size > s.cfg.MaxFileSize {
		safeSend(conn, Outbound{
			Type:    msgTypeError,
			Message: "file exceeds maximum size limit",
		})
		return
	}
	if s.cfg.FileChunkSize > 0 && in.Size > int64(in.TotalChunks)*int64(s.cfg.FileChunkSize)*4 {
		safeSend(conn, Outbound{Type: msgTypeError, Message: "file size inconsistent with chunk count"})
		return
	}

	// B56 FIX: never trust a client-supplied message ID. An empty MsgId
	// would break chunk correlation for every receiver; generate one
	// server-side when absent.
	msgId := in.MsgId
	if msgId == "" {
		msgId = genId()
	}

	// Register the announced upload so chunks can be validated.
	session.Mu.Lock()
	if session.Uploads == nil {
		session.Uploads = make(map[string]*announcedUpload)
	}
	session.Uploads[conn.Username+"\x00"+msgId] = &announcedUpload{
		Owner:       conn.Username,
		TotalChunks: in.TotalChunks,
		Size:        in.Size,
	}
	session.Mu.Unlock()

	msg := ChatMessage{
		MsgId:       msgId,
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
	// Validate against the announced upload; require ownership and bounds.
	// Rate-limit separately from chat so bulk data cannot amplify at line rate.
	if in.MsgId == "" || in.Data == "" {
		return
	}
	session.Mu.RLock()
	meta, ok := session.Uploads[conn.Username+"\x00"+in.MsgId]
	session.Mu.RUnlock()
	if !ok {
		safeSend(conn, Outbound{Type: msgTypeError, Message: "chunk for unknown file announcement"})
		return
	}
	if in.ChunkIndex < 0 || in.ChunkIndex >= meta.TotalChunks {
		safeSend(conn, Outbound{Type: msgTypeError, Message: "chunk index out of range"})
		return
	}
	if len(in.Data) > 1<<20 {
		safeSend(conn, Outbound{Type: msgTypeError, Message: "chunk too large"})
		return
	}
	if !s.limiter.Allow("chunk:" + session.Id + ":" + conn.Username) {
		safeSend(conn, Outbound{Type: msgTypeError, Message: "slow down — file rate limited"})
		return
	}
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
	// Only the original author may delete their message: look it up in the
	// recent buffer. Unknown IDs are rejected instead of broadcast blindly.
	if !session.Buffer.OwnedBy(in.DeleteMsgId, conn.Username) {
		safeSend(conn, Outbound{Type: msgTypeError, Message: "cannot delete unknown or foreign message"})
		return
	}
	out := Outbound{
		Type:        "delete",
		DeleteMsgId: in.DeleteMsgId,
		Username:    conn.Username,
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
// Non-blocking: drops the message if the channel is full (slow consumer)
// or the connection is shutting down. Never panics.
func safeSend(conn *Connection, msg Outbound) {
	data, err := jsonMarshal(msg)
	if err != nil {
		return
	}
	conn.trySend(data)
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
	// Only remove the map entry if it is still THIS connection.
	// After a reconnect the same username maps to a new Connection; the old
	// connection's readPump returning would otherwise evict the live one.
	cur, ok := session.Users[conn.Username]
	if ok && cur == conn {
		delete(session.Users, conn.Username)
	}
	empty := len(session.Users) == 0
	session.Mu.Unlock()

	// Signal the writer to exit; it owns close(Send). Never close(Send) here.
	conn.shutdown()

	if empty {
		// Defer to cleanerLoop for expiry sweeps; no per-departure sleeper
		// goroutine (previously one unsupervised 5-min sleeper per leave
		// that survived shutdown and raced cleanerLoop).
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

// cleanerLoop periodically sweeps expired sessions, notifying connected
// clients with msgTypeEnded before disconnecting them.
func (s *Server) cleanerLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for id, sess := range s.sessions {
			if now.After(sess.ExpiresAt) {
				// Notify + disconnect live members, then drop the room.
				sess.Mu.RLock()
				conns := make([]*Connection, 0, len(sess.Users))
				for _, c := range sess.Users {
					conns = append(conns, c)
				}
				empty := len(conns) == 0
				sess.Mu.RUnlock()
				if !empty {
					ended, _ := jsonMarshal(Outbound{Type: msgTypeEnded, Reason: "session expired"})
					for _, c := range conns {
						c.trySend(ended)
						c.shutdown()
					}
					sess.Mu.Lock()
					sess.Users = make(map[string]*Connection)
					sess.Mu.Unlock()
				}
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
// Returns an error instead of silently producing "" (the "no password"
// sentinel) when crypto/rand fails — callers must fail closed.
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("rand.Read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 16*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=16384,t=3,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
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
