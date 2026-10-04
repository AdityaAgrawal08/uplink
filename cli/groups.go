package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// ─── Groups ──────────────────────────────────────────────────────────────────
//
// A group is a named session (6-digit code) created inside the current room
// (parentCode). The creator is crowned automatically by the server; other
// members join through invites. This file holds the HTTP client surface for
// the group endpoints plus the pure helpers the TUI uses to route group
// conversations through the existing chat model.

// groupInvite is one pending invite row from GET /invites/mine (newest
// first, server caps at 50). at is epoch-MILLIS — the server emits a JSON
// NUMBER, so the Go field is int64 (a string here produced the
// "cannot unmarshal number into Go struct field groupInvite.at" failure
// that surfaced as "invites unavailable" in the settings window).
type groupInvite struct {
	Code      string `json:"code"`
	GroupName string `json:"groupName"`
	GroupDesc string `json:"groupDesc"`
	By        string `json:"by"`
	At        int64  `json:"at"`
}

// groupConvPrefix namespaces group conversation buckets. Group messages are
// broadcasts (To == "") inside their own session, so the client needs a
// distinct ConvID namespace: "group:<code>". Usernames are
// [a-zA-Z0-9_]{3,20} and session codes are digits, so the prefix can never
// collide with a DM pair key ("a|b") or "general".
const groupConvPrefix = "group:"

// groupConv builds the conversation bucket for a group session code.
func groupConv(code string) string { return groupConvPrefix + code }

// groupCodeOf extracts the session code from a group conversation bucket
// ("" when the conv does not belong to a group).
func groupCodeOf(conv string) string {
	code, ok := strings.CutPrefix(conv, groupConvPrefix)
	if !ok || code == "" {
		return ""
	}
	return code
}

// groupPeerCode extracts the session code from a sidebar chatItem peer
// ("" when the peer is a DM username). Sidebar group rows carry
// peer = "group:<code>" so itemIndexFor/hover/open paths can tell a group
// row from a member row without extra state.
func groupPeerCode(peer string) string {
	return groupCodeOf(peer)
}

// groupSession is one joined group: the session's signal client + engine
// (kept running in the background so the sidebar stays fresh) plus display
// state (name, unread). Lifecycle is owned by the Update goroutine; the
// engine only pushes into the shared UI queue.
type groupSession struct {
	code   string
	name   string
	desc   string
	sig    *signalClient
	eng    *engine
	unread int
}

// ─── validation (mirrors the server contract, pre-flight only) ──────────────

// validateGroupName returns an inline error string ("" = valid): 1-64 chars.
func validateGroupName(name string) string {
	n := len([]rune(strings.TrimSpace(name)))
	if n < 1 || n > 64 {
		return "group name must be 1-64 characters"
	}
	return ""
}

// validateGroupDesc returns an inline error string ("" = valid): <= 256 chars.
func validateGroupDesc(desc string) string {
	if len([]rune(desc)) > 256 {
		return "group description must be at most 256 characters"
	}
	return ""
}

// validateMaxMembers parses the Max-People field: empty = unlimited (nil),
// otherwise an integer >= 2. The returned error doubles as the inline
// message.
func validateMaxMembers(raw string) (*int, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n < 2 || n > 1000000 {
		return nil, "Max-People must be a whole number of at least 2 (or empty for unlimited)"
	}
	return &n, ""
}

// ─── group endpoints ─────────────────────────────────────────────────────────

// rawEndpoint builds a server URL outside the /session/ tree (the invites
// list is user-scoped, not session-scoped).
func (c *signalClient) rawEndpoint(path string) string {
	base := c.serverURL
	if len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	return base + path
}

// createGroupRoom mints a group session. groupName/groupDesc/maxMembers
// follow the server contract; parentCode is the room the group was created
// from. Returns the fresh session code (also parked in c.key).
func (c *signalClient) createGroupRoom(username, pubkey, groupName, groupDesc string, maxMembers *int, parentCode string) (string, error) {
	payload := map[string]any{
		"username":  username,
		"pubkey":    pubkey,
		"groupName": groupName,
	}
	if groupDesc != "" {
		payload["groupDesc"] = groupDesc
	}
	if maxMembers != nil {
		payload["maxMembers"] = *maxMembers
	} else {
		payload["maxMembers"] = nil // explicit unlimited
	}
	if parentCode != "" {
		payload["parentCode"] = parentCode
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
	if err := jsonDecode(body, &r); err != nil || r.SessionId == "" {
		return "", fmt.Errorf("server returned no session id for the group")
	}
	c.key = r.SessionId
	return r.SessionId, nil
}

// sendInvite invites one user into the current session (the new group).
// 409 "User is already in this session" is a normal outcome when the user
// somehow pre-dates the invite; callers surface it as an inline note and
// continue with the rest.
func (c *signalClient) sendInvite(username string) error {
	code, body, err := postJSON(c.endpoint("/invites"), map[string]any{"username": username}, c.sigHeaders("POST", c.endpoint("/invites")))
	if err != nil {
		return err
	}
	if code != 201 {
		return apiErr(code, body)
	}
	return nil
}

// myInvites lists my pending invites, newest first (server caps at 50).
// User-scoped, so it rides the raw endpoint outside /session/.
func (c *signalClient) myInvites() ([]groupInvite, error) {
	code, body, err := getJSON(c.rawEndpoint("/api/v1/invites/mine"), c.sigHeadersWithPubkey("GET", c.rawEndpoint("/api/v1/invites/mine")))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, apiErr(code, body)
	}
	var r struct {
		Invites []groupInvite `json:"invites"`
	}
	if err := jsonDecode(body, &r); err != nil {
		return nil, err
	}
	if r.Invites == nil {
		r.Invites = []groupInvite{}
	}
	return r.Invites, nil
}

// acceptInvite joins the invite's session (server-side join, identical to
// POST /join). Password is checked FIRST by the server: 401 "Password is
// required for this session" (empty password) or "Incorrect session
// password"; then "Invite not found" (404); a full session answers 403
// "Maximum allowance is reached" AND consumes the invite (terminal — a
// re-accept of the same code 404s, never retry). Success mirrors /join:
// roster + epoch.
func (c *signalClient) acceptInvite(code, pubkey, password string) ([]rosterMember, int64, error) {
	payload := map[string]any{"code": code, "pubkey": pubkey}
	if password != "" {
		payload["password"] = password
	}
	status, body, err := postJSON(c.rawEndpoint("/api/v1/session/"+url.PathEscape(code)+"/invites/accept"), payload,
		c.sigHeaders("POST", c.rawEndpoint("/api/v1/session/"+url.PathEscape(code)+"/invites/accept")))
	if err != nil {
		return nil, 0, err
	}
	if status != 200 {
		return nil, 0, apiErr(status, body)
	}
	var r struct {
		SessionId string         `json:"sessionId"`
		Roster    []rosterMember `json:"roster"`
		Epoch     int64          `json:"epoch"`
	}
	if err := jsonDecode(body, &r); err != nil {
		return nil, 0, err
	}
	c.key = r.SessionId
	if c.key == "" {
		c.key = code
	}
	return r.Roster, r.Epoch, nil
}

// declineInvite consumes a pending invite (200 {ok:true}); a missing invite
// answers 404 "Invite not found".
func (c *signalClient) declineInvite(code string) error {
	status, body, err := postJSON(c.rawEndpoint("/api/v1/session/"+url.PathEscape(code)+"/invites/decline"),
		map[string]any{"code": code}, c.sigHeadersWithPubkey("POST", c.rawEndpoint("/api/v1/session/"+url.PathEscape(code)+"/invites/decline")))
	if err != nil {
		return err
	}
	if status != 200 {
		return apiErr(status, body)
	}
	return nil
}

// ─── desktop ping ────────────────────────────────────────────────────────────

// notifyInvited is the desktop ping for a fresh invite: EXACT body
// "<by> invited you to group <name>" (name falls back for unnamed groups).
func notifyInvited(by, group string) {
	if group == "" {
		group = "a group"
	}
	notify("Uplink-Delta", fmt.Sprintf("%s invited you to group %s", by, group))
}

// inviteNotifier is the invite-ping sink, swappable so tests can capture
// the exact body without popping real toasts.
var inviteNotifier = notifyInvited

// pubkeyB64 is the short helper every group join path needs: the device
// identity's advertised pubkey.
func pubkeyB64(id *identityKey) string {
	return base64.StdEncoding.EncodeToString(id.publicKey())
}
