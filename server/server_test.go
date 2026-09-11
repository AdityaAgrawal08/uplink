package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// ─── RingBuffer tests ──────────────────────────────────────────────────────

func TestRingBuffer_PushAndSnapshot(t *testing.T) {
	rb := NewRingBuffer(3)

	// Empty buffer.
	if snap := rb.Snapshot(); snap != nil {
		t.Fatalf("expected nil snapshot, got %d items", len(snap))
	}

	// Push one message.
	rb.Push(ChatMessage{MsgId: "1", Text: "hello"})
	snap := rb.Snapshot()
	if len(snap) != 1 || snap[0].MsgId != "1" {
		t.Fatalf("expected 1 message, got %v", snap)
	}

	// Push two more (fill buffer).
	rb.Push(ChatMessage{MsgId: "2", Text: "world"})
	rb.Push(ChatMessage{MsgId: "3", Text: "!"})
	snap = rb.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(snap))
	}
	if snap[0].MsgId != "1" || snap[2].MsgId != "3" {
		t.Fatalf("wrong order: %v", snap)
	}

	// Overflow: push a 4th message, oldest should be evicted.
	rb.Push(ChatMessage{MsgId: "4", Text: "overflow"})
	snap = rb.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 messages after overflow, got %d", len(snap))
	}
	if snap[0].MsgId != "2" || snap[2].MsgId != "4" {
		t.Fatalf("wrong order after overflow: %v", snap)
	}
}

func TestRingBuffer_ConcurrentPush(t *testing.T) {
	rb := NewRingBuffer(100)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rb.Push(ChatMessage{MsgId: string(rune('A' + id%26))})
		}(i)
	}
	wg.Wait()
	snap := rb.Snapshot()
	if len(snap) > 100 {
		t.Fatalf("snapshot exceeded capacity: %d", len(snap))
	}
}

// ─── JSON wire format tests ────────────────────────────────────────────────

func TestInboundJSON(t *testing.T) {
	raw := `{"type":"chat","text":"hello","to":"bob"}`
	var in Inbound
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if in.Type != "chat" || in.Text != "hello" || in.To != "bob" {
		t.Fatalf("unexpected: %+v", in)
	}
}

func TestOutboundJSON(t *testing.T) {
	out := Outbound{
		Type:      "chat",
		MsgId:     "abc123",
		Username:  "alice",
		Text:      "hello",
		Kind:      "chat",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Outbound
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.MsgId != "abc123" || decoded.Username != "alice" {
		t.Fatalf("round-trip failed: %+v", decoded)
	}
}

func TestDeleteOutboundJSON(t *testing.T) {
	out := Outbound{
		Type:        "delete",
		DeleteMsgId: "msg-456",
		Username:    "alice",
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["deleteMsgId"] != "msg-456" {
		t.Fatalf("deleteMsgId not in JSON: %v", decoded)
	}
}

// ─── Rate limiter tests ────────────────────────────────────────────────────

func TestRateLimiter_Allow(t *testing.T) {
	rl := NewRateLimiter(3) // 3 per second

	// First 3 should pass.
	for i := 0; i < 3; i++ {
		if !rl.Allow("user1") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	// 4th should be rejected.
	if rl.Allow("user1") {
		t.Fatal("4th request should be rate-limited")
	}
	// Different key should pass.
	if !rl.Allow("user2") {
		t.Fatal("different key should be allowed")
	}
}

func TestRateLimiter_WindowReset(t *testing.T) {
	rl := NewRateLimiter(2)
	rl.Allow("k")
	rl.Allow("k")
	if rl.Allow("k") {
		t.Fatal("should be rate-limited")
	}
	// Manually expire the window.
	rl.mu.Lock()
	rl.windows["k"].resetAt = time.Now().Add(-time.Second)
	rl.mu.Unlock()
	if !rl.Allow("k") {
		t.Fatal("should be allowed after window reset")
	}
}

// ─── Config tests ──────────────────────────────────────────────────────────

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Port != 8080 {
		t.Fatalf("expected port 8080, got %d", cfg.Port)
	}
	if cfg.MaxSessionUsers != 50 {
		t.Fatalf("expected 50 max users, got %d", cfg.MaxSessionUsers)
	}
	if cfg.MessageBufSize != 100 {
		t.Fatalf("expected 100 message buf, got %d", cfg.MessageBufSize)
	}
}

// ─── Helper tests ──────────────────────────────────────────────────────────

func TestIsValidUsername(t *testing.T) {
	tests := []struct{ u string; ok bool }{
		{"alice", true},
		{"bob123", true},
		{"a", false},         // too short
		{"ab", false},        // too short
		{"a]b", false},       // invalid char
		{"hello world", false}, // space
		{"", false},          // empty
	}
	for _, tt := range tests {
		if got := isValidUsername(tt.u); got != tt.ok {
			t.Errorf("isValidUsername(%q) = %v, want %v", tt.u, got, tt.ok)
		}
	}
}

func TestGenId_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := genId()
		if seen[id] {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

func TestGenNumericId(t *testing.T) {
	id := genNumericId(6)
	if len(id) != 6 {
		t.Fatalf("expected 6 digits, got %d: %s", len(id), id)
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			t.Fatalf("non-digit in ID: %c", c)
		}
	}
}
