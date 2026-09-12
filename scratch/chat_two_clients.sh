#!/usr/bin/env bash
# Two-client headless chat E2E: creator + joiner, cross-delivery over the
# real P2P stack (direct line or inbox fallback), clean exits, and room-gone
# verification via API afterwards.
#
# New semantics (Vercel-native architecture): no system join messages, no
# server transcript, no message polling endpoint. Delivery is P2P E2E with
# inbox fallback; the room vanishes when the last member leaves.
set -u
cd "$(dirname "$0")/.." || exit 1
BIN="${BIN:-$(pwd)/cli/build/uplink}"
SERVER="${SERVER:-http://localhost:3000}"
T="$(mktemp -d "${TMPDIR:-/tmp}/chattest.XXXXXX")"
PASS=0; FAIL=0
ok()  { echo "  [PASS] $1"; PASS=$((PASS+1)); }
bad() { echo "  [FAIL] $1"; FAIL=$((FAIL+1)); }

pkill -f 'uplink create session' 2>/dev/null
pkill -f "uplink join " 2>/dev/null

mkfifo "$T/a.in"

# Creator: username first, held-open FIFO doubles as chat stdin.
( UPLINK_CHAT_PLAIN=1 "$BIN" create session --server "$SERVER" < "$T/a.in" > "$T/a.out" 2>&1 ) &
CREATOR=$!

exec 3> "$T/a.in"          # holds the pipe open for the whole session
printf 'alice\n' >&3       # answer the username prompt

KEY=""
for _ in $(seq 1 40); do
    KEY=$(grep -oP '(?<=KEY: )\d{6}' "$T/a.out" 2>/dev/null | head -1)
    [ -n "$KEY" ] && break
    sleep 0.25
done
if [ -z "$KEY" ]; then echo "[FAIL] no session key minted"; exec 3>&-; kill $CREATOR 2>/dev/null; exit 1; fi
echo "key=$KEY"
sleep 1.5

# Joiner: username + scripted conversation.
( printf 'bob\n'
  sleep 6; printf 'hi alice!\n'
  sleep 6
  sleep 2; printf '/exit\n' ) | UPLINK_CHAT_PLAIN=1 "$BIN" join "$KEY" --server "$SERVER" > "$T/b.out" 2>&1 &
JOINER=$!

sleep 8; printf 'hello from alice\n' >&3
sleep 8
sleep 2; printf '/exit\n' >&3       # alice leaves LAST -> she ends the room
for _ in $(seq 1 48); do
    grep -qE "You left|Disconnected" "$T/a.out" && break
    sleep 0.25
done
wait $JOINER 2>/dev/null || true
exec 3>&-

chk() { grep -q "$2" "$1" && ok "$3" || bad "$3"; }

chk "$T/a.out" "Connected to session $KEY as 'alice'" "alice connected"
chk "$T/b.out" "Connected to session $KEY as 'bob'"   "bob connected"
chk "$T/b.out" "hello from alice"                     "cross-delivery A→B (P2P or inbox fallback)"
chk "$T/a.out" "hi alice!"                            "cross-delivery B→A (P2P or inbox fallback)"
grep -qP "Online \(2\): .*alice.*bob|Online \(2\): .*bob.*alice" "$T/b.out" \
    && ok "connect banner lists both users" || bad "roster incomplete at connect"
chk "$T/b.out" "You left the session."                "bob clean exit"
grep -qE "You left the session." "$T/a.out" \
    && ok "alice exits last (ends room)" || bad "alice end state"

# Room must be GONE once everyone left (rooms live till empty, never by timeout).
CODE=$(curl -s --max-time 15 -o /dev/null -w '%{http_code}' \
    -X POST "$SERVER/api/v1/session/$KEY/join" \
    -H 'Content-Type: application/json' \
    -d '{"username":"latecomer","pubkey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}')
[ "$CODE" = "404" ] && ok "room destroyed after last leave (404)" \
    || bad "expected 404 for destroyed room, got $CODE"

echo "RESULT: PASS=$PASS FAIL=$FAIL (artifacts in $T)"
[ $FAIL -eq 0 ]
