#!/usr/bin/env bash
# Signaling-plane lifecycle test (Vercel-native architecture):
# create -> join -> heartbeat/roster -> signal rendezvous -> inbox/ack ->
# guards -> leave (room dies on empty) -> cleanup.
set -u
cd "$(dirname "$0")/.." || exit 1

SERVER="${SERVER:-http://localhost:3000}"
BODY="$(mktemp "${TMPDIR:-/tmp}/uplink-sg-body.XXXXXX")"
PASS=0; FAIL=0
ok()  { printf '  [PASS] %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL+1)); }
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))" 2>/dev/null; }
code() { curl -s --max-time 20 -o "$BODY" -w '%{http_code}' "$@"; }
PUBKEY=$(python3 -c "import base64,os;print(base64.b64encode(bytes(range(32))).decode())")

if [ ! -d src/app/api/v1/session ]; then
    echo "session routes not present on this branch — SKIPPING"
    exit 0
fi

echo "=== S-C1: create session ==="
ST=$(code -X POST "$SERVER/api/v1/session/create" -H 'Content-Type: application/json' \
     -d "{\"username\":\"sg_alice\",\"pubkey\":\"$PUBKEY\"}")
SID=$(jget sessionId < "$BODY")
if [ "$ST" = "201" ] && [ -n "$SID" ]; then ok "created $SID"; else bad "create status=$ST body=$(cat "$BODY")"; fi

echo "=== S-C2: join peer (roster carries pubkeys) ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/join" -H 'Content-Type: application/json' \
     -d "{\"username\":\"sg_bob\",\"pubkey\":\"$PUBKEY\"}")
HASBOB=$(python3 -c "import json;d=json.load(open('"$BODY"'));print('sg_bob' in d.get('participants',[]) and all('pubkey' in m for m in d.get('roster',[])))")
if [ "$ST" = "200" ] && [ "$HASBOB" = "True" ]; then ok "joined, roster has pubkeys"; else bad "join status=$ST body=$(cat "$BODY")"; fi

echo "=== S-C3: duplicate username 409 ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/join" \
     -H 'Content-Type: application/json' -d "{\"username\":\"sg_alice\",\"pubkey\":\"$PUBKEY\"}")
[ "$ST" = "409" ] && ok "duplicate username 409" || bad "dup expected 409, got $ST"

echo "=== S-C4: heartbeat presence ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/heartbeat" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: sg_bob' \
     -d '{"peerId":"peer-bob","addrs":["/ip4/127.0.0.1/tcp/4001"]}')
HASBOTH=$(python3 -c "import json;d=json.load(open('"$BODY"'));u=d.get('activeUsers',[]);print('sg_alice' in u and 'sg_bob' in u)" 2>/dev/null)
if [ "$ST" = "200" ] && [ "$HASBOTH" = "True" ]; then ok "heartbeat + roster"; else bad "status=$ST body=$(cat "$BODY")"; fi

echo "=== S-C5: signaling rendezvous (offer/answer drain) ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/signal" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: sg_alice' \
     -d '{"to":"sg_bob","type":"offer","payload":"SDP-OFFER-BYTES"}')
[ "$ST" = "201" ] && ok "offer deposited" || bad "offer status=$ST body=$(cat "$BODY")"
ST=$(code "$SERVER/api/v1/session/$SID/signal" -H 'X-Uplink-Username: sg_bob')
GOTOFFER=$(python3 -c "import json;d=json.load(open('"$BODY"'));print(any(n.get('type')=='offer' and n.get('from')=='sg_alice' for n in d.get('notes',[])))" 2>/dev/null)
[ "$ST" = "200" ] && [ "$GOTOFFER" = "True" ] && ok "bob drains offer" || bad "drain wrong: $ST"
ST=$(code "$SERVER/api/v1/session/$SID/signal" -H 'X-Uplink-Username: sg_bob')
EMPTY=$(python3 -c "import json;d=json.load(open('"$BODY"'));print(d.get('notes',[]))" 2>/dev/null)
[ "$EMPTY" = "[]" ] && ok "drain clears queue" || bad "queue not cleared: $EMPTY"

echo "=== S-C6: inbox deposit/fetch/ack ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/inbox" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: sg_alice' \
     -d '{"to":"sg_bob","msgId":"box1","kind":"chat","payload":"CIPHERTEXT-1"}')
[ "$ST" = "201" ] && ok "box deposited" || bad "deposit status=$ST body=$(cat "$BODY")"
# idempotent retry must not duplicate
code -X POST "$SERVER/api/v1/session/$SID/inbox" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: sg_alice' \
     -d '{"to":"sg_bob","msgId":"box1","kind":"chat","payload":"CIPHERTEXT-1"}' > /dev/null
ST=$(code "$SERVER/api/v1/session/$SID/inbox" -H 'X-Uplink-Username: sg_bob')
COUNT=$(python3 -c "import json;d=json.load(open('"$BODY"'));print(len(d.get('boxes',[])))" 2>/dev/null)
[ "$COUNT" = "1" ] && ok "fetch returns box once (idempotent)" || bad "count=$COUNT"
ST=$(code -X POST "$SERVER/api/v1/session/$SID/inbox/ack" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: sg_bob' \
     -d '{"ids":["box1"]}')
grep -q '"removed":1\|"removed": 1' "$BODY" && ok "ACK deletes box" || bad "ack body=$(cat "$BODY")"
ST=$(code "$SERVER/api/v1/session/$SID/inbox" -H 'X-Uplink-Username: sg_bob')
EMPTY=$(python3 -c "import json;d=json.load(open('"$BODY"'));print(d.get('boxes',[]))" 2>/dev/null)
[ "$EMPTY" = "[]" ] && ok "inbox empty after ACK" || bad "inbox not empty: $EMPTY"

echo "=== S-C7: guards ==="
ST=$(code "$SERVER/api/v1/session/$SID/inbox" -H 'X-Uplink-Username: eve_outsider')
[ "$ST" = "403" ] && ok "non-member fetch rejected (403)" || bad "expected 403, got $ST"
ST=$(code -X POST "$SERVER/api/v1/session/$SID/inbox" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: sg_alice' \
     -d '{"to":"ghost","msgId":"m","kind":"chat","payload":"x"}')
[ "$ST" = "404" ] && ok "unknown recipient rejected (404)" || bad "expected 404, got $ST"
ST=$(code -X POST "$SERVER/api/v1/session/$SID/inbox" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: sg_alice' \
     -d '{"to":"sg_alice","msgId":"m","kind":"chat","payload":"x"}')
[ "$ST" = "400" ] && ok "self-box rejected (400)" || bad "expected 400, got $ST"

echo "=== S-C8: password rooms ==="
ST=$(code -X POST "$SERVER/api/v1/session/create" -H 'Content-Type: application/json' \
     -d "{\"username\":\"sg_owner\",\"pubkey\":\"$PUBKEY\",\"password\":\"s3cret\"}")
PSID=$(jget sessionId < "$BODY")
[ "$ST" = "201" ] && ok "password room created" || bad "create status=$ST"
ST=$(code -X POST "$SERVER/api/v1/session/$PSID/join" \
     -H 'Content-Type: application/json' -d "{\"username\":\"sg_guest\",\"pubkey\":\"$PUBKEY\"}")
[ "$ST" = "401" ] && ok "password required (401)" || bad "expected 401, got $ST"
ST=$(code -X POST "$SERVER/api/v1/session/$PSID/join" \
     -H 'Content-Type: application/json' -d "{\"username\":\"sg_guest\",\"pubkey\":\"$PUBKEY\",\"password\":\"wrong\"}")
[ "$ST" = "401" ] && ok "wrong password rejected (401)" || bad "expected 401, got $ST"
ST=$(code -X POST "$SERVER/api/v1/session/$PSID/join" \
     -H 'Content-Type: application/json' -d "{\"username\":\"sg_guest\",\"pubkey\":\"$PUBKEY\",\"password\":\"s3cret\"}")
[ "$ST" = "200" ] && ok "correct password joins" || bad "expected 200, got $ST"
code -X POST "$SERVER/api/v1/session/$PSID/leave" -H 'X-Uplink-Username: sg_guest' > /dev/null
code -X POST "$SERVER/api/v1/session/$PSID/leave" -H 'X-Uplink-Username: sg_owner' > /dev/null

echo "=== S-C9: leave destroys room when empty ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/leave" -H 'X-Uplink-Username: sg_alice')
grep -q '"ended":false' "$BODY" && ok "alice left, room lives" || bad "leave1: $(cat "$BODY")"
ST=$(code -X POST "$SERVER/api/v1/session/$SID/leave" -H 'X-Uplink-Username: sg_bob')
grep -q '"ended":true' "$BODY" && ok "last leave ends room" || bad "leave2: $(cat "$BODY")"
ST=$(code -X POST "$SERVER/api/v1/session/$SID/join" \
     -H 'Content-Type: application/json' -d "{\"username\":\"sg_late\",\"pubkey\":\"$PUBKEY\"}")
[ "$ST" = "404" ] && ok "destroyed room is gone (404)" || bad "expected 404, got $ST"

echo "=== S-C10: cleanup run ==="
ST=$(code -X POST "$SERVER/api/v1/session/cleanup")
[ "$ST" = "200" ] && ok "cleanup executed" || bad "status=$ST"

echo "RESULT: PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
