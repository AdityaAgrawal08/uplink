package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSignalServer is a minimal in-memory implementation of the signaling
// plane contract (create/join/leave/heartbeat/signal/inbox/ack). It mirrors
// the Next.js semantics closely enough to prove the CLI client against.
type fakeSignalServer struct {
	mu      sync.Mutex
	members map[string]map[string]rosterMember // code -> username -> member
	signals map[string][]signalNote            // code/username -> notes
	boxes   map[string]map[string]inboxBox     // code/username -> msgId -> box
}

func newFakeSignalServer() *fakeSignalServer {
	return &fakeSignalServer{
		members: map[string]map[string]rosterMember{},
		signals: map[string][]signalNote{},
		boxes:   map[string]map[string]inboxBox{},
	}
}

func (f *fakeSignalServer) me(r *http.Request) string {
	return r.Header.Get("X-Uplink-Username")
}

func (f *fakeSignalServer) write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeSignalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path

	if p == "/api/v1/session/create" && r.Method == "POST" {
		var body struct {
			Username string `json:"username"`
			Pubkey   string `json:"pubkey"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Username) < 3 || body.Pubkey == "" {
			f.write(w, 400, map[string]string{"error": "bad input"})
			return
		}
		code := "123456"
		if f.members[code] == nil {
			f.members[code] = map[string]rosterMember{}
		}
		if _, taken := f.members[code][body.Username]; taken {
			f.write(w, 409, map[string]string{"error": "taken"})
			return
		}
		f.members[code][body.Username] = rosterMember{Username: body.Username, Pubkey: body.Pubkey, Online: true}
		f.write(w, 201, map[string]string{"sessionId": code})
		return
	}

	rest, ok := strings.CutPrefix(p, "/api/v1/session/")
	if !ok {
		f.write(w, 404, map[string]string{"error": "nope"})
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	code, sub := parts[0], ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	members, roomExists := f.members[code]
	if !roomExists {
		f.write(w, 404, map[string]string{"error": "Session not found"})
		return
	}
	roster := func() []rosterMember {
		out := []rosterMember{}
		for _, m := range members {
			out = append(out, m)
		}
		return out
	}

	switch sub + "|" + r.Method {
	case "join|POST":
		var body struct {
			Username string `json:"username"`
			Pubkey   string `json:"pubkey"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if _, taken := members[body.Username]; taken {
			f.write(w, 409, map[string]string{"error": "Username already taken"})
			return
		}
		members[body.Username] = rosterMember{Username: body.Username, Pubkey: body.Pubkey, Online: true}
		f.write(w, 200, map[string]any{"sessionId": code, "participants": []string{body.Username}, "roster": roster()})
	case "leave|POST":
		me := f.me(r)
		delete(members, me)
		remaining := len(members)
		ended := remaining == 0
		if ended {
			delete(f.members, code)
		}
		f.write(w, 200, map[string]any{"ok": true, "remaining": remaining, "ended": ended})
	case "heartbeat|POST":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		f.write(w, 200, map[string]any{"ok": true, "activeUsers": []string{me}, "roster": roster()})
	case "signal|POST":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		var body struct {
			To, Type, Payload string
		}
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := members[body.To]; !ok {
			f.write(w, 404, map[string]string{"error": "Recipient is not in this session"})
			return
		}
		k := code + "/" + body.To
		f.signals[k] = append(f.signals[k], signalNote{From: me, Type: body.Type, Payload: body.Payload})
		f.write(w, 201, map[string]bool{"ok": true})
	case "signal|GET":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		k := code + "/" + me
		notes := f.signals[k]
		if notes == nil {
			notes = []signalNote{}
		}
		delete(f.signals, k)
		f.write(w, 200, map[string]any{"notes": notes})
	case "inbox|POST":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		var body struct {
			To, MsgId, Kind, Payload string
		}
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := members[body.To]; !ok {
			f.write(w, 404, map[string]string{"error": "Recipient is not in this session"})
			return
		}
		k := code + "/" + body.To
		if f.boxes[k] == nil {
			f.boxes[k] = map[string]inboxBox{}
		}
		f.boxes[k][body.MsgId] = inboxBox{MsgId: body.MsgId, From: me, Kind: body.Kind, Payload: body.Payload}
		f.write(w, 201, map[string]bool{"ok": true})
	case "inbox|GET":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		out := []inboxBox{}
		for _, b := range f.boxes[code+"/"+me] {
			out = append(out, b)
		}
		f.write(w, 200, map[string]any{"boxes": out})
	case "inbox/ack|POST":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		var body struct {
			Ids []string `json:"ids"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		removed := 0
		for _, id := range body.Ids {
			if _, ok := f.boxes[code+"/"+me][id]; ok {
				delete(f.boxes[code+"/"+me], id)
				removed++
			}
		}
		f.write(w, 200, map[string]any{"ok": true, "removed": removed})
	default:
		f.write(w, 404, map[string]string{"error": "unknown"})
	}
}

func newSignalTestClient(ts *httptest.Server, me string) *signalClient {
	return &signalClient{serverURL: ts.URL, me: me}
}

func TestSignalFullFlow(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := newSignalTestClient(srv, "alice")
	sid, err := alice.createRoom("alice", "pubkey-alice", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sid != "123456" {
		t.Fatalf("unexpected sid %q", sid)
	}

	bob := newSignalTestClient(srv, "bob")
	bob.key = sid
	roster, err := bob.joinRoom("bob", "pubkey-bob", "")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if len(roster) != 2 {
		t.Fatalf("expected 2 members, got %d", len(roster))
	}
	for _, m := range roster {
		if m.Pubkey == "" {
			t.Fatalf("roster member %s missing pubkey", m.Username)
		}
	}

	// duplicate claim
	if _, err := bob.joinRoom("alice", "pubkey-x", ""); err == nil {
		t.Fatal("expected 409 on duplicate username")
	}

	// heartbeat
	if _, err := bob.heartbeat("peer1", []string{"/ip4/1.2.3.4/tcp/1"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// signaling roundtrip
	if err := alice.signalSend("bob", "offer", "SDP"); err != nil {
		t.Fatalf("signalSend: %v", err)
	}
	notes, err := bob.signalPoll()
	if err != nil {
		t.Fatalf("signalPoll: %v", err)
	}
	if len(notes) != 1 || notes[0].From != "alice" || notes[0].Payload != "SDP" {
		t.Fatalf("wrong notes: %+v", notes)
	}
	notes, _ = bob.signalPoll()
	if len(notes) != 0 {
		t.Fatal("queue must drain")
	}

	// inbox roundtrip with idempotent retry
	if err := alice.inboxSend("bob", "m1", "chat", "CIPH"); err != nil {
		t.Fatalf("inboxSend: %v", err)
	}
	if err := alice.inboxSend("bob", "m1", "chat", "CIPH"); err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
	boxes, err := bob.inboxFetch()
	if err != nil {
		t.Fatalf("inboxFetch: %v", err)
	}
	if len(boxes) != 1 || boxes[0].MsgId != "m1" {
		t.Fatalf("wrong boxes: %+v", boxes)
	}
	n, err := bob.inboxAck([]string{"m1"})
	if err != nil || n != 1 {
		t.Fatalf("ack: %v %d", err, n)
	}
	boxes, _ = bob.inboxFetch()
	if len(boxes) != 0 {
		t.Fatal("inbox must be empty after ack")
	}

	// guards
	eve := newSignalTestClient(srv, "eve")
	eve.key = sid
	if _, err := eve.inboxFetch(); err == nil {
		t.Fatal("expected 403 for non-member")
	}
	if err := alice.inboxSend("ghost", "m2", "chat", "x"); err == nil {
		t.Fatal("expected 404 for unknown recipient")
	}

	// leave destroys when empty
	if err := alice.leaveRoom(); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := bob.leaveRoom(); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := eve.joinRoom("eve", "pub", ""); err == nil {
		t.Fatal("expected 404 joining destroyed room")
	}
}
