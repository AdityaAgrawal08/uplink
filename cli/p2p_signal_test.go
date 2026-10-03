package main

import (
	"encoding/json"
	"fmt"
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
	mu        sync.Mutex
	members   map[string]map[string]rosterMember // code -> username -> member
	signals   map[string][]signalNote            // code/username -> notes
	boxes     map[string]map[string]inboxBox     // code/username -> msgId -> box
	epochs    map[string]int64                   // code -> roster generation (join/leave bumps)
	reactions map[string]map[string]string       // code -> "msgId|emoji|user" -> "1"
}

func newFakeSignalServer() *fakeSignalServer {
	return &fakeSignalServer{
		members:   map[string]map[string]rosterMember{},
		signals:   map[string][]signalNote{},
		boxes:     map[string]map[string]inboxBox{},
		epochs:    map[string]int64{},
		reactions: map[string]map[string]string{},
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
		if len(body.Username) < 1 || body.Pubkey == "" {
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
		// First member through create owns the room (mirrors server creator).
		role := "member"
		if len(f.members[code]) == 0 {
			role = "creator"
		}
		f.members[code][body.Username] = rosterMember{Username: body.Username, Pubkey: body.Pubkey, Online: true, Role: role}
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
		members[body.Username] = rosterMember{Username: body.Username, Pubkey: body.Pubkey, Online: true, Role: "member"}
		f.epochs[code]++
		f.write(w, 200, map[string]any{"sessionId": code, "participants": []string{body.Username}, "roster": roster(), "epoch": f.epochs[code]})
	case "leave|POST":
		me := f.me(r)
		if _, ok := members[me]; ok {
			f.epochs[code]++
		}
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
		f.write(w, 200, map[string]any{"ok": true, "activeUsers": []string{me}, "roster": roster(), "epoch": f.epochs[code]})
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
		f.write(w, 200, map[string]any{"boxes": out, "epoch": f.epochs[code]})
	case "kick|POST":
		me := f.me(r)
		actor, ok := members[me]
		if !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		var body struct {
			Target string `json:"target"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		tgt, ok := members[body.Target]
		if !ok {
			f.write(w, 404, map[string]string{"error": "User is not in this session"})
			return
		}
		// Matrix mirrors the real server: creator kicks anyone but self;
		// admins kick members only; the creator cannot be kicked.
		switch {
		case me == body.Target:
			f.write(w, 403, map[string]string{"error": "You cannot kick yourself"})
			return
		case tgt.Role == "creator":
			f.write(w, 403, map[string]string{"error": "Nobody can kick the room creator"})
			return
		case actor.Role == "creator":
			// full privilege
		case actor.Role == "admin" && tgt.Role == "member":
			// granted admins kick members only
		case actor.Role == "admin":
			f.write(w, 403, map[string]string{"error": "Only the room creator can kick an admin"})
			return
		default:
			f.write(w, 403, map[string]string{"error": "Only the room creator or an admin can kick users"})
			return
		}
		delete(members, body.Target)
		f.epochs[code]++
		f.write(w, 200, map[string]any{"ok": true, "roster": roster(), "epoch": f.epochs[code], "remaining": len(members)})
	case "admin|POST":
		me := f.me(r)
		actor, ok := members[me]
		if !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		if actor.Role != "creator" {
			f.write(w, 403, map[string]string{"error": "Only the room creator can grant admin"})
			return
		}
		var body struct {
			Target string `json:"target"`
			Admin  bool   `json:"admin"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if me == body.Target {
			f.write(w, 403, map[string]string{"error": "You cannot change your own role"})
			return
		}
		m, ok := members[body.Target]
		if !ok {
			f.write(w, 404, map[string]string{"error": "User is not in this session"})
			return
		}
		if m.Role == "creator" {
			f.write(w, 403, map[string]string{"error": "The creator role cannot be changed"})
			return
		}
		role := "member"
		if body.Admin {
			role = "admin"
		}
		m.Role = role
		members[body.Target] = m
		f.epochs[code]++
		f.write(w, 200, map[string]any{"ok": true, "roster": roster(), "epoch": f.epochs[code]})
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
	case "reactions|POST":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		var body struct {
			MsgId string `json:"msgId"`
			Emoji string `json:"emoji"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.MsgId == "" || !reactionEmojiAllowed(body.Emoji) {
			f.write(w, 400, map[string]string{"error": "bad reaction"})
			return
		}
		if f.reactions[code] == nil {
			f.reactions[code] = map[string]string{}
		}
		prefix := body.MsgId + "|"
		suffix := "|" + me
		same := false
		for field := range f.reactions[code] {
			if !strings.HasPrefix(field, prefix) || !strings.HasSuffix(field, suffix) {
				continue
			}
			if field == body.MsgId+"|"+body.Emoji+"|"+me {
				same = true
			}
			delete(f.reactions[code], field)
		}
		if !same {
			f.reactions[code][body.MsgId+"|"+body.Emoji+"|"+me] = "1"
		}
		f.write(w, 200, map[string]any{"msgId": body.MsgId, "emoji": body.Emoji, "reacted": !same})
	case "reactions|GET":
		me := f.me(r)
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		filter := map[string]bool{}
		if raw := r.URL.Query().Get("msgIds"); raw != "" {
			for _, id := range strings.Split(raw, ",") {
				if id != "" {
					filter[id] = true
				}
			}
		}
		type reactAgg struct {
			counts map[string]int
			mine   map[string]bool
		}
		byMsg := map[string]*reactAgg{}
		for field := range f.reactions[code] {
			i := strings.LastIndex(field, "|")
			if i <= 0 {
				continue
			}
			j := strings.LastIndex(field[:i], "|")
			if j <= 0 {
				continue
			}
			mid, emoji, user := field[:j], field[j+1:i], field[i+1:]
			if len(filter) > 0 && !filter[mid] {
				continue
			}
			a := byMsg[mid]
			if a == nil {
				a = &reactAgg{counts: map[string]int{}, mine: map[string]bool{}}
				byMsg[mid] = a
			}
			a.counts[emoji]++
			if user == me {
				a.mine[emoji] = true
			}
		}
		out := []map[string]any{}
		for mid, a := range byMsg {
			mine := []string{}
			for _, e := range reactionEmojis {
				if a.mine[e] {
					mine = append(mine, e)
				}
			}
			out = append(out, map[string]any{"msgId": mid, "counts": a.counts, "mine": mine})
		}
		f.write(w, 200, map[string]any{"reactions": out})
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
	roster, epoch, err := bob.joinRoom("bob", "pubkey-bob", "")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if len(roster) != 2 {
		t.Fatalf("expected 2 members, got %d", len(roster))
	}
	if epoch != 1 {
		t.Fatalf("join must report bumped epoch, got %d", epoch)
	}
	for _, m := range roster {
		if m.Pubkey == "" {
			t.Fatalf("roster member %s missing pubkey", m.Username)
		}
	}

	// duplicate claim
	if _, _, err := bob.joinRoom("alice", "pubkey-x", ""); err == nil {
		t.Fatal("expected 409 on duplicate username")
	}

	// heartbeat carries the roster epoch
	if _, epoch, err := bob.heartbeat("peer1", []string{"/ip4/1.2.3.4/tcp/1"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	} else if epoch != 1 {
		t.Fatalf("heartbeat epoch = %d; want 1", epoch)
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
	boxes, epoch, err := bob.inboxFetch()
	if err != nil {
		t.Fatalf("inboxFetch: %v", err)
	}
	if len(boxes) != 1 || boxes[0].MsgId != "m1" {
		t.Fatalf("wrong boxes: %+v", boxes)
	}
	if epoch != 1 {
		t.Fatalf("inbox epoch = %d; want 1", epoch)
	}
	n, err := bob.inboxAck([]string{"m1"})
	if err != nil || n != 1 {
		t.Fatalf("ack: %v %d", err, n)
	}
	boxes, _, _ = bob.inboxFetch()
	if len(boxes) != 0 {
		t.Fatal("inbox must be empty after ack")
	}

	// guards
	eve := newSignalTestClient(srv, "eve")
	eve.key = sid
	if _, _, err := eve.inboxFetch(); err == nil {
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
	if _, _, err := eve.joinRoom("eve", "pub", ""); err == nil {
		t.Fatal("expected 404 joining destroyed room")
	}
}

func TestIsServerDown(t *testing.T) {
	down := []error{
		fmt.Errorf(`Post "http://s/x": dial tcp 127.0.0.1:1: connection refused`),
		fmt.Errorf(`Post "http://s/x": read: connection reset by peer`),
		fmt.Errorf(`Post "http://s/x": no such host`),
		fmt.Errorf(`Post "http://s/x": Client.Timeout exceeded while awaiting headers`),
		fmt.Errorf(`Post "http://s/x": context deadline exceeded`),
		fmt.Errorf(`Post "http://s/x": unexpected EOF`),
		fmt.Errorf(`Post "http://s/x": dial tcp: network is unreachable`),
		apiErr(502, []byte(`{"error":"bad gateway"}`)),
		apiErr(503, []byte(`{"error":"unavailable"}`)),
		apiErr(504, []byte(`{"error":"gateway timeout"}`)),
	}
	for _, err := range down {
		if !isServerDown(err) {
			t.Errorf("must classify as down: %v", err)
		}
	}
	up := []error{
		nil,
		fmt.Errorf("empty message"),
		apiErr(400, []byte(`{"error":"bad input"}`)),
		apiErr(403, []byte(`{"error":"not in session"}`)),
		apiErr(404, []byte(`{"error":"Session not found"}`)),
		apiErr(409, []byte(`{"error":"taken"}`)),
		apiErr(429, []byte(`{"error":"slow"}`)),
	}
	for _, err := range up {
		if isServerDown(err) {
			t.Errorf("must NOT classify as down: %v", err)
		}
	}
}

func TestIsServerDownExcludesMesh(t *testing.T) {
	// Local WebRTC/mesh path failures must never raise the server banner.
	for _, s := range []string{
		"ICE gathering timed out",
		"answer wait timed out",
		"offer wait timed out",
		"KEY SWAP ALERT for alice: handshake key does not match roster",
		"offer SDP 9000 bytes exceeds signaling cap",
	} {
		if isServerDown(fmt.Errorf("%s", s)) {
			t.Errorf("mesh error must not classify as down: %q", s)
		}
	}
	if !isServerDown(fmt.Errorf("Post https://x: connection refused")) {
		t.Error("refused transport error must classify as down")
	}
}
