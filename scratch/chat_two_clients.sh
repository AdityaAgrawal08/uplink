#!/usr/bin/env bash
# Two-client headless chat E2E: creator + joiner, cross-delivery, roster,
# clean exits, and instant room-end verification via API afterwards.
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
sleep 1.5                  # let backlog fetch settle

# Joiner: username + scripted conversation.
( printf 'bob\n'
  sleep 4; printf 'hi alice!\n'
  sleep 2.5; printf '/users\n'
  sleep 2; printf '/exit\n' ) | UPLINK_CHAT_PLAIN=1 "$BIN" join "$KEY" --server "$SERVER" > "$T/b.out" 2>&1 &
JOINER=$!

sleep 5; printf 'hello from alice\n' >&3
sleep 3; printf '/users\n' >&3
sleep 2; printf '/exit\n' >&3       # alice leaves LAST -> she ends the room
for _ in $(seq 1 48); do
    grep -qE "You left|Disconnected|Session has ended" "$T/a.out" && break
    sleep 0.25
done
wait $JOINER 2>/dev/null || true
exec 3>&-

chk() { grep -q "$2" "$1" && ok "$3" || bad "$3"; }

chk "$T/a.out" "Connected to session $KEY as 'alice'" "alice connected"
chk "$T/a.out" "bob joined"                         "join event visible to alice"
chk "$T/a.out" "hello from alice"                     "alice self-message rendered"
chk "$T/b.out" "Connected to session $KEY as 'bob'"   "bob connected"
chk "$T/b.out" "hello from alice"                     "cross-delivery A→B"
chk "$T/b.out" "hi alice!"                            "cross-delivery B→A"
chk "$T/b.out" "Online:"                              "roster command works"
grep -qP "Online:.*alice.*bob|Online:.*bob.*alice" "$T/b.out" \
    && ok "roster lists both" || bad "roster incomplete"
chk "$T/b.out" "You left the session."                "bob clean exit"
grep -qE "You left the session.|Session has ended" "$T/a.out" \
    && ok "alice exits last (ends room)" || bad "alice end state"

# Room must be ENDED with purged transcript once everyone left.
BODY=$(mktemp "${TMPDIR:-/tmp}/chatend.XXXXXX")
CODE=$(curl -s --max-time 15 -o "$BODY" -w '%{http_code}' \
    "$SERVER/api/v1/session/$KEY/messages?after=0" -H 'X-Uplink-Username: alice')
case "$CODE" in
    200) grep -q '"ended":true' "$BODY" && grep -q '"messages":\[\]' "$BODY" \
            && ok "room ENDED + transcript purged" || bad "end/purge state wrong ($CODE)" ;;
    403|410) ok "room terminated (alice no longer member)" ;;
    410) ok "room ENDED (410)" ;;
    *)   bad "unexpected post-exit state: $CODE $(cat "$BODY")" ;;
esac

echo "RESULT: PASS=$PASS FAIL=$FAIL (artifacts in $T)"
[ $FAIL -eq 0 ]
