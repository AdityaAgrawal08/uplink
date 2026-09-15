package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

// joinRoom claims a username and returns the roster (with peer pubkeys).
func (c *signalClient) joinRoom(username, pubkey, password string) ([]rosterMember, error) {
	payload := map[string]any{"username": username, "pubkey": pubkey}
	if password != "" {
		payload["password"] = password
	}
	code, body, err := postJSON(c.endpoint("/join"), payload, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, apiErr(code, body)
	}
	var r struct {
		Roster []rosterMember `json:"roster"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Roster, nil
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

// heartbeat pings presence and returns the fresh roster.
func (c *signalClient) heartbeat(peerId string, addrs []string) ([]rosterMember, error) {
	payload := map[string]any{}
	if peerId != "" {
		payload["peerId"] = peerId
	}
	if addrs != nil {
		payload["addrs"] = addrs
	}
	code, body, err := postJSON(c.endpoint("/heartbeat"), payload, c.headers())
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, apiErr(code, body)
	}
	var r struct {
		Roster []rosterMember `json:"roster"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Roster, nil
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

// inboxFetch returns my boxes WITHOUT deleting (deletion is explicit ACK).
func (c *signalClient) inboxFetch() ([]inboxBox, error) {
	code, body, err := getJSON(c.endpoint("/inbox"), c.headers())
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, apiErr(code, body)
	}
	var r struct {
		Boxes []inboxBox `json:"boxes"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Boxes == nil {
		r.Boxes = []inboxBox{}
	}
	return r.Boxes, nil
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
