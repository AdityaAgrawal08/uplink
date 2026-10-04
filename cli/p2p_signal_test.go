package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSignalServer is a minimal in-memory implementation of the signaling
// plane contract (create/join/leave/heartbeat/signal/inbox/ack) plus the
// groups plane (named group creation, invites, invites/mine, accept with
// password-first semantics and max-allowance consumption, decline). It
// mirrors the Next.js semantics closely enough to prove the CLI client
// against.
type fakeSignalServer struct {
	mu        sync.Mutex
	members   map[string]map[string]rosterMember // code -> username -> member
	signals   map[string][]signalNote            // code/username -> notes
	boxes     map[string]map[string]inboxBox     // code/username -> msgId -> box
	epochs    map[string]int64                   // code -> roster generation (join/leave bumps)
	reactions map[string]map[string]string       // code -> "msgId|emoji|user" -> "1"
	// Groups plane: named sessions with a cap + optional password, and the
	// per-user pending invite lists (newest first).
	groupMeta map[string]fakeGroupMeta // code -> name/desc/max/password
	invites   map[string][]groupInvite // username -> pending invites
	groupSeq  int                      // distinct 6-digit codes for groups
	// inviteConflict force-409s invites for a user, simulating the real
	// race where the target joins the group by code between the creator's
	// create and invite calls (test-only hook).
	inviteConflict map[string]bool
	// Request-signature plane (mirrors the real server): sigkeys is the
	// immutable per-username device-key claim used by invite-scoped calls
	// (invites/mine, decline) where the caller has no roster entry yet.
	// nonces makes each (user, timestamp|nonce) usable exactly once.
	sigkeys map[string]string
	nonces  map[string]bool
}

// fakeGroupMeta is the fake's group metadata (max < 0 = unlimited).
type fakeGroupMeta struct {
	name, desc, pass string
	max              int
}

func newFakeSignalServer() *fakeSignalServer {
	return &fakeSignalServer{
		members:        map[string]map[string]rosterMember{},
		signals:        map[string][]signalNote{},
		boxes:          map[string]map[string]inboxBox{},
		epochs:         map[string]int64{},
		reactions:      map[string]map[string]string{},
		groupMeta:      map[string]fakeGroupMeta{},
		invites:        map[string][]groupInvite{},
		inviteConflict: map[string]bool{},
		sigkeys:        map[string]string{},
		nonces:         map[string]bool{},
	}
}

// --- request-signature gate (mirrors the real server) ----------------------

// requireSig rejects the request unless its signature verifies against the
// anchor pubkey (the roster entry for the claimed username, the body pubkey
// on accept, or the per-user sigkey claim on invite-scoped calls). The
// signed payload is METHOD|PATH|TIMESTAMP|NONCE (path WITHOUT query), the
// window is 30s, and each (timestamp, nonce) pair is single-use.
func (f *fakeSignalServer) requireSig(w http.ResponseWriter, r *http.Request, anchorPubkey string) bool {
	me := f.me(r)
	reject := func() bool {
		f.write(w, 401, map[string]string{"error": "Invalid or missing request signature"})
		return false
	}
	ts := r.Header.Get("X-Uplink-Timestamp")
	nonce := r.Header.Get("X-Uplink-Nonce")
	sigB64 := r.Header.Get("X-Uplink-Sig")
	if anchorPubkey == "" || ts == "" || nonce == "" || sigB64 == "" {
		return reject()
	}
	tsv, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || tsv <= 0 || nowMs()-tsv > requestSigWindowMs || tsv-nowMs() > requestSigWindowMs {
		return reject()
	}
	replayKey := "sig:nonce:" + me + ":" + sha256Hex(ts+"|"+nonce)
	if f.nonces[replayKey] {
		return reject()
	}
	f.nonces[replayKey] = true
	u, err := base64.StdEncoding.DecodeString(anchorPubkey)
	if err != nil || len(u) != 32 {
		return reject()
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != 64 {
		return reject()
	}
	msg := signatureMsg(r.Method, r.URL.Path, ts, nonce)
	if !verifyRequestSigAgainstX25519(u, msg, sig) {
		return reject()
	}
	return true
}

// sigAnchorForUser resolves the verification anchor for invite-scoped calls:
// the immutable per-username claim if one exists; otherwise the caller's
// X-Uplink-Pubkey header (self-claim on FIRST use — the device key binds the
// username from then on, exactly like a roster claim).
func (f *fakeSignalServer) sigAnchorForUser(w http.ResponseWriter, r *http.Request, me string) (string, bool) {
	if anchor := f.sigkeys[me]; anchor != "" {
		return anchor, f.requireSig(w, r, anchor)
	}
	anchor := strings.TrimSpace(r.Header.Get("X-Uplink-Pubkey"))
	if !f.requireSig(w, r, anchor) {
		return "", false
	}
	f.sigkeys[me] = anchor // first claim wins (immutable)
	return anchor, true
}

// sha256Hex is the replay-key digest (imports kept local to this file's
// existing dependency set; crypto/sha256 is already available).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func nowMs() int64 { return time.Now().UnixMilli() }

// fakeInviteCount reports pending invites for a user (test accessor).
func (f *fakeSignalServer) fakeInviteCount(user string) int {
	return len(f.invites[user])
}

// fakeHasInvite reports whether user holds a pending invite for code.
func (f *fakeSignalServer) fakeHasInvite(user, code string) bool {
	for _, inv := range f.invites[user] {
		if inv.Code == code {
			return true
		}
	}
	return false
}

// removeInvite drops one pending invite (accept/decline/consumption).
func (f *fakeSignalServer) removeInvite(user, code string) {
	kept := f.invites[user][:0]
	for _, inv := range f.invites[user] {
		if inv.Code != code {
			kept = append(kept, inv)
		}
	}
	f.invites[user] = kept
}

// fakeGroup returns a group's meta (test accessor).
func (f *fakeSignalServer) fakeGroup(code string) (fakeGroupMeta, bool) {
	m, ok := f.groupMeta[code]
	return m, ok
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

	// User-scoped invite inbox (outside the /session/ tree).
	if p == "/api/v1/invites/mine" && r.Method == "GET" {
		me := f.me(r)
		if _, ok := f.sigAnchorForUser(w, r, me); !ok {
			return
		}
		out := f.invites[me]
		if out == nil {
			out = []groupInvite{}
		}
		f.write(w, 200, map[string]any{"invites": out})
		return
	}

	if p == "/api/v1/session/create" && r.Method == "POST" {
		var body struct {
			Username   string `json:"username"`
			Pubkey     string `json:"pubkey"`
			Password   string `json:"password,omitempty"`
			GroupName  string `json:"groupName"`
			GroupDesc  string `json:"groupDesc"`
			MaxMembers *int   `json:"maxMembers"`
			ParentCode string `json:"parentCode"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Username) < 1 || body.Pubkey == "" {
			f.write(w, 400, map[string]string{"error": "bad input"})
			return
		}
		if body.GroupName != "" || body.ParentCode != "" {
			// Group creation: validate per the server contract, then mint
			// a fresh distinct code (the shared home room stays 123456).
			if n := len([]rune(body.GroupName)); n < 1 || n > 64 {
				f.write(w, 400, map[string]string{"error": "groupName must be a string of 1-64 characters"})
				return
			}
			if len([]rune(body.GroupDesc)) > 256 {
				f.write(w, 400, map[string]string{"error": "groupDesc must be a string of at most 256 characters"})
				return
			}
			if body.MaxMembers != nil && (*body.MaxMembers < 2 || *body.MaxMembers > 1000000) {
				f.write(w, 400, map[string]string{"error": "maxMembers must be an integer of at least 2, or null for unlimited"})
				return
			}
			f.groupSeq++
			code := fmt.Sprintf("%06d", 700000+f.groupSeq)
			if f.members[code] == nil {
				f.members[code] = map[string]rosterMember{}
			}
			max := -1
			if body.MaxMembers != nil {
				max = *body.MaxMembers
			}
			f.groupMeta[code] = fakeGroupMeta{name: body.GroupName, desc: body.GroupDesc, pass: body.Password, max: max}
			// The creator is crowned automatically (server role).
			f.members[code][body.Username] = rosterMember{Username: body.Username, Pubkey: body.Pubkey, Online: true, Role: "creator"}
			f.write(w, 201, map[string]string{"sessionId": code})
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
		// Join/accept need the room to actually exist (plain 404). Every
		// other route is signature-gated FIRST, exactly like the real
		// server: a ghost room yields no roster anchor for the claimed
		// username, so the gate answers 401 before any room lookup.
		if sub+"|"+r.Method == "join|POST" || sub+"|"+r.Method == "invites/accept|POST" {
			f.write(w, 404, map[string]string{"error": "Session not found"})
			return
		}
		members = map[string]rosterMember{}
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
		// Plain /join enforces the group cap with the exact server message.
		if meta, ok := f.groupMeta[code]; ok && meta.max >= 0 && len(members) >= meta.max {
			f.write(w, 403, map[string]string{"error": "Maximum allowance is reached"})
			return
		}
		role := "member"
		if gm, ok := f.groupMeta[code]; ok && len(members) == 0 {
			_ = gm
			role = "creator"
		}
		members[body.Username] = rosterMember{Username: body.Username, Pubkey: body.Pubkey, Online: true, Role: role}
		f.epochs[code]++
		f.write(w, 200, map[string]any{"sessionId": code, "participants": []string{body.Username}, "roster": roster(), "epoch": f.epochs[code]})
	case "invites|POST":
		me := f.me(r)
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		var body struct {
			Username string `json:"username"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Username == me {
			f.write(w, 400, map[string]string{"error": "You cannot invite yourself"})
			return
		}
		if f.inviteConflict[body.Username] {
			f.write(w, 409, map[string]string{"error": "User is already in this session"})
			return
		}
		if _, ok := members[body.Username]; ok {
			f.write(w, 409, map[string]string{"error": "User is already in this session"})
			return
		}
		meta := f.groupMeta[code]
		// The REAL server emits at as epoch-MILLIS (a JSON number) — the fake
		// mirrors the contract so decode stays honest (a string here once hid
		// the client's number-into-string bug).
		inv := groupInvite{Code: code, GroupName: meta.name, GroupDesc: meta.desc, By: me, At: time.Now().UnixMilli()}
		// Replace a stale invite for the same code (re-invite refreshes).
		kept := f.invites[body.Username][:0]
		for _, old := range f.invites[body.Username] {
			if old.Code != code {
				kept = append(kept, old)
			}
		}
		f.invites[body.Username] = append([]groupInvite{inv}, kept...)
		f.write(w, 201, map[string]bool{"ok": true})
	case "invites/accept|POST":
		me := f.me(r)
		var body struct {
			Code     string `json:"code"`
			Pubkey   string `json:"pubkey"`
			Password string `json:"password,omitempty"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		// Signature FIRST, anchored to the pubkey carried in the body: the
		// seat is bound to the device key that proved possession, not just
		// to the claimed name.
		if !f.requireSig(w, r, body.Pubkey) {
			return
		}
		if f.sigkeys[me] == "" {
			f.sigkeys[me] = body.Pubkey // first-use claim (immutable)
		}
		// Password FIRST: required/incorrect answer 401 before anything
		// else (the contract's ordering, so the modal opens correctly).
		if meta, ok := f.groupMeta[code]; ok && meta.pass != "" && body.Password != meta.pass {
			if body.Password == "" {
				f.write(w, 401, map[string]string{"error": "Password is required for this session"})
				return
			}
			f.write(w, 401, map[string]string{"error": "Incorrect session password"})
			return
		}
		if !f.fakeHasInvite(me, code) {
			f.write(w, 404, map[string]string{"error": "Invite not found"})
			return
		}
		sess, exists := f.members[code]
		if !exists {
			f.removeInvite(me, code)
			f.write(w, 404, map[string]string{"error": "Invite not found"})
			return
		}
		// Full: the 403 is TERMINAL — the invite is consumed, re-accept 404s.
		if meta, ok := f.groupMeta[code]; ok && meta.max >= 0 && len(sess) >= meta.max {
			f.removeInvite(me, code)
			f.write(w, 403, map[string]string{"error": "Maximum allowance is reached"})
			return
		}
		if _, ok := sess[me]; ok {
			f.removeInvite(me, code)
			f.write(w, 409, map[string]string{"error": "User is already in this session"})
			return
		}
		sess[me] = rosterMember{Username: me, Pubkey: body.Pubkey, Online: true, Role: "member"}
		f.epochs[code]++
		f.removeInvite(me, code)
		f.write(w, 200, map[string]any{"sessionId": code, "participants": []string{me}, "roster": roster(), "epoch": f.epochs[code]})
	case "invites/decline|POST":
		me := f.me(r)
		if _, ok := f.sigAnchorForUser(w, r, me); !ok {
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !f.fakeHasInvite(me, body.Code) {
			f.write(w, 404, map[string]string{"error": "Invite not found"})
			return
		}
		f.removeInvite(me, body.Code)
		f.write(w, 200, map[string]bool{"ok": true})
	case "leave|POST":
		me := f.me(r)
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
		if _, ok := members[me]; !ok {
			f.write(w, 403, map[string]string{"error": "Not in this session"})
			return
		}
		f.write(w, 200, map[string]any{"ok": true, "activeUsers": []string{me}, "roster": roster(), "epoch": f.epochs[code]})
	case "signal|POST":
		me := f.me(r)
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, actor.Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, actor.Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
		if !f.requireSig(w, r, members[me].Pubkey) {
			return
		}
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
			counts  map[string]int
			mine    map[string]bool
			byEmoji map[string][]string
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
				a = &reactAgg{counts: map[string]int{}, mine: map[string]bool{}, byEmoji: map[string][]string{}}
				byMsg[mid] = a
			}
			a.counts[emoji]++
			a.byEmoji[emoji] = append(a.byEmoji[emoji], user)
			if user == me {
				a.mine[emoji] = true
			}
		}
		out := []map[string]any{}
		for mid, a := range byMsg {
			mine := []string{}
			details := []map[string]any{}
			for _, e := range reactionEmojis {
				if a.mine[e] {
					mine = append(mine, e)
				}
				if names := a.byEmoji[e]; len(names) > 0 {
					sort.Strings(names)
					details = append(details, map[string]any{"emoji": e, "usernames": names})
				}
			}
			out = append(out, map[string]any{"msgId": mid, "counts": a.counts, "mine": mine, "details": details})
		}
		f.write(w, 200, map[string]any{"reactions": out})
	default:
		f.write(w, 404, map[string]string{"error": "unknown"})
	}
}

func newSignalTestClient(t *testing.T, ts *httptest.Server, me string) *signalClient {
	t.Helper()
	return &signalClient{serverURL: ts.URL, me: me, id: mustTestIdentity(t)}
}

func TestSignalFullFlow(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := newSignalTestClient(t, srv, "alice")
	sid, err := alice.createRoom("alice", pubkeyB64(alice.id), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sid != "123456" {
		t.Fatalf("unexpected sid %q", sid)
	}

	bob := newSignalTestClient(t, srv, "bob")
	bob.key = sid
	roster, epoch, err := bob.joinRoom("bob", pubkeyB64(bob.id), "")
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
	eve := newSignalTestClient(t, srv, "eve")
	eve.key = sid
	if _, _, err := eve.inboxFetch(); err == nil {
		t.Fatal("expected 401 for non-member (no roster key anchor)")
	} else if apiStatusCode(err) != 401 {
		t.Fatalf("non-member inboxFetch status = %d; want 401", apiStatusCode(err))
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
