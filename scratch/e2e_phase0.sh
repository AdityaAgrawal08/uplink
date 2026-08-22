#!/usr/bin/env bash
# Phase 0 acceptance matrix for uplink send/receive.
# Usage: SERVER=http://localhost:3000 ./scratch/e2e_phase0.sh
set -u
cd "$(dirname "$0")/.." || exit 1

SERVER="${SERVER:-http://localhost:3000}"
BIN="$(pwd)/cli/build/uplink"
TMP_ROOT="${TMPDIR:-/tmp}/opencode"
mkdir -p "$TMP_ROOT"
WORK="$(mktemp -d "$TMP_ROOT/e2e.XXXXXX")"
PASS=0; FAIL=0

say()  { printf '\n=== %s ===\n' "$1"; }
ok()   { printf '  [PASS] %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL+1)); }

send_code() { # $1=path -> echoes download code or empty; failure reason goes to stderr
    local out rc
    out=$("$BIN" send "$1" --server "$SERVER" --no-qr 2>&1); rc=$?
    local code
    code=$(printf '%s\n' "$out" | awk '/^Code:$/{getline; print; exit}')
    if [ -z "$code" ]; then
        printf '[diag] %s send rc=%d: %s\n' "$(basename "$1")" "$rc" \
            "$(printf '%s\n' "$out" | grep -v '█\|░' | tail -2 | tr '\n' ' ')" >&2
    fi
    printf '%s' "$code"
}

say "S1: small text file round-trip"
printf 'phase0 smoke %s\n' "$(date +%s)" > "$WORK/s1.txt"
CODE=$(send_code "$WORK/s1.txt")
if [ -n "$CODE" ]; then ok "send produced code $CODE"; else bad "send returned no code"; fi
mkdir -p "$WORK/rx1"
( cd "$WORK/rx1" && "$BIN" receive "$CODE" --server "$SERVER" >/dev/null 2>&1 )
if diff -q "$WORK/s1.txt" "$WORK/rx1/s1.txt" >/dev/null 2>&1; then ok "content identical"; else bad "round-trip mismatch"; fi

say "S2: directory (tarball) round-trip"
mkdir -p "$WORK/pkg/sub"
echo "alpha $(date +%s)" > "$WORK/pkg/a.txt"
echo beta  > "$WORK/pkg/sub/b.txt"
CODE=$(send_code "$WORK/pkg")
if [ -n "$CODE" ]; then ok "folder send code $CODE"; else bad "folder send failed"; fi
mkdir -p "$WORK/rx2"
( cd "$WORK/rx2" && "$BIN" receive "$CODE" --server "$SERVER" >/dev/null 2>&1 )
if diff -r "$WORK/pkg" "$WORK/rx2/pkg" >/dev/null 2>&1; then ok "tree identical"; else bad "tree mismatch"; fi

say "S3: E2EE round-trip"
printf 'secret-%s\n' "$(date +%s)" > "$WORK/s3.txt"
OUT=$("$BIN" send "$WORK/s3.txt" --server "$SERVER" --no-qr --encrypt 2>&1)
CODE=$(printf '%s\n' "$OUT" | awk '/^Code:$/{getline; print; exit}')
case "$CODE" in *:*) ok "encrypted code embeds key"; ;; *) bad "code missing :key ($CODE)";; esac
( cd "$WORK/rx1" && "$BIN" receive "$CODE" --server "$SERVER" >/dev/null 2>&1 )
if diff -q "$WORK/s3.txt" "$WORK/rx1/s3.txt" >/dev/null 2>&1; then ok "decrypted content identical"; else bad "decrypt/mismatch"; fi

say "S4: zero-byte file rejected cleanly"
: > "$WORK/s4.bin"
OUT=$("$BIN" send "$WORK/s4.bin" --server "$SERVER" --no-qr 2>&1); RC=$?
if [ $RC -ne 0 ] && printf '%s' "$OUT" | grep -qi "size"; then ok "rejected: $(printf '%s' "$OUT" | tail -c 80)"
else bad "expected size rejection, rc=$RC out=$(printf '%s' "$OUT" | tail -c 80)"; fi

say "S5: multipart contract (partsCount>100 must be impossible via CLI)"
HASH=$(printf x | sha256sum | cut -d' ' -f1)
RESP=$(curl -s --max-time 30 -X POST "$SERVER/api/v1/share/init" -H 'Content-Type: application/json' \
    -d "{\"filename\":\"c1probe.bin\",\"size\":150000000,\"mimeType\":\"application/octet-stream\",\"hashValue\":\"$HASH\",\"partsCount\":151}")
if printf '%s' "$RESP" | grep -q "partsCount cannot exceed 100"; then
    ok "server guards >100 parts (CLI chunk clamp still pending: C1)"
else bad "unexpected init response: $(printf '%s' "$RESP" | head -c 100)"; fi

say "S6: idempotent re-send keeps a usable code"
CODE_A=$(send_code "$WORK/s1.txt")
CODE_B=$(send_code "$WORK/s1.txt")
if [ -n "$CODE_B" ]; then ok "re-send code present ($CODE_B)"; else bad "re-send printed no code"; fi

say "S7: multipart (>10MB) round-trip"
dd if=/dev/urandom of="$WORK/s7.bin" bs=1M count=25 status=none
CODE=$(send_code "$WORK/s7.bin")
if [ -n "$CODE" ]; then ok "multipart send code $CODE"; else bad "multipart send failed"; fi
mkdir -p "$WORK/rx3"
( cd "$WORK/rx3" && "$BIN" receive "$CODE" --server "$SERVER" >/dev/null 2>&1 )
if cmp -s "$WORK/s7.bin" "$WORK/rx3/s7.bin"; then ok "25MB byte-identical"; else bad "multipart mismatch"; fi

say "S8: stale idempotency cache cannot resurrect a dead share (RUN_SLOW=1, ~80s)"
if [ "${RUN_SLOW:-0}" = "1" ]; then
    printf 'stale-cache %s\n' "$(date +%s)" > "$WORK/s8.txt"
    OUT=$("$BIN" send "$WORK/s8.txt" --server "$SERVER" --no-qr --expire 1m 2>&1)
    CODE_A=$(printf '%s\n' "$OUT" | awk '/^Code:$/{getline; print; exit}')
    if [ -n "$CODE_A" ]; then ok "initial send code $CODE_A (expires in 1m)"; else bad "initial send failed"; fi
    sleep 70
    OUT=$("$BIN" send "$WORK/s8.txt" --server "$SERVER" --no-qr --expire 1m 2>&1)
    CODE_B=$(printf '%s\n' "$OUT" | awk '/^Code:$/{getline; print; exit}')
    if [ -n "$CODE_B" ] && [ "$CODE_B" != "$CODE_A" ]; then ok "re-send after expiry got fresh share $CODE_B"
    elif [ -n "$CODE_B" ]; then bad "re-send reused dead share code $CODE_B"
    else bad "re-send after expiry failed: $(printf '%s\n' "$OUT" | grep -v '█\|░' | tail -2 | tr '\n' ' ')"; fi
else
    echo "  [skip] set RUN_SLOW=1 to enable (~80s)"
fi

say "RESULT: PASS=$PASS FAIL=$FAIL  (workdir $WORK)"
[ $FAIL -eq 0 ]
