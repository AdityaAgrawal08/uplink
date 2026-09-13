# Deploying uplink (Vercel)

The chat signaling plane needs **one** shared backend. Without it, every
serverless isolate gets its own empty memory: rooms are created (201) and
then 404 on the very next request, and no two users ever meet. The server
fails loud (500/503) in that state — if you see rooms vanishing instantly,
check this checklist first.

## 1. Create a free Redis (2 minutes)

1. Go to [upstash.com](https://upstash.com) → Console → Create Database.
2. Pick any region, copy the **REST URL** and **REST TOKEN** (not the
   TCP endpoint — serverless uses the HTTP API).

## 2. Set Vercel environment variables

Project → Settings → Environment Variables (Production + Preview):

| Variable | Value | Why |
|---|---|---|
| `UPSTASH_REDIS_REST_URL` | `https://…upstash.io` | Shared room/signal/inbox store. **Missing or unreachable = total outage** (fail-loud by design, never silent split-brain). |
| `UPSTASH_REDIS_REST_TOKEN` | `A…x` | Same as above. |
| `IP_ANONYMIZATION_SECRET` | any random 32+ char string | HMAC key for rate-limit IP hashes. |
| `MONGODB_URI` | Atlas/local URI | Only for standalone `uplink send/receive` shares. Skip if unused. |
| R2_* (`R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_ENDPOINT_URL`, `R2_BUCKET_NAME`) | Cloudflare R2 | Only for standalone shares. Skip if unused. |

Then **redeploy** (env changes need a new deployment).

Do NOT set `ALLOW_MOCK_REDIS=true` in production. It exists only for CI
and throwaway environments: mock mode keeps state per isolate, so rooms
created on one request are invisible on the next.

## 3. Verify the deploy (60 seconds)

```bash
S=https://YOUR-APP.vercel.app
PUB=$(python3 -c "import base64,os;print(base64.b64encode(os.urandom(32)).decode())")
SID=$(curl -s -X POST "$S/api/v1/session/create" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"check\",\"pubkey\":\"$PUB\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('sessionId',''))")
echo "room=$SID"
# This must print 200. A 404 here means no shared Redis (see step 1-2).
curl -s -o /dev/null -w 'heartbeat:%{http_code}\n' \
  -X POST "$S/api/v1/session/$SID/heartbeat" \
  -H 'Content-Type: application/json' -H 'X-Uplink-Username: check' -d '{}'
```

- `heartbeat:200` → backend healthy; two terminals on this server will meet.
- `heartbeat:404` right after create → requests are landing on isolates
  without shared state → recheck step 1–2 and redeploy.
- `500` → read the message: it names the missing variable.

## 4. Point the CLI at it

```bash
uplink config set server https://YOUR-APP.vercel.app
```

Both chatters must use the **same** server URL — same code on two backends
(e.g. one tab on `localhost:3000`, one on Vercel) is two different rooms
by construction.

## 5. Client tunables (optional)

- `UPLINK_STUN`: comma-separated STUN servers for WebRTC NAT traversal.
  Default: `stun:stun.l.google.com:19302`. Example:
  `UPLINK_STUN="stun:stun.l.google.com:19302" uplink ...`
- The `sharp` / `unrs-resolver` lines in the Vercel build log
  (`npm warn allow-scripts ...`) are informational: they name transitive
  Next.js dependencies with install scripts and do not fail or affect the
  build. No action needed.
