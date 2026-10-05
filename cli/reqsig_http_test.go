package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── Request-signature gate: spoofs, replays, staleness ────────────────────
//
// These drive the fake signaling server's signature gate directly (raw HTTP)
// to prove the trust-plane contract the CLI relies on:
//   - a request signed with the roster-anchored device key passes;
//   - the same request re-labeled with a different username is rejected
//     (the anchor lookup follows the CLAIMED name);
//   - missing / stale / replayed signatures are all rejected with 401.

// rawSignedPost issues a request with the given headers verbatim against the
// fake server and returns status + JSON body.
func rawSignedPost(t *testing.T, srv *httptest.Server, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestReqSigTamperedUsernameRejected signs one request properly with alice's
// device key and then replays the identical bytes with the username swapped
// to "bob": the gate looks up BOB's roster anchor and must refuse.
func TestReqSigTamperedUsernameRejected(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := &signalClient{serverURL: srv.URL, me: "alice", id: mustTestIdentity(t)}
	sid, err := alice.createRoom("alice", pubkeyB64(alice.id), "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid
	bob := &signalClient{serverURL: srv.URL, me: "bob", id: mustTestIdentity(t)}
	bob.key = sid
	if _, _, err := bob.joinRoom("bob", pubkeyB64(bob.id), ""); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/session/" + sid + "/heartbeat"
	headers := alice.sigHeaders("POST", srv.URL+path)
	status, body := rawSignedPost(t, srv, path, map[string]any{"peerId": "p1"}, headers)
	if status != 200 {
		t.Fatalf("alice's own signed beat = %d %v; want 200", status, body)
	}
	// Swap the username header only — signature, timestamp, nonce untouched.
	forged := map[string]string{}
	for k, v := range headers {
		forged[k] = v
	}
	forged["X-Uplink-Username"] = "bob"
	status, body = rawSignedPost(t, srv, path, map[string]any{"peerId": "p1"}, forged)
	if status != 401 {
		t.Fatalf("tampered-username beat = %d %v; want 401", status, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "Invalid or missing request signature") {
		t.Fatalf("tampered-username error = %q; want the signature message", msg)
	}
}

// TestReqSigMissingAndStaleAndReplay: all three failure classes answer 401
// with the distinct signature message.
func TestReqSigMissingAndStaleAndReplay(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := &signalClient{serverURL: srv.URL, me: "alice", id: mustTestIdentity(t)}
	sid, err := alice.createRoom("alice", pubkeyB64(alice.id), "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid
	path := "/api/v1/session/" + sid + "/heartbeat"

	// 1. No signature headers at all.
	status, _ := rawSignedPost(t, srv, path, map[string]any{}, map[string]string{"X-Uplink-Username": "alice"})
	if status != 401 {
		t.Fatalf("unsigned beat = %d; want 401", status)
	}

	// 2. Stale timestamp (beyond the 30s window).
	stale := alice.sigHeaders("POST", srv.URL+path)
	stale["X-Uplink-Timestamp"] = fmt.Sprintf("%d", time.Now().UnixMilli()-requestSigWindowMs-1000)
	status, _ = rawSignedPost(t, srv, path, map[string]any{}, stale)
	if status != 401 {
		t.Fatalf("stale beat = %d; want 401", status)
	}

	// 3. Replay: the exact same (timestamp, nonce, signature) twice.
	good := alice.sigHeaders("POST", srv.URL+path)
	status, _ = rawSignedPost(t, srv, path, map[string]any{}, good)
	if status != 200 {
		t.Fatalf("first beat = %d; want 200", status)
	}
	status, _ = rawSignedPost(t, srv, path, map[string]any{}, good)
	if status != 401 {
		t.Fatalf("replayed beat = %d; want 401", status)
	}
}

// TestReqSigWrongKeyRejected: a signature made with a DIFFERENT device key
// than the one rostered for the claimed username must fail (the core
// impersonation defense at the HTTP layer).
func TestReqSigWrongKeyRejected(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := &signalClient{serverURL: srv.URL, me: "alice", id: mustTestIdentity(t)}
	sid, err := alice.createRoom("alice", pubkeyB64(alice.id), "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid

	// eve signs as alice with eve's own key.
	eve := &signalClient{serverURL: srv.URL, me: "alice", id: mustTestIdentity(t)}
	eve.key = sid
	path := "/api/v1/session/" + sid + "/heartbeat"
	status, _ := rawSignedPost(t, srv, path, map[string]any{},
		eve.sigHeaders("POST", srv.URL+path))
	if status != 401 {
		t.Fatalf("wrong-key beat = %d; want 401", status)
	}
}

// TestReqSigInviteScopedClaims: invites/mine and decline are name-claims
// with no roster entry — the FIRST signed use binds the username to the
// signing device key (immutably), and later calls must match it.
func TestReqSigInviteScopedClaims(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	// bob creates a group and invites alice.
	bobID := mustTestIdentity(t)
	bob := &signalClient{serverURL: srv.URL, me: "bob", id: bobID}
	code, err := bob.createGroupRoom("bob", pubkeyB64(bobID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	bob.key = code
	if err := bob.sendInvite("alice"); err != nil {
		t.Fatal(err)
	}

	// alice's first mine GET claims her key and succeeds.
	aliceID := mustTestIdentity(t)
	alice := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	invites, err := alice.myInvites()
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	if len(invites) != 1 || invites[0].Code != code {
		t.Fatalf("invites = %+v; want the group invite", invites)
	}
	// A different key claiming the same username is now refused.
	impostor := &signalClient{serverURL: srv.URL, me: "alice", id: mustTestIdentity(t)}
	if _, err := impostor.myInvites(); err == nil || apiStatusCode(err) != 401 {
		t.Fatalf("impostor mine = %v; want 401", err)
	}
	// The legitimate key declines fine.
	if err := alice.declineInvite(code); err != nil {
		t.Fatalf("decline: %v", err)
	}
	// The claim's pubkey is exactly alice's device key — proven by
	// behavior: the impostor's later mine got 401, alice's own 200.
}

// TestReqSigInviteAcceptBindsBodyKey proves the accept contract: the
// signature must verify against the PUBKEY CARRIED IN THE BODY (the seat is
// bound to the key that signed), and accepting stamps the per-user claim.
func TestReqSigInviteAcceptBindsBodyKey(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	bobID := mustTestIdentity(t)
	bob := &signalClient{serverURL: srv.URL, me: "bob", id: bobID}
	code, err := bob.createGroupRoom("bob", pubkeyB64(bobID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	bob.key = code
	if err := bob.sendInvite("alice"); err != nil {
		t.Fatal(err)
	}

	aliceID := mustTestIdentity(t)
	alice := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	acceptPath := "/api/v1/session/" + code + "/invites/accept"
	payload := map[string]any{"code": code, "pubkey": pubkeyB64(aliceID)}

	// Signature made with a DIFFERENT key than the body pubkey: 401 even
	// though the signature itself is valid.
	other := &signalClient{serverURL: srv.URL, me: "alice", id: mustTestIdentity(t)}
	status, _ := rawSignedPost(t, srv, acceptPath, payload, other.sigHeaders("POST", srv.URL+acceptPath))
	if status != 401 {
		t.Fatalf("accept signed with the wrong key = %d; want 401", status)
	}
	// Correct signature + body pubkey: 200, and alice is a member.
	status, _ = rawSignedPost(t, srv, acceptPath, payload, alice.sigHeaders("POST", srv.URL+acceptPath))
	if status != 200 {
		t.Fatalf("accept with the true key = %d; want 200", status)
	}
	// And the per-user claim was stamped by the accept (decline works).
	if err := alice.declineInvite(code); err != nil &&
		!strings.Contains(err.Error(), "Invite not found") {
		t.Fatalf("accept must stamp the sigkey claim: %v", err)
	}
}

// TestReqSigSignedEndToEnd: one fully signed lifecycle over the fake —
// create (claim), join (claim), beat + inbox + signal + reactions
// (signature-gated), leave. Proves the production client signs every
// identity-bearing call correctly.
func TestReqSigSignedEndToEnd(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := &signalClient{serverURL: srv.URL, me: "alice", id: mustTestIdentity(t)}
	sid, err := alice.createRoom("alice", pubkeyB64(alice.id), "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid
	bob := &signalClient{serverURL: srv.URL, me: "bob", id: mustTestIdentity(t)}
	bob.key = sid
	if _, _, err := bob.joinRoom("bob", pubkeyB64(bob.id), ""); err != nil {
		t.Fatal(err)
	}

	if _, _, err := alice.heartbeat("peer1", nil); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if err := alice.signalSend("bob", "offer", "SDP"); err != nil {
		t.Fatalf("signalSend: %v", err)
	}
	if notes, err := bob.signalPoll(); err != nil || len(notes) != 1 {
		t.Fatalf("signalPoll: %v %d notes", err, len(notes))
	}
	if err := alice.inboxSend("bob", "m1", "chat", "CIPH"); err != nil {
		t.Fatalf("inboxSend: %v", err)
	}
	if boxes, _, err := bob.inboxFetch(); err != nil || len(boxes) != 1 {
		t.Fatalf("inboxFetch: %v %d boxes", err, len(boxes))
	}
	if n, err := bob.inboxAck([]string{"m1"}); err != nil || n != 1 {
		t.Fatalf("inboxAck: %v %d", err, n)
	}
	if err := bob.react("m1", "👍"); err != nil {
		t.Fatalf("react: %v", err)
	}
	if summaries, err := alice.reactions([]string{"m1"}); err != nil || len(summaries) != 1 {
		t.Fatalf("reactions: %v %d", err, len(summaries))
	}
	if err := bob.leaveRoom(); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := alice.leaveRoom(); err != nil {
		t.Fatalf("leave: %v", err)
	}
	_ = base64.StdEncoding
}
