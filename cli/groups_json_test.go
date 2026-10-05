package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Invites/groups JSON contract: decode tests against the REAL server payload
// shapes (src/app/api/v1/...), so a server-side emit change breaks in the
// client test suite with the exact field named, not as a mystery number-
// into-string failure in the running TUI.
// ---------------------------------------------------------------------------

// TestInviteMineDecodesRealServerShape pins the GET /invites/mine contract:
// the route answers {"invites":[{code, groupName|null, groupDesc|null, by,
// at}]} where "at" is epoch-MILLIS — a JSON NUMBER, never a string —
// and groupName/groupDesc are nullable. This is the literal payload the
// server emits; the client struct must decode it without the
// "json: cannot unmarshal number into Go struct field groupInvite.at"
// failure that historically broke the settings inbox as
// "invites unavailable - json: ..." .
func TestInviteMineDecodesRealServerShape(t *testing.T) {
	body := []byte(`{"invites":[
		{"code":"123456","groupName":"Squad","groupDesc":"the squad","by":"alice","at":1720000000000},
		{"code":"654321","groupName":null,"groupDesc":null,"by":"bob","at":1720000001234}
	]}`)
	var r struct {
		Invites []groupInvite `json:"invites"`
	}
	if err := jsonDecode(body, &r); err != nil {
		t.Fatalf("decode real /invites/mine payload: %v", err)
	}
	if len(r.Invites) != 2 {
		t.Fatalf("invites = %d; want 2", len(r.Invites))
	}
	first := r.Invites[0]
	if first.Code != "123456" || first.GroupName != "Squad" || first.GroupDesc != "the squad" || first.By != "alice" {
		t.Fatalf("first invite = %+v", first)
	}
	if first.At != 1720000000000 {
		t.Fatalf("at = %d; want the epoch-millis number 1720000000000", first.At)
	}
	// null name/desc decode as empty without error (real unnamed groups).
	second := r.Invites[1]
	if second.GroupName != "" || second.GroupDesc != "" || second.By != "bob" || second.At != 1720000001234 {
		t.Fatalf("null-name invite = %+v", second)
	}
}

// TestInviteMineRawDecodeErrorSurfaces proves a decode failure on a real
// HTTP response rides out of myInvites UNWRAPPED — the settings window
// surfaces msg.err.Error() verbatim ("invites unavailable — <raw json
// error>"), so the exact offending field stays diagnosable.
func TestInviteMineRawDecodeErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"invites":[{"code":"700001","groupName":null,"groupDesc":null,"by":"bob","at":"not-a-number"}]}`))
	}))
	defer srv.Close()
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	sig := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	_, err = sig.myInvites()
	if err == nil {
		t.Fatal("a malformed /invites/mine payload must fail the decode")
	}
	if !strings.Contains(err.Error(), "cannot unmarshal") || !strings.Contains(err.Error(), ".invites.0.at") || !strings.Contains(err.Error(), "of type int64") {
		t.Fatalf("decode failure must surface the raw json error naming the field, got: %v", err)
	}
}

// TestAcceptInviteDecodesRealServerShape pins the POST
// /api/v1/session/{code}/invites/accept response: sessionId + participants
// + roster (username/pubkey/online/peerId/addrs — role is ABSENT from the
// route's mapping) + epoch as a JSON NUMBER.
func TestAcceptInviteDecodesRealServerShape(t *testing.T) {
	body := []byte(`{"sessionId":"123456","participants":["alice","bob"],
		"roster":[
			{"username":"alice","pubkey":"pk_alice","online":true,"peerId":"p1","addrs":["1.1.1.1:9"]},
			{"username":"bob","pubkey":"pk_bob","online":false}
		],"epoch":7}`)
	var r struct {
		SessionId string         `json:"sessionId"`
		Roster    []rosterMember `json:"roster"`
		Epoch     int64          `json:"epoch"`
	}
	if err := jsonDecode(body, &r); err != nil {
		t.Fatalf("decode accept response: %v", err)
	}
	if r.SessionId != "123456" || r.Epoch != 7 || len(r.Roster) != 2 {
		t.Fatalf("accept = sessionId %q epoch %d roster %d; want 123456/7/2", r.SessionId, r.Epoch, len(r.Roster))
	}
	// role is not in the accept mapping — must stay "" (not an error).
	if r.Roster[0].Role != "" || !r.Roster[0].Online || r.Roster[0].PeerId != "p1" || len(r.Roster[0].Addrs) != 1 {
		t.Fatalf("roster[0] = %+v", r.Roster[0])
	}
	if r.Roster[1].Username != "bob" || r.Roster[1].Role != "" || r.Roster[1].Online {
		t.Fatalf("roster[1] = %+v", r.Roster[1])
	}
}