package main

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ─── WebSocket wire messages ────────────────────────────────────────────────

// Inbound message types from client.
const (
	msgTypeJoin         = "join"
	msgTypeChat         = "chat"
	msgTypeFile         = "file"
	msgTypeFileChunk    = "file-chunk"
	msgTypeFileComplete = "file-complete"
	msgTypeDelete       = "delete"
	msgTypeHeartbeat    = "heartbeat"
)

// Outbound message types to client.
const (
	msgTypeWelcome = "welcome"
	msgTypeUsers   = "users"
	msgTypeKeys    = "keys"
	msgTypeSystem  = "system"
	msgTypeEnded   = "ended"
	msgTypeError   = "error"
)

// Inbound is the envelope every client→server frame is decoded into.
type Inbound struct {
	Type         string `json:"type"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	ClientPubKey string `json:"clientPublicKey,omitempty"`

	// chat
	Text string `json:"text,omitempty"`
	To   string `json:"to,omitempty"`

	// file metadata
	MsgId       string `json:"msgId,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	TotalChunks int    `json:"totalChunks,omitempty"`

	// file chunk
	ChunkIndex int    `json:"chunkIndex,omitempty"`
	Data       string `json:"data,omitempty"` // base64 ciphertext

	// delete
	DeleteMsgId string `json:"deleteMsgId,omitempty"`
}

// Outbound is the envelope every server→client frame is encoded as.
type Outbound struct {
	Type string `json:"type"`

	// welcome
	SessionId     string        `json:"sessionId,omitempty"`
	SessionPubKey string        `json:"sessionPublicKey,omitempty"`
	Users         []string      `json:"users,omitempty"`
	History       []ChatMessage `json:"history,omitempty"`

	// keys (E2E public-key distribution)
	Keys []UserKey `json:"keys,omitempty"`

	// chat / file / delete
	MsgId     string `json:"msgId,omitempty"`
	Username  string `json:"username,omitempty"`
	Text      string `json:"text,omitempty"`
	To        string `json:"to,omitempty"`
	Kind      string `json:"kind,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`

	// file
	Filename    string `json:"filename,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	TotalChunks int    `json:"totalChunks,omitempty"`
	ChunkIndex  int    `json:"chunkIndex,omitempty"`
	Data        string `json:"data,omitempty"`

	// system / ended / error
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`

	// delete
	DeleteMsgId string `json:"deleteMsgId,omitempty"`
}

// UserKey is one participant's X25519 public key, distributed to peers so
// they can derive shared E2E encryption keys.
type UserKey struct {
	Username  string `json:"username"`
	PublicKey string `json:"publicKey"`
}

// ChatMessage is a single transcript entry held in the ring buffer.
type ChatMessage struct {
	MsgId       string    `json:"msgId"`
	Username    string    `json:"username"`
	Kind        string    `json:"kind"` // "chat", "system", "file", "delete"
	Text        string    `json:"text,omitempty"`
	To          string    `json:"to,omitempty"`
	Filename    string    `json:"filename,omitempty"`
	Size        int64     `json:"size,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	TotalChunks int       `json:"totalChunks,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// ─── Ring buffer (fixed-capacity, thread-safe) ──────────────────────────────

// RingBuffer stores the last N messages for late-joiners.
type RingBuffer struct {
	mu       sync.Mutex
	msgs     []ChatMessage
	capacity int
	head     int // next write position
	count    int // how many slots are occupied
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		msgs:     make([]ChatMessage, capacity),
		capacity: capacity,
	}
}

// Push appends a message, overwriting the oldest if full.
func (r *RingBuffer) Push(m ChatMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs[r.head] = m
	r.head = (r.head + 1) % r.capacity
	if r.count < r.capacity {
		r.count++
	}
}

// Snapshot returns a copy of all buffered messages in chronological order.
func (r *RingBuffer) Snapshot() []ChatMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return nil
	}
	out := make([]ChatMessage, r.count)
	start := (r.head - r.count + r.capacity) % r.capacity
	for i := 0; i < r.count; i++ {
		out[i] = r.msgs[(start+i)%r.capacity]
	}
	return out
}

// OwnedBy reports whether msgId exists in the buffer and was authored by
// username (used to authorize deletes).
func (r *RingBuffer) OwnedBy(msgId, username string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < r.count; i++ {
		m := r.msgs[(r.head-r.count+i+r.capacity*2)%r.capacity]
		if m.MsgId == msgId {
			return m.Username == username
		}
	}
	return false
}

// ─── Connection (one per WebSocket client) ──────────────────────────────────

// Connection wraps a single WebSocket client.
//
// Lifecycle: the writer goroutine (writePump) owns close(Send) and runs it
// exactly once on exit under SendMu. Evictors/removers never close(Send);
// they call shutdown() which closes done once and closes the network conn.
// Senders use trySend(), which holds SendMu across the closed-check and the
// channel send, so a send can never race the writer's close.
type Connection struct {
	Username     string
	Conn         *websocket.Conn
	Send         chan []byte
	LastBeat     time.Time
	SendMu       sync.Mutex
	done         chan struct{}
	closed       bool
	closeOnce    sync.Once
	ServerPubKey string // X25519 public key distributed to this client
	ClientPubKey string // X25519 public key sent by this client
}

// newConnection builds a Connection with lifecycle channels initialized.
func newConnection(username string, ws *websocket.Conn) *Connection {
	return &Connection{
		Username: username,
		Conn:     ws,
		Send:     make(chan []byte, 256),
		LastBeat: time.Now(),
		done:     make(chan struct{}),
	}
}

// shutdown signals the writer to exit and closes the network connection.
// It never closes Send (writer-owned). Safe for concurrent use.
func (c *Connection) shutdown() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.Conn.Close()
	})
}

// trySend queues one message unless the connection is shutting down.
// Never panics: the closed-check and the send share SendMu with the
// writer's close(Send).
func (c *Connection) trySend(data []byte) bool {
	c.SendMu.Lock()
	defer c.SendMu.Unlock()
	if c.closed {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.Send <- data:
		return true
	case <-c.done:
		return false
	default:
		// channel full — drop (slow consumer)
		return false
	}
}

// ─── Session (one per chat room) ───────────────────────────────────────────

// announcedUpload tracks a file meta announcement so later chunks can be
// validated (owner, bounds) instead of relayed blindly.
type announcedUpload struct {
	Owner       string
	TotalChunks int
	Size        int64
}

// Session is an in-memory chat room.
type Session struct {
	Id            string
	PasswordHash  string // argon2id hash; empty = no password
	ExpiresAt     time.Time
	Users         map[string]*Connection      // username → connection
	Uploads       map[string]*announcedUpload // "username\x00msgId" → meta
	Buffer        *RingBuffer
	ServerKeyPair *KeyPair // ephemeral X25519 keypair for this session
	Mu            sync.RWMutex
}

// ActiveUsernames returns a sorted list of connected usernames.
func (s *Session) ActiveUsernames() []string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	names := make([]string, 0, len(s.Users))
	for u := range s.Users {
		names = append(names, u)
	}
	return names
}

// Broadcast sends an outbound message to every connection in the session
// except the sender (identified by excludeUser). If excludeUser is empty,
// all connections receive the message.
func (s *Session) Broadcast(msg Outbound, excludeUser string) {
	data, err := jsonMarshal(msg)
	if err != nil {
		return
	}
	s.Mu.RLock()
	conns := make([]*Connection, 0, len(s.Users))
	for _, c := range s.Users {
		if c.Username == excludeUser {
			continue
		}
		conns = append(conns, c)
	}
	s.Mu.RUnlock()
	for _, c := range conns {
		c.trySend(data)
	}
}

// SendTo sends an outbound message to a specific user.
func (s *Session) SendTo(username string, msg Outbound) {
	data, err := jsonMarshal(msg)
	if err != nil {
		return
	}
	s.Mu.RLock()
	c, ok := s.Users[username]
	s.Mu.RUnlock()
	if !ok {
		return
	}
	c.trySend(data)
}

// SendSystem broadcasts a system event (join/leave) to all participants.
func (s *Session) SendSystem(text string) {
	s.Buffer.Push(ChatMessage{
		MsgId:     genId(),
		Username:  "system",
		Kind:      "system",
		Text:      text,
		CreatedAt: time.Now(),
	})
	s.Broadcast(Outbound{
		Type:      msgTypeSystem,
		Text:      text,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, "")
}
