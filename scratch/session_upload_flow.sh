#!/usr/bin/env bash
# Session /upload feature gate: exercises exactly what the CLI picker's
# engine and receiver depend on beyond session_flow_test.sh:
#   1. /files?since= watermark returns only newer uploads
#   2. an upload by alice is visible when bob polls (cross-participant)
#   3. presigned download round-trip is byte-identical
set -u
cd "$(dirname "$0")/.." || exit 1

SERVER="${SERVER:-http://localhost:3000}"
BODY="$(mktemp "${TMPDIR:-/tmp}/uplink-ul-body.XXXXXX")"
FIXTURE="$(mktemp "${TMPDIR:-/tmp}/uplink-ul-fixture.XXXXXX")"
GOT="$(mktemp "${TMPDIR:-/tmp}/uplink-ul-got.XXXXXX")"
PASS=0; FAIL=0
ok()  { printf '  [PASS] %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL+1)); }
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))" 2>/dev/null; }
code() { curl -s --max-time 20 -o "$BODY" -w '%{http_code}' "$@"; }

if [ ! -d src/app/api/v1/session ]; then
    echo "session routes not present on this branch — SKIPPING"
    exit 0
fi

head -c 300000 /dev/urandom > "$FIXTURE"      # ~300KB binary payload
FIXTURE1="$(mktemp "${TMPDIR:-/tmp}/uplink-ul-fix1.XXXXXX")"
cp "$FIXTURE" "$FIXTURE1"                     # kept for the final byte-compare
SIZE=$(wc -c < "$FIXTURE" | tr -d ' ')
HASH=$(sha256sum "$FIXTURE" | cut -d' ' -f1)

echo "=== S-U1: create + join ==="
ST=$(code -X POST "$SERVER/api/v1/session/create" -H 'Content-Type: application/json' \
     -d '{"username":"ul_alice","duration":300}')
SID=$(jget sessionId < "$BODY")
[ "$ST" = "201" ] && [ -n "$SID" ] && ok "created $SID" || { bad "create status=$ST"; exit 1; }
code -X POST "$SERVER/api/v1/session/$SID/join" -H 'Content-Type: application/json' \
     -d '{"username":"ul_bob"}' > /dev/null

echo "=== S-U3: alice announces + pushes bytes ==="
ST=$(code -X POST "$SERVER/api/v1/session/$SID/announce" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ul_alice' \
     -d "{\"filename\":\"ul-payload.bin\",\"size\":$SIZE,\"sha256\":\"$HASH\"}")
FILEID=$(jget fileId < "$BODY"); SHAREID=$(jget shareId < "$BODY")
[ "$ST" = "201" ] && ok "announced" || bad "announce status=$ST body=$(cat "$BODY")"

ST=$(code -X POST "$SERVER/api/v1/share/init" -H 'Content-Type: application/json' \
     -d "{\"shareId\":\"$SHAREID\",\"filename\":\"ul-payload.bin\",\"size\":$SIZE,\"mimeType\":\"application/octet-stream\",\"hashValue\":\"$HASH\"}")
UPURL=$(jget uploadUrl < "$BODY")
[ "$ST" = "201" ] && [ -n "$UPURL" ] && ok "init minted PUT url" || bad "init status=$ST body=$(head -c 120 "$BODY")"
ST=$(code -X PUT "$UPURL" -H 'Content-Type: application/octet-stream' --data-binary @"$FIXTURE")
[ "$ST" = "200" ] && ok "bytes stored" || bad "PUT status=$ST"
code -X POST "$SERVER/api/v1/share/$SHAREID/confirm" -H 'Content-Type: application/json' -d '{}' > /dev/null
code -X POST "$SERVER/api/v1/session/$SID/upload-complete" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ul_alice' \
     -d "{\"fileId\":\"$FILEID\",\"shareId\":\"$SHAREID\"}" > /dev/null

echo "=== S-U4: since= watermark strictly-new semantics ==="
# Capture file #1's uploadedAt as the watermark, then push a second file.
UPAT=$(code "$SERVER/api/v1/session/$SID/files" > /dev/null; python3 -c "import json;d=json.load(open('$BODY'));print([f['uploadedAt'] for f in d.get('files',[]) if f['fileId']=='$FILEID'][0])")
printf 'second payload %s\n' "$(date +%s)" > "$FIXTURE"
SIZE=$(wc -c < "$FIXTURE" | tr -d ' ')
HASH=$(sha256sum "$FIXTURE" | cut -d' ' -f1)
ST=$(code -X POST "$SERVER/api/v1/session/$SID/announce" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ul_bob' \
     -d "{\"filename\":\"ul-second.txt\",\"size\":$SIZE,\"sha256\":\"$HASH\"}")
FILEID2=$(jget fileId < "$BODY"); SHAREID2=$(jget shareId < "$BODY")
[ "$ST" = "201" ] && ok "second announced (by bob)" || bad "announce2 status=$ST"
code -X POST "$SERVER/api/v1/share/init" -H 'Content-Type: application/json' \
     -d "{\"shareId\":\"$SHAREID2\",\"filename\":\"ul-second.txt\",\"size\":$SIZE,\"mimeType\":\"text/plain\",\"hashValue\":\"$HASH\"}" > /dev/null
UPURL2=$(jget uploadUrl < "$BODY")
code -X PUT "$UPURL2" --data-binary @"$FIXTURE" > /dev/null
code -X POST "$SERVER/api/v1/share/$SHAREID2/confirm" -H 'Content-Type: application/json' -d '{}' > /dev/null
code -X POST "$SERVER/api/v1/session/$SID/upload-complete" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ul_bob' \
     -d "{\"fileId\":\"$FILEID2\",\"shareId\":\"$SHAREID2\"}" > /dev/null

# since=<uploadedAt of #1> must return #2 and NOT #1 again.
ST=$(code "$SERVER/api/v1/session/$SID/files?since=$UPAT")
SEEN2=$(python3 -c "import json;d=json.load(open('$BODY'));print(sum(1 for f in d.get('files',[]) if f['fileId']=='$FILEID2'))")
RESEEN1=$(python3 -c "import json;d=json.load(open('$BODY'));print(sum(1 for f in d.get('files',[]) if f['fileId']=='$FILEID'))")
if [ "$SEEN2" = "1" ] && [ "$RESEEN1" = "0" ]; then ok "watermark strictly-new only"
else bad "watermark broken new=$SEEN2 replayed=$RESEEN1"; fi

echo "=== S-U5: cross-participant visibility + byte identity ==="
ST=$(code "$SERVER/api/v1/session/$SID/files" -H 'X-Uplink-Username: ul_bob')
BOB_SEES=$(FILEID="$FILEID" BODY="$BODY" python3 -c 'import json,os;d=json.load(open(os.environ["BODY"]));fid=os.environ["FILEID"];fs=[f for f in d.get("files",[]) if f["fileId"]==fid];print("UPLOADED" if fs and fs[0]["status"]=="UPLOADED" else "")')
[ "$BOB_SEES" = "UPLOADED" ] && ok "bob lists alice's file as UPLOADED" || bad "bob view wrong ($BOB_SEES)"

ST=$(code -X POST "$SERVER/api/v1/session/$SID/download/$FILEID" \
     -H 'Content-Type: application/json' -H 'X-Uplink-Username: ul_bob' -d '{}')
DLURL=$(jget downloadUrl < "$BODY")
[ "$ST" = "200" ] && [ -n "$DLURL" ] && ok "presigned download authorized" || bad "dl status=$ST body=$(head -c 120 "$BODY")"
curl -s --max-time 30 -o "$GOT" "$DLURL"
if cmp -s "$FIXTURE1" "$GOT"; then ok "round-trip byte-identical"; else bad "payload mismatch after download"; fi

rm -f "$BODY" "$FIXTURE" "$FIXTURE1" "$GOT"
echo "RESULT: PASS=$PASS FAIL=$FAIL"
[ $FAIL -eq 0 ]
