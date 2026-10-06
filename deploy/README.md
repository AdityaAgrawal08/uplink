# Deploying Uplink-Delta on a single AWS EC2 box

Runbook for hosting the Next.js web/signaling plane on one Amazon Linux 2023
instance with Docker. **Compute-only move:** Upstash Redis, MongoDB Atlas and
Cloudflare R2 stay exactly as they are today (all endpoint env vars
unchanged). Vercel is **not deleted** — it remains live as a rollback path.

Target box: `52.7.217.135` (Elastic IP), HTTP only for now. TLS is deferred
until a domain exists; Caddy already sits in front, so cutover is one edit.

```
Internet ──:80/:443──▶ caddy (caddy:2-alpine)
                          │  reverse_proxy app:3000
                          ▼
                        app (Next.js standalone, 127.0.0.1:3000 on the host)
                          │
                          ├── Upstash Redis   (unchanged)
                          ├── MongoDB Atlas   (unchanged)
                          └── Cloudflare R2   (unchanged)
```

Files: `Dockerfile`, `.dockerignore`, `docker-compose.yml`, `Caddyfile`
(repo root); `deploy/uplink-cleanup.{service,timer}` (this directory).

---

## 0. Prerequisites

- Amazon Linux 2023 EC2 instance (`t3.micro` is enough to run; see §11 for
  the build-memory caveat), 30 GB gp3.
- Elastic IP `52.7.217.135` associated with the instance.
- Security group inbound: `22` from your IP, `80` and `443` from
  `0.0.0.0/0`. **Do not expose port 3000** — the app publishes only on
  `127.0.0.1` and Caddy reaches it over the compose network.
- The managed services are already provisioned (the same ones Vercel used).
  If Atlas has an IP allowlist, add the Elastic IP to it (§7).

## 1. Install Docker (+ Compose) on AL2023

```sh
sudo dnf update -y
sudo dnf install -y docker
sudo systemctl enable --now docker
sudo usermod -aG docker ec2-user        # log out / back in for the group

# AL2023's docker package does NOT include the compose plugin (there is no
# docker-compose-plugin RPM in its repos) — install the official plugin:
sudo mkdir -p /usr/libexec/docker/cli-plugins
sudo curl -SL "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-$(uname -m)" \
  -o /usr/libexec/docker/cli-plugins/docker-compose
sudo chmod +x /usr/libexec/docker/cli-plugins/docker-compose
docker compose version
```

## 2. Clone and configure

```sh
sudo mkdir -p /opt/uplink-delta && sudo chown ec2-user:ec2-user /opt/uplink-delta
git clone https://github.com/AdityaAgrawal08/uplink-delta /opt/uplink-delta
cd /opt/uplink-delta
# deploy/aws-ec2 until this branch is merged; then main.
git checkout deploy/aws-ec2

cp .env.example .env
chmod 600 .env
$EDITOR .env
```

The systemd units and compose file assume the repo lives at
`/opt/uplink-delta`; adjust the unit paths if you clone elsewhere.

### Required `.env` values

| Variable | Value / where it comes from |
|---|---|
| `NODE_ENV` | `production` **(change from the template's `development`)** so `lib/env.ts` hard-fails instead of warning/mock-fallback. |
| `HOSTNAME` | `0.0.0.0` (already in the template) — standalone server bind address. |
| `PORT` | `3000` (already in the template). |
| `MONGODB_URI` | Atlas connection string — same cluster as Vercel. Copy from the Vercel project env, or create/rotate a DB user (readWrite on the `r2-uplink` DB). Ensure the EIP is allowed in Atlas (§7). |
| `UPSTASH_REDIS_REST_URL`, `UPSTASH_REDIS_REST_TOKEN` | Upstash console → the same database used today. |
| `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET_NAME`, `R2_ENDPOINT_URL` | Cloudflare R2 → manage API tokens → the same bucket/account as today. Endpoint format unchanged. |
| `IP_ANONYMIZATION_SECRET` | Same value as Vercel (changing it only invalidates previously hashed IPs). |
| `TRUST_PROXY` | `true` — **required behind Caddy**: Caddy rewrites `X-Forwarded-For` with the real client IP and the app only honors it when this is set. |
| `CRON_SECRET` | Generate with `openssl rand -hex 32`. Used by the daily cleanup timer; also accepted by `GET/POST /api/v1/cleanup` (share cleanup) as `Authorization: Bearer`. |
| `APP_URL` | `http://52.7.217.135:3000` — **temporary until a domain exists** (only used to build dev mock-R2 URLs; set it anyway). |
| `ADMIN_API_KEY` | Optional. Only needed for `GET /api/v1/admin/quota` / manual share cleanup. Generate with `openssl rand -hex 32` if used. |
| `LOG_LEVEL`, `MAX_UPLOAD_SIZE`, `DEFAULT_EXPIRY`, `UPLOAD_URL_EXPIRY`, `DOWNLOAD_URL_EXPIRY` | Keep the template defaults or copy the Vercel values. |

Copying existing values: Vercel dashboard → Project → Settings → Environment
Variables (or `vercel env pull .env.vercel` with the Vercel CLI). Never commit
`.env` — it is gitignored and excluded from the Docker build context.

## 3. First build and start

```sh
cd /opt/uplink-delta
docker compose up -d --build
docker compose ps                     # app healthy, caddy running
docker compose logs -f app
```

Build-memory note: `next build` can exceed the 1 GB RAM of a `t3.micro`.
AL2023 ships zram swap by default; if the build is OOM-killed, add a disk
swapfile (or build on a larger instance for the day):

```sh
sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

## 4. Smoke tests

All of these run from your workstation unless noted.

```sh
# 1) Speedtest through Caddy: 200 + exactly 256 KB.
curl -fsS -o /dev/null -w '%{http_code}\n' http://52.7.217.135/api/v1/speedtest   # 200
curl -fsS http://52.7.217.135/api/v1/speedtest | wc -c                            # 262144

# 2) Session create + join (signaling routes take username + base64 X25519
#    pubkey; create/join need no request signature).
PUBKEY=$(openssl rand -base64 32)
CODE=$(curl -fsS -X POST http://52.7.217.135/api/v1/session/create \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"smoke-host\",\"pubkey\":\"$PUBKEY\"}" \
  | sed -E 's/.*"sessionId":"([0-9]+)".*/\1/')
echo "session=$CODE"
curl -fsS -X POST "http://52.7.217.135/api/v1/session/$CODE/join" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"smoke-peer\",\"pubkey\":\"$PUBKEY\"}"

# 3) Upload → confirm → download round-trip through R2 + Mongo.
cd /tmp && printf 'uplink smoke test\n' > smoke.txt
SIZE=$(stat -c%s smoke.txt); HASH=$(sha256sum smoke.txt | cut -d' ' -f1)
INIT=$(curl -fsS -X POST http://52.7.217.135/api/v1/share/init \
  -H 'Content-Type: application/json' -H "Idempotency-Key: smoke-$(date +%s)" \
  -d "{\"filename\":\"smoke.txt\",\"mimeType\":\"text/plain\",\"size\":$SIZE,\"hashValue\":\"$HASH\",\"partsCount\":0}")
SHARE=$(printf '%s' "$INIT" | sed -E 's/.*"shareId":"([^"]+)".*/\1/')
UPURL=$(printf '%s' "$INIT" | sed -E 's/.*"uploadUrl":"([^"]+)".*/\1/')
curl -fsS -X PUT --data-binary @smoke.txt -H 'Content-Type: text/plain' "$UPURL" -o /dev/null
curl -fsS -X POST "http://52.7.217.135/api/v1/share/$SHARE/confirm" \
  -H 'Content-Type: application/json' -d '{}'
DL=$(curl -fsS -X POST "http://52.7.217.135/api/v1/share/$SHARE/authorize-download" \
  -H 'Content-Type: application/json' -d '{"preview":false}' \
  | sed -E 's/.*"downloadUrl":"([^"]+)".*/\1/')
curl -fsS "$DL" -o out.bin && sha256sum out.bin   # must equal $HASH
```

Best cross-check (exercises the whole stack end to end):

```sh
UPLINK_SERVER=http://52.7.217.135:3000 uplink send ./some_file
# then on another machine:
uplink receive <code-or-link> --server http://52.7.217.135:3000
```

## 5. Daily cleanup timer (replaces the Vercel cron)

`vercel.json`'s `0 3 * * *` cron pointed at `/api/v1/session/cleanup`.
That route is **unauthenticated** (Vercel crons send no credentials); the
systemd unit sends the `CRON_SECRET` bearer anyway so it stays valid if the
route is ever gated or repointed at `/api/v1/cleanup`, which *does* require
`CRON_SECRET`/`ADMIN_API_KEY`. Verify once installed:

```sh
sudo cp deploy/uplink-cleanup.service deploy/uplink-cleanup.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now uplink-cleanup.timer
systemctl list-timers uplink-cleanup.timer
sudo systemctl start uplink-cleanup.service          # run once now
journalctl -u uplink-cleanup.service -n 20           # expect cleanup JSON
```

The units run at **03:00 UTC** with `Persistent=true` and a 5-minute random
delay, and expect `.env` at `/opt/uplink-delta/.env`. Optional: add a second
`ExecStart` for `/api/v1/cleanup` (share cleanup, CRON_SECRET-gated) if you
want it on a schedule rather than only the opportunistic after()-hook runs.

## 6. What stays on managed services

- **Upstash Redis** — rate limits, rooms, idempotency. Nothing to do; a
  single always-on box is actually easier on it than serverless fan-out.
- **Cloudflare R2** — presigned PUT/GET, multipart. Endpoint env unchanged.
- **MongoDB Atlas** — shares, quotas, indexes. See §7.

## 7. Atlas note (already done)

Atlas is already provisioned for this project and needs no migration. The only
possible change: if the cluster uses **Network Access** IP allowlisting, add
the Elastic IP `52.7.217.135/32` (Vercel's serverless egress ranges are
allowlisted today). The connection string itself does not change.

## 8. Updating the deployment

```sh
cd /opt/uplink-delta
git pull
docker compose up -d --build
docker image prune -f        # optional: drop the previous image
```

## 9. Caddy domain cutover (later)

1. Point an `A` record (e.g. `uplink.example.com`) at `52.7.217.135`; keep
   inbound `80`/`443` open.
2. Edit `Caddyfile`: replace the `http:// {` site block with the commented
   domain block (or simply the bare domain as the site address). Caddy then
   provisions and auto-renews a Let's Encrypt certificate and redirects HTTP
   → HTTPS by itself.
3. `docker compose restart caddy` and verify `https://uplink.example.com`.
4. Update `.env` `APP_URL=https://uplink.example.com`, then update the CLI
   default (`cli/config.go`) and ship a release; existing clients can opt in
   immediately with `uplink config set server https://uplink.example.com`.

## 10. Rollback (Vercel is still live)

- The Vercel project, `vercel.json` and its cron are untouched — reverting
  is configuration, not redeploying: `docker compose down` on the box if you
  want compute off; clients built before this change already default to
  `https://uplink-delta-xi.vercel.app`.
- To point current clients back: `uplink config set server
  https://uplink-delta-xi.vercel.app`.
- Do not delete the Vercel project until the domain cutover (§9) has been
  stable for a while.

## 11. Cost notes (free-tier aware)

- `t3.micro` + 30 GB gp3: covered by the classic 750 h/month free tier for
  one always-on instance (AWS moved newer accounts to a credit-based free
  tier — check your account's terms).
- **Elastic IP rule:** an EIP is free only while attached to a **running**
  instance. Stopping the instance or detaching the EIP starts hourly
  charges — keep it attached or release it.
- Upstash / Atlas / R2 remain on their own free tiers; this box adds no
  Redis/Mongo/storage cost.
- EC2 data transfer: first 100 GB/month egress free (shared across AWS
  services), then ~$0.09/GB. Downloads go through R2, so EC2 egress is
  mostly signaling traffic.
- EBS snapshots, additional EBS volumes and a domain/TLS (free via Caddy)
  are the only other potential line items.

## Troubleshooting

| Symptom | Check |
|---|---|
| `app` unhealthy | `docker compose logs app` — usually a missing/invalid env (503 "Backend not configured"). |
| Caddy returns 502 | `docker compose exec app wget -qO- http://127.0.0.1:3000/api/v1/speedtest`; make sure `depends_on` health passed. |
| Per-IP budgets look global | `TRUST_PROXY=true` missing in `.env` (Caddy sets XFF; the app ignores it otherwise). |
| Build OOM-killed | Add swap (§3) or build on a larger instance. |
| Cleanup timer failed | `journalctl -u uplink-cleanup.service`; the unit uses `curl -f`, so 4xx/5xx shows as a failure. |
