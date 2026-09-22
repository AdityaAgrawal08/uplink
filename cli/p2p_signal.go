package main

import (
	"encoding/json"
	"errors"
	"fmt"
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

func (c *signalClient) headers() map[string]string {
	return map[string]string{"X-Uplink-Username": c.me}
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
	s := strings.ToLower(err.Error())
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
	code, body, err := postJSON(c.endpoint("/kick"), map[string]any{"target": target}, c.headers())
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
	code, body, err := postJSON(c.endpoint("/admin"), map[string]any{"target": target, "admin": admin}, c.headers())
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
	code, body, err := postJSON(c.endpoint("/leave"), map[string]any{}, c.headers())
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
	code, body, err := postJSON(c.endpoint("/heartbeat"), payload, c.headers())
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
		map[string]any{"to": to, "type": sigType, "payload": payload}, c.headers())
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
	code, body, err := getJSON(c.endpoint("/signal"), c.headers())
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
		map[string]any{"to": to, "msgId": msgId, "kind": kind, "payload": payload}, c.headers())
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
	code, body, err := getJSON(c.endpoint("/inbox"), c.headers())
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
		map[string]any{"ids": ids}, c.headers())
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
