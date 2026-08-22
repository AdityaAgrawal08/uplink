#!/usr/bin/env bash
# Collaborative-session lifecycle test: create -> join -> announce -> share
# upload/confirm -> upload-complete -> heartbeat -> files -> download -> cleanup.
# Skips gracefully when session routes are absent from the branch under test.
set -u
cd "$(dirname "$0")/.." || exit 1

SERVER="${SERVER:-http://localhost:3000}"
BODY="$(mktemp "${TMPDIR:-/tmp}/uplink-sf-body.XXXXXX")"
FIXTURE="$(mktemp "${TMPDIR:-/tmp}/uplink-sf-fixture.XXXXXX")"
PASS=0; FAIL=0
ok()  { printf '  [PASS] %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL+1)); }
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))" 2>/dev/null; }
code() { curl -s --max-time 20 -o "$BODY" -w '%{http_code}' "$@"; }

if [ ! -d src/app/api/v1/session ]; then
    echo "session routes not present on this branch — SKIPPING"
    exit 0
fi

printf 'ci session payload %s\n' "$(date +%s)" > "$FIXTURE"
SIZE=$(wc -c < "$FIXTURE" | tr -d ' ')
HASH=$(sha256sum "$FIXTURE" | cut -d' ' -f1)

echo "=== S-C1: create session ==="
ST=$(code -X POST "$SERVER/api/v1/session/create" -H 'Content-Type: application/json' \
     -d '{"username":"ci_alice","duration":300}')
SID=$(jget sessionId < "$BODY")
if [ "$ST" = "201" ] && [ -n "$SID" ]; then ok "created $SID"; else bad "create status=$ST body=$(cat "$BODY")"; fi

echo "=== S-C2: join peer ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/join" -H 'Content-Type: application/json' \
     -d '{"username":"ci_bob"}')
BOB_IN=$(python3 -c "import json;print('ci_bob' in json.load(open('"$BODY"')).get('participants',[]))" 2>/dev/null)
if [ "$ST" = "200" ] && [ "$BOB_IN" = "True" ]; then ok "joined, participant listed"; else bad "join status=$ST body=$(cat "$BODY")"; fi

echo "=== S-C3: announce file ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/announce" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ci_alice' \
     -d "{\"filename\":\"ci.txt\",\"size\":$SIZE,\"sha256\":\"$HASH\"}")
FILEID=$(jget fileId < "$BODY")
SHAREID=$(jget shareId < "$BODY")
if [ "$ST" = "201" ] && [ -n "$FILEID" ] && [ -n "$SHAREID" ]; then ok "fileId=$FILEID shareId=$SHAREID"
else bad "announce status=$ST body=$(cat "$BODY")"; fi

echo "=== S-C4: upload underlying share (init->PUT->confirm) ==="
ST=$(code -X POST "$SERVER/api/v1/share/init" -H 'Content-Type: application/json' \
     -d "{\"shareId\":\"$SHAREID\",\"filename\":\"ci.txt\",\"size\":$SIZE,\"mimeType\":\"text/plain\",\"hashValue\":\"$HASH\",\"partsCount\":0}")
UPURL=$(jget uploadUrl < "$BODY")
if [ "$ST" = "201" ] && [ -n "$UPURL" ]; then ok "init ok"; else bad "init status=$ST body=$(head -c 120 "$BODY")"; fi
ST=$(code -X PUT "$UPURL" -H 'Content-Type: text/plain' --data-binary @"$FIXTURE")
[ "$ST" = "200" ] && ok "PUT mock storage" || bad "PUT status=$ST"
ST=$(code -X POST "$SERVER/api/v1/share/$SHAREID/confirm" -H 'Content-Type: application/json' -d '{}')
grep -q '"ACTIVE"' "$BODY" && ok "confirmed ACTIVE" || bad "confirm body=$(cat "$BODY")"

echo "=== S-C5: mark upload complete ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/upload-complete" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ci_alice' \
     -d "{\"fileId\":\"$FILEID\",\"shareId\":\"$SHAREID\"}")
grep -q '"success": *true\|"success":true' "$BODY" && ok "upload-complete" || bad "status=$ST body=$(cat "$BODY")"

echo "=== S-C6: heartbeat presence ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/heartbeat" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ci_bob' \
     -d '{"peerId":"12D3KooWCItestPeer","addrs":["/ip4/127.0.0.1/tcp/4001"]}')
[ "$ST" = "200" ] && ok "heartbeat" || bad "status=$ST"

echo "=== S-C7: files listing ==="
ST=$(code "$SERVER/api/v1/session/$SID/files")
HAS_FILE=$(FILEID="$FILEID" BODY="$BODY" python3 -c 'import json,os;d=json.load(open(os.environ["BODY"]));fid=os.environ["FILEID"];print(any(f.get("fileId")==fid for f in d.get("files",[])))' 2>/dev/null)
if [ "$ST" = "200" ] && [ "$HAS_FILE" = "True" ]; then ok "file listed"; else bad "files status=$ST body=$(head -c 160 "$BODY")"; fi

echo "=== S-C8: session-scoped download ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/download/$FILEID" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ci_bob' -d '{}')
DLURL=$(jget downloadUrl < "$BODY")
if [ "$ST" = "200" ] && [ -n "$DLURL" ]; then ok "download authorized"; else bad "status=$ST body=$(head -c 160 "$BODY")"; fi

echo "=== S-C9: cleanup run ==="
ST=$(code -X POST "$SERVER/api/v1/session/cleanup")
[ "$ST" = "200" ] && ok "cleanup executed" || bad "status=$ST"

echo "RESULT: PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
