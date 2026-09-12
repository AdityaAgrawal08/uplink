package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ─── P2P wire protocol (v1, JSON frames over WebRTC data channels) ──────────
//
// Layering: Noise_XX establishes a per-peer encrypted channel (see
// p2p_noise.go). Every frame below travels INSIDE that channel, so `data`
// fields are already ciphertext when they hit the wire. Frame headers
// (type/msgId/from/to) stay plaintext for routing — they carry no content.
// WebRTC's own DTLS encrypts the transport too, but since SDP fingerprints
// pass through our (untrusted) signaling server, app-layer Noise is what
// actually delivers the E2E guarantee.

const protoVersion = 1

// Frame types.
const (
	frameChat         = "chat"
	frameAck          = "ack"
	frameFileMeta     = "file-meta"
	frameFileChunk    = "file-chunk"
	frameFileComplete = "file-complete"
	frameTyping       = "typing"
)

// Max plaintext bytes per file chunk (encrypted individually so receivers
// can stream-verify and the fallback inbox caps stay meaningful).
const frameChunkSize = 64 * 1024

type frame struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	MsgId      string `json:"msgId"`
	From       string `json:"from"`
	To         string `json:"to,omitempty"`
	Data       string `json:"data,omitempty"`       // base64 ciphertext (chat) or chunk (file)
	Filename   string `json:"filename,omitempty"`   // file-meta
	Size       int64  `json:"size,omitempty"`       // file-meta
	SHA256     string `json:"sha256,omitempty"`     // file-meta
	Chunks     int    `json:"chunks,omitempty"`     // file-meta: total chunk count
	ChunkIndex int    `json:"chunkIndex,omitempty"` // file-chunk
	Active     bool   `json:"active,omitempty"`     // typing
}

func newFrame(ftype, msgId, from, to string) frame {
	return frame{V: protoVersion, Type: ftype, MsgId: msgId, From: from, To: to}
}

func encodeFrame(f frame) ([]byte, error) {
	return json.Marshal(f)
}

func decodeFrame(raw []byte) (frame, error) {
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	if f.V != protoVersion {
		return f, fmt.Errorf("unsupported protocol version %d", f.V)
	}
	switch f.Type {
	case frameChat, frameAck, frameFileMeta, frameFileChunk, frameFileComplete, frameTyping:
		return f, nil
	default:
		return f, fmt.Errorf("unknown frame type %q", f.Type)
	}
}

// newMsgId mints a 128-bit random message ID (hex). Globally unique for
// practical purposes; doubles as the dedup key and inbox key.
func newMsgId() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ─── Dedup set (bounded, thread-safe) ───────────────────────────────────────

// seenSet remembers recent message IDs so at-least-once delivery (retries,
// racing duplicate paths) surfaces each message exactly once. Oldest entries
// evict past capacity — matches Telegram's bounded-recent-IDs pattern.
type seenSet struct {
	mu   sync.Mutex
	cap  int
	ids  map[string]struct{}
	order []string
}

func newSeenSet(capacity int) *seenSet {
	return &seenSet{cap: capacity, ids: make(map[string]struct{})}
}

//seen reports whether id was already observed. First observation records it.
func (s *seenSet) seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return true
	}
	s.ids[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.cap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.ids, oldest)
	}
	return false
}

// ─── Chunking ───────────────────────────────────────────────────────────────

// splitChunks slices plaintext into frame-sized pieces for streaming.
func splitChunks(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	var out [][]byte
	for i := 0; i < len(data); i += frameChunkSize {
		end := i + frameChunkSize
		if end > len(data) {
			end = len(data)
		}
		out = append(out, data[i:end])
	}
	return out
}

// ─── Safety codes (server key-swap detection) ───────────────────────────────

// safetyCode derives a human-comparable fingerprint from two identity public
// keys (order-independent). Both sides display it; users compare once out of
// band (read aloud, like Signal/WhatsApp safety numbers). A server swapping
// keys to man-in-the-middle changes the code on one side and the attack is
// exposed. Rendered as 4 groups for easy reading.
func safetyCode(pubA, pubB []byte) string {
	keys := [][]byte{pubA, pubB}
	sort.Slice(keys, func(i, j int) bool {
		return string(keys[i]) < string(keys[j])
	})
	sum := sha256.Sum256(append(keys[0], keys[1]...))
	return fmt.Sprintf("%02x%02x-%02x%02x-%02x%02x-%02x%02x",
		sum[0], sum[1], sum[2], sum[3], sum[4], sum[5], sum[6], sum[7])
}
