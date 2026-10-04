package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ─── Signaling client (Vercel-native architecture) ──────────────────────────
//
// Thin HTTP wrapper over the signaling plane: room create/join/leave,
// heartbeats, WebRTC rendezvous notes, and the offline/fallback inbox.
// All payloads are opaque to the server (E2E ciphertext or SDP blobs);
// this client never interprets them.

type rosterMember struct {
	Username string   `json:"username"`
	Pubkey   string   `json:"pubkey"`
	Online   bool     `json:"online"`
	Role     string   `json:"role,omitempty"` // creator|admin|member (empty = pre-roles server)
	PeerId   string   `json:"peerId,omitempty"`
	Addrs    []string `json:"addrs,omitempty"`
}

type signalNote struct {
	From    string `json:"from"`
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Ts      int64  `json:"ts"`
}

type inboxBox struct {
	MsgId   string `json:"msgId"`
	From    string `json:"from"`
	Kind    string `json:"kind"`
	Payload string `json:"payload"`
	Ts      int64  `json:"ts"`
}

type signalClient struct {
	serverURL string
	key       string // session code; empty until create/join returns it
	me        string
	id        *identityKey // device identity; signs every identity-bearing call
}

func (c *signalClient) endpoint(path string) string {
	base := c.serverURL
	if len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if c.key == "" {
		return base + "/api/v1/session" + path
	}
	return base + "/api/v1/session/" + url.PathEscape(c.key) + path
}

// apiError extracts a server error message or falls back to status text.
// Typed as *apiStatusError so callers can branch on status codes without
// substring-matching formatted strings.
func apiErr(code int, body []byte) error {
	var r struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err == nil && r.Error != "" {
		return &apiStatusError{Code: code, Msg: r.Error}
	}
	return &apiStatusError{Code: code, Msg: truncateStringPlain(string(body), 120)}
}

// apiStatusError carries an HTTP status through the error chain.
type apiStatusError struct {
	Code int
	Msg  string
}

func (e *apiStatusError) Error() string { return fmt.Sprintf("status %d: %s", e.Code, e.Msg) }

// apiStatusCode unwraps the HTTP status (0 when unknown/transient).
func apiStatusCode(err error) int {
	var se *apiStatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// serverDownMsg is the single alert shown whenever the signaling server is
// unreachable: every user action that needs the server surfaces exactly
// this, and the roster tick holds it until heartbeats succeed again.
const serverDownMsg = "services are down, try again later"

// isServerDown classifies errors that mean the server is gone (crashed,
// stopped, or network-dead) as opposed to application rejections (4xx,
// rate limits). Transport errors carry status 0; only gateway 502/503/504
// count as down among real HTTP statuses.
func isServerDown(err error) bool {
	if err == nil {
		return false
	}
	if code := apiStatusCode(err); code != 0 {
		return code == 502 || code == 503 || code == 504
	}
	// Prefer typed net errors over substring matching (user content may
	// contain tokens like "ice"/"timeout"). Fall back to substrings only
	// for opaque transport errors.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return true
		}
		// Refused/reset/unreachable => server/network down; ICE/DTLS
		// handshake failures are local connectivity, not server-down.
		s := strings.ToLower(opErr.Err.Error())
		for _, sub := range []string{"connection refused", "connection reset", "no such host", "network is unreachable", "connection aborted"} {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	// Mesh/WebRTC path failures (ICE, offer/answer, DTLS, STUN) are local
	// connectivity problems, never proof the server is down — a symmetric
	// NAT must not raise the server banner.
	for _, sub := range []string{
		"ice", "offer", "answer", "dtls", "stun", "turn", "webrtc",
		"safety code", "handshake",
	} {
		if strings.Contains(s, sub) {
			return false
		}
	}
	for _, sub := range []string{
		"connection refused",
		"connection reset",
		"no such host",
		"network is unreachable",
		"connection aborted",
		"timeout",
		"deadline exceeded",
		"unexpected eof",
	} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// createRoom mints a session code. password empty = open room.
func (c *signalClient) createRoom(username, pubkey, password string) (string, error) {
	payload := map[string]any{"username": username, "pubkey": pubkey}
	if password != "" {
		payload["password"] = password
	}
	code, body, err := postJSON(c.endpoint("/create"), payload, nil)
	if err != nil {
		return "", err
	}
	if code != 201 {
		return "", apiErr(code, body)
	}
	var r struct {
		SessionId string `json:"sessionId"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	if r.SessionId == "" {
		return "", fmt.Errorf("server returned empty sessionId")
	}
	c.key = r.SessionId
	return r.SessionId, nil
}

// joinRoom claims a username and returns the roster (with peer pubkeys)
// plus the roster epoch (bumped by this join) for the change tracker.
func (c *signalClient) joinRoom(username, pubkey, password string) ([]rosterMember, int64, error) {
	payload := map[string]any{"username": username, "pubkey": pubkey}
	if password != "" {
		payload["password"] = password
	}
	code, body, err := postJSON(c.endpoint("/join"), payload, nil)
	if err != nil {
		return nil, 0, err
	}
	if code != 200 {
		return nil, 0, apiErr(code, body)
	}
	var r struct {
		Roster []rosterMember `json:"roster"`
		Epoch  int64          `json:"epoch"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, 0, err
	}
	return r.Roster, r.Epoch, nil
}

// kickUser removes a user from the room (creator/admin only, server
// enforced). Returns the fresh roster + epoch so the caller refreshes
// without waiting for the next beat.
func (c *signalClient) kickUser(target string) ([]rosterMember, int64, error) {
	code, body, err := postJSON(c.endpoint("/kick"), map[string]any{"target": target}, c.sigHeaders("POST", c.endpoint("/kick")))
	if err != nil {
		return nil, 0, err
	}
	if code != 200 {
		return nil, 0, apiErr(code, body)
	}
	var r struct {
		Roster []rosterMember `json:"roster"`
		Epoch  int64          `json:"epoch"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, 0, err
	}
	return r.Roster, r.Epoch, nil
}

// setRole grants (admin=true) or revokes (admin=false) the admin role
// (room creator only, server enforced). Returns the fresh roster + epoch.
func (c *signalClient) setRole(target string, admin bool) ([]rosterMember, int64, error) {
	code, body, err := postJSON(c.endpoint("/admin"), map[string]any{"target": target, "admin": admin}, c.sigHeaders("POST", c.endpoint("/admin")))
	if err != nil {
		return nil, 0, err
	}
	if code != 200 {
		return nil, 0, apiErr(code, body)
	}
	var r struct {
		Roster []rosterMember `json:"roster"`
		Epoch  int64          `json:"epoch"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, 0, err
	}
	return r.Roster, r.Epoch, nil
}

func (c *signalClient) leaveRoom() error {
	code, body, err := postJSON(c.endpoint("/leave"), map[string]any{}, c.sigHeaders("POST", c.endpoint("/leave")))
	if err != nil {
		return err
	}
	if code != 200 {
		return apiErr(code, body)
	}
	return nil
}

// heartbeat pings presence and returns the fresh roster plus the roster
// epoch. Absent epoch (older servers) parses as 0 — change tracking simply
// stays quiet.
func (c *signalClient) heartbeat(peerId string, addrs []string) ([]rosterMember, int64, error) {
	payload := map[string]any{}
	if peerId != "" {
		payload["peerId"] = peerId
	}
	if addrs != nil {
		payload["addrs"] = addrs
	}
	code, body, err := postJSON(c.endpoint("/heartbeat"), payload, c.sigHeaders("POST", c.endpoint("/heartbeat")))
	if err != nil {
		return nil, 0, err
	}
	if code != 200 {
		return nil, 0, apiErr(code, body)
	}
	var r struct {
		Roster []rosterMember `json:"roster"`
		Epoch  int64          `json:"epoch"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, 0, err
	}
	return r.Roster, r.Epoch, nil
}

// signalSend deposits one rendezvous note (SDP offer/answer, ICE).
func (c *signalClient) signalSend(to, sigType, payload string) error {
	code, body, err := postJSON(c.endpoint("/signal"),
		map[string]any{"to": to, "type": sigType, "payload": payload}, c.sigHeaders("POST", c.endpoint("/signal")))
	if err != nil {
		return err
	}
	if code != 201 {
		return apiErr(code, body)
	}
	return nil
}

// signalPoll drains my rendezvous queue.
func (c *signalClient) signalPoll() ([]signalNote, error) {
	code, body, err := getJSON(c.endpoint("/signal"), c.sigHeaders("GET", c.endpoint("/signal")))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, apiErr(code, body)
	}
	var r struct {
		Notes []signalNote `json:"notes"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Notes == nil {
		r.Notes = []signalNote{}
	}
	return r.Notes, nil
}

// inboxSend deposits one ciphertext box for a peer (offline delivery or
// P2P-fallback relay). Idempotent on msgId — safe to retry.
func (c *signalClient) inboxSend(to, msgId, kind, payload string) error {
	code, body, err := postJSON(c.endpoint("/inbox"),
		map[string]any{"to": to, "msgId": msgId, "kind": kind, "payload": payload}, c.sigHeaders("POST", c.endpoint("/inbox")))
	if err != nil {
		return err
	}
	if code != 201 {
		return apiErr(code, body)
	}
	return nil
}

// inboxFetch returns my boxes WITHOUT deleting (deletion is explicit ACK),
// plus the roster epoch piggybacked on the response.
func (c *signalClient) inboxFetch() ([]inboxBox, int64, error) {
	code, body, err := getJSON(c.endpoint("/inbox"), c.sigHeaders("GET", c.endpoint("/inbox")))
	if err != nil {
		return nil, 0, err
	}
	if code != 200 {
		return nil, 0, apiErr(code, body)
	}
	var r struct {
		Boxes []inboxBox `json:"boxes"`
		Epoch int64      `json:"epoch"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, 0, err
	}
	if r.Boxes == nil {
		r.Boxes = []inboxBox{}
	}
	return r.Boxes, r.Epoch, nil
}

// inboxAck deletes exactly the acknowledged boxes.
func (c *signalClient) inboxAck(ids []string) (int, error) {
	code, body, err := postJSON(c.endpoint("/inbox/ack"),
		map[string]any{"ids": ids}, c.sigHeaders("POST", c.endpoint("/inbox/ack")))
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, apiErr(code, body)
	}
	var r struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, err
	}
	return r.Removed, nil
}

// reactionDetail breaks one emoji's tally down per reactor: every username
// that currently picked it. Servers predating the breakdown omit the field,
// in which case the client falls back to counts+mine.
type reactionDetail struct {
	Emoji     string   `json:"emoji"`
	Usernames []string `json:"usernames"`
}

// reactionSummary is one message's aggregated reactions: counts per allowlisted
// emoji plus the requesting user's own picks (at most one by server rule).
// Details is the optional per-reactor breakdown; nil means unknown.
type reactionSummary struct {
	MsgId   string           `json:"msgId"`
	Counts  map[string]int   `json:"counts"`
	Mine    []string         `json:"mine"`
	Details []reactionDetail `json:"details"`
}

// UnmarshalJSON accepts the canonical `details` breakdown plus tolerant
// aliases (`breakdown`, `reactors`; `users` for `usernames`) so a field-name
// drift on the server side degrades to counts+mine instead of dropping the
// reactor list. The count/mine payload is always parsed verbatim.
func (s *reactionSummary) UnmarshalJSON(data []byte) error {
	var raw struct {
		MsgId     string          `json:"msgId"`
		Counts    map[string]int  `json:"counts"`
		Mine      []string        `json:"mine"`
		Details   json.RawMessage `json:"details"`
		Breakdown json.RawMessage `json:"breakdown"`
		Reactors  json.RawMessage `json:"reactors"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.MsgId, s.Counts, s.Mine = raw.MsgId, raw.Counts, raw.Mine
	for _, blob := range []json.RawMessage{raw.Details, raw.Breakdown, raw.Reactors} {
		if len(blob) == 0 {
			continue
		}
		if details := parseReactionDetails(blob); len(details) > 0 {
			s.Details = details
			break
		}
	}
	return nil
}

// parseReactionDetails accepts both shapes the breakdown could plausibly take:
// an array of {emoji, usernames} objects and a flat {emoji: [usernames]} map.
// Malformed input yields nil (counts+mine stay usable).
func parseReactionDetails(blob json.RawMessage) []reactionDetail {
	var arr []struct {
		Emoji     string   `json:"emoji"`
		Usernames []string `json:"usernames"`
		Users     []string `json:"users"`
	}
	if err := json.Unmarshal(blob, &arr); err == nil && len(arr) > 0 {
		out := make([]reactionDetail, 0, len(arr))
		for _, d := range arr {
			names := d.Usernames
			if len(names) == 0 {
				names = d.Users
			}
			if d.Emoji == "" || len(names) == 0 {
				continue
			}
			out = append(out, reactionDetail{Emoji: d.Emoji, Usernames: names})
		}
		if len(out) > 0 {
			return out
		}
	}
	var flat map[string][]string
	if err := json.Unmarshal(blob, &flat); err == nil && len(flat) > 0 {
		out := make([]reactionDetail, 0, len(flat))
		for emoji, names := range flat {
			if emoji != "" && len(names) > 0 {
				out = append(out, reactionDetail{Emoji: emoji, Usernames: names})
			}
		}
		return out
	}
	return nil
}

// react toggles one reaction on the server (authoritative): the same emoji
// removes the caller's reaction, a different one replaces it. The server
// returns the resulting state; callers rely on the next poll for truth.
func (c *signalClient) react(msgId, emoji string) error {
	code, body, err := postJSON(c.endpoint("/reactions"),
		map[string]any{"msgId": msgId, "emoji": emoji}, c.sigHeaders("POST", c.endpoint("/reactions")))
	if err != nil {
		return err
	}
	if code != 200 {
		return apiErr(code, body)
	}
	return nil
}

// reactions fetches aggregated counts. msgIds narrows the hash scan to the
// messages a caller actually renders (server caps the list at 50); nil asks
// for the whole-room aggregate.
func (c *signalClient) reactions(msgIds []string) ([]reactionSummary, error) {
	path := "/reactions"
	if len(msgIds) > 0 {
		path += "?msgIds=" + url.QueryEscape(strings.Join(msgIds, ","))
	}
	code, body, err := getJSON(c.endpoint(path), c.sigHeaders("GET", c.endpoint(path)))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, apiErr(code, body)
	}
	var r struct {
		Reactions []reactionSummary `json:"reactions"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Reactions == nil {
		r.Reactions = []reactionSummary{}
	}
	return r.Reactions, nil
}
