# syntax=docker/dockerfile:1
#
# Uplink-Delta web app (Next.js) — self-hosted image for the EC2 box.
# Requires `output: "standalone"` in next.config.ts (set for this repo).
#
# Build:  docker build -t uplink-delta .
# Run:    see docker-compose.yml (env_file: .env, fronted by Caddy).

# ── Stage 1: deps — install node_modules from the lockfile ──────────────────
FROM node:20-alpine AS deps
RUN apk add --no-cache libc6-compat
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

# ── Stage 2: builder — produce .next/standalone + .next/static ──────────────
FROM node:20-alpine AS builder
RUN apk add --no-cache libc6-compat
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
# Env validation runs at request time, not build time; no secrets baked in.
# Generate public/graph.json (served by /graph) before the Next build.
RUN npm run graph:gen
RUN npm run build

# ── Stage 3: runner — minimal runtime, non-root ─────────────────────────────
FROM node:20-alpine AS runner
WORKDIR /app

ENV NODE_ENV=production \
    PORT=3000 \
    HOSTNAME=0.0.0.0 \
    NEXT_TELEMETRY_DISABLED=1

RUN addgroup --system --gid 1001 nodejs \
    && adduser --system --uid 1001 --ingroup nodejs nextjs

# Standalone bundle ships its own node_modules subset; static assets and the
# generated public/ (graph.json for /graph) sit outside it and must be copied
# alongside.
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static
COPY --from=builder --chown=nextjs:nodejs /app/public ./public

USER nextjs
EXPOSE 3000

# /api/v1/speedtest is unauthenticated (rate-limited: 10 req / 5 min / IP).
# 60s interval = 5 req / 5 min, safely under that cap.
HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}/api/v1/speedtest" || exit 1

CMD ["node", "server.js"]
