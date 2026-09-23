# UPLINK-Delta — Documentation

> Describes the repository as of the `UI` branch tip (`3765ba5`).
> Companion file: `FILEMAP.md` (per-file index of the whole tree).
> `main` may still contain the pre-cleanup video-calling code; this document
> describes `UI`, which is audio-only.

## 1. What this project is

Uplink-Delta is an **ephemeral, end-to-end-encrypted file-sharing and P2P
chat system** with three parts:

| Part | Location | Role |
|---|---|---|
| CLI | `cli/` (Go, module `github.com/AdityaAgrawal08/uplink-delta/cli`, go 1.27) | File send/receive (cloud, LAN, WAN, P2P mesh), terminal chat TUI, voice calls, QR, self-update |
| Web signaling plane | `src/` (Next.js + TypeScript) | Room signaling (rooms, presence, inbox, WebRTC rendezvous), file-share metadata + R2 presigned URLs, quota, share preview pages. Deployed on Vercel serverless |
| Shelved relay | `server/` (Go, separate module, go 1.23) | **SHELVED, not used in production.** Standalone WebSocket relay kept as documented fallback for an always-on host (see `server/README.md`) |

Core promise: **message/file contents are opaque to the server** (E2E via
Noise_XX over P2P, AES-GCM boxes via inbox fallback, AES-GCM file chunks).
The server sees only metadata (usernames, pubkeys, presence, sizes, timing).

## 2. Architecture and data planes

- **Signaling (server):** Next.js API routes under `src/app/api/v1/session/*`
  backed by Upstash Redis (`src/lib/rooms.ts`, `src/lib/redis.ts`). Rooms are
  6-digit codes, presence heartbeat, per-user signal queues (WebRTC
  offer/answer) and inbox queues (offline E2E boxes), roles
  creator/admin/member + bans, epochs.
- **File shares (server):** `src/app/api/v1/share/*` backed by MongoDB
  (`src/lib/mongodb.ts`) + Cloudflare R2 (`src/lib/r2.ts`) with presigned
  PUT/GET, multipart resume, quota ledger (`src/lib/quota.ts`), share preview
  pages (`src/app/share/[id]`). Dev-only mock R2 routes exist
  (`mock-r2-upload`, `mock-r2-download`).
- **Chat data plane (CLI):** per-peer `Noise_XX` sessions (X25519 /
  ChaCha20-Poly1305 / SHA256, `cli/p2p_noise.go`) over a WebRTC mesh
  (`cli/p2p_mesh.go`, pion). Engine (`cli/p2p_engine.go`) adds frames,
  inbox fallback, file chunking/reassembly, delivery acks + retries,
  roster/heartbeat beat loop.
- **Inbox fallback:** pairwise static-static X25519 → HKDF-SHA256 →
  AES-256-GCM boxes (`cli/p2p_box.go`), deposited/fetched via the server
  inbox routes. No forward secrecy on this path by design.
- **Voice calls (CLI, audio-only):** publish model in `cli/media_call.go`.
  `/audio` toggles mic publish to DM peer or room; solo use runs a local
  preview; late joiners are admitted via `PublishTo`. No ringing/accepting.
  Mic capture (`cli/media_audio.go`, ALSA/pulse/ffmpeg/test backends), Opus
  encode, jitter buffer (`cli/media_jitter.go`), speaker playout.
- **LAN transfers:** mDNS discovery (`cli/lan/discovery.go`) + ephemeral-TLS
  HTTPS server (`cli/lan/server.go`, `cli/lan/tls.go`).
- **WAN transfers:** libp2p + Kad-DHT rendezvous (`cli/wan/wan.go`).
- **Passwords:** room/share passwords are Argon2id-hashed server-side
  (`src/lib/crypto.ts`); never stored in clear.

## 3. Repository layout

```text
cli/            Go CLI (chat TUI, media, P2P engine, transfers, update)
cli/lan/        mDNS discovery + TLS file server + LAN download client
cli/wan/        libp2p DHT WAN transfer
cli/pkg/crc64   NVMe CRC64 helper (+ test)
cli/pkg/tarball Directory pack/unpack with slip + bomb limits (+ test)
src/app/api/v1  19 Next.js API routes (admin, cleanup, mock-r2, session, share, speedtest)
src/app/share   Share landing/preview pages
src/components  FilePreview (QR, password, download), SyntaxHighlighter
src/lib         auth, rooms, redis (+mock), r2 (+mock), crypto, quota, mongodb,
                env, api-utils, crc64 (+ 3 vitest suites)
server/         SHELVED Go WebSocket relay (own go.mod) — not deployed
scratch/        Dev e2e shell scripts + R2/quota debug probes (44K, untracked-tooling)
packaging/      Arch Linux PKGBUILD
.github/workflows  ci.yml (lint/test/build/e2e gate), release.yml (GoReleaser),
                    npm.yml (tag-gated npm publish)
Makefile        build/install/clean/5-target release tarballs
install.sh / install.ps1  checksum-verified installers (fail closed)
DEPLOY.md       Vercel/Redis/R2 setup + verification steps
docs/           This documentation + FILEMAP.md
```

## 4. CLI guide

### Install

```sh
# released binary (checksums verified, fail closed)
/bin/sh install.sh            # Linux/macOS → /usr/local/bin or ~/.local/bin
./install.ps1                 # Windows
# from source
make build && sudo make install
```

### Commands (`uplink --help`)

| Command | Purpose |
|---|---|
| `(no args)` | Open CREATE/JOIN landing TUI |
| `send <path> [--encrypt] [--lan] [--wan] [--password]` | Share file/dir via cloud (default), LAN peer, or DHT WAN |
| `receive <code\|URL>[:KEY] [dest]` | Download; `:KEY` suffix decrypts E2EE shares |
| `create session` | Mint a chat room code |
| `join` | Join chat (prompts code/username) |
| `config get\|set\|ls` | Layered config (defaults < file < env) |
| `version` | Print version/commit/date (bake via `-X main.version=…`) |
| `update` | Self-update from GitHub releases (checksums.txt verified, fail closed) |

Flags support interspersed order (`normalizeFlagOrder`); `--` terminator is
stripped, so dash-prefixed filenames need `./-file` workaround.

### Chat TUI

Three regions: left chat list (search + General room + DM threads), center
transcript + composer, no right panel (removed). Keybindings: `Enter` send,
`Ctrl+K` command palette, `Ctrl+L` clear, `↑↓` scroll, mouse supported.

Slash commands: `/help`, `/upload`, `/download`, `/audio`, `/kick`,
`/admin`, `/unadmin` (last three role-gated: creator/admin only; `/admin`
creator-only). Moderation needs a live server; failures surface on the
status line.

### Environment variables (CLI)

| Variable | Effect |
|---|---|
| `UPLINK_SERVER` | Signaling base URL (overrides config) |
| `UPLINK_MIC` | Mic backend: unset = auto, `test` = synthetic sine, else exact ALSA/ffmpeg device |
| `UPLINK_SPEAKER_OUT` | Test hook: raw PCM capture path for speaker |
| `UPLINK_STUN` | Comma-separated STUN servers for WebRTC |
| `UPLINK_EXPIRY`, `UPLINK_DOWNLOAD_DIR`, `UPLINK_LAN_PORT`, `UPLINK_ADAPTIVE_CHUNKS`, `UPLINK_SHOW_QR` | Misc behavior overrides |
| `GITHUB_TOKEN` | Private-repo self-update auth |

`UPLINK_CAMERA` / video-style variables are **retired** (video calling
removed). Config lives in `~/.uplink/` (identity `0600`, rest `0700/0600`).

## 5. Web app guide

- Dev: `npm install`, `npm run dev`. Required env is validated at runtime
  (`src/lib/env.ts`); see `.env.example`. Without Redis/Mongo/R2 the app
  runs against in-memory mock backends (dev only).
- Key routes: `POST /api/v1/session/create`, `.../[sessionId]/{join,
  heartbeat, signal (GET+POST), inbox (GET+POST), inbox/ack, leave, kick,
  admin}`, `GET|POST /api/v1/session/cleanup` (cron), `POST
  /api/v1/share/{init, [id], [id]/confirm, [id]/parts, [id]/authorize-download,
  [id]/preview-text}`, `GET /api/v1/admin/quota` (bearer `ADMIN_API_KEY`),
  `GET /api/v1/speedtest`, mock R2 routes (dev only).
- Identity model: signaling callers present `X-Uplink-Username` (no bearer
  token — known limitation, see audit); file shares are bearer
  (`shareId`/`downloadCode`/`uploadId`); admin uses `ADMIN_API_KEY`.
- Deploy: Vercel (`vercel.json`: region, function timeouts, daily session
  cleanup cron). See `DEPLOY.md`.

## 6. Build, test, CI, release

```sh
cd cli && go build ./... && go vet . && go test -count=1 .   # ~100s
cd cli && go test -race -count=1 .                            # ~115s, CI runs -race
npx tsc --noEmit && npm run lint && npx vitest run            # web: 39 tests
make release                                                  # 5-target tarballs
```

CI (`ci.yml`, runs on every push): Go vet + `go test -race ./...` +
6-target cross-builds; web lint + typecheck + unit + build; e2e matrix
(real Mongo service container + mock R2). Single `pipeline` gate for branch
protection. Per-run binary artifacts are intentionally NOT uploaded
(quota); only failure-only `e2e-server-log` (1-day retention).
Release (`release.yml`, tags `v*`): GoReleaser → versioned tarballs +
`checksums.txt`, which both installers and `uplink update` verify.

## 7. Protocols and crypto (summary)

- Peer identity: long-lived X25519 key, `~/.uplink/identity` (`0600`).
- Live chat/files: `Noise_XX` handshake per peer pair over WebRTC data
  channels; frames are JSON (`p2p_proto.go`: chat/ack/file-meta/chunk...),
  per-peer send serialization preserves nonce order; decrypt failure tears
  the session down (fail closed).
- Offline: AES-256-GCM boxes via server inbox, at-least-once delivery with
  sender retries, consumer-side dedup (`seenMsg` 5000), fetch-then-ACK.
- Voice: Opus over UDP Noise sessions, jitter buffer, mic-level gate.
- Share links: `id:KEY` (AES-256 file key, never sent to server in clear);
  QR encodes the receive command.

## 8. Implementation conventions (read before changing code)

- **TUI exact-row contract:** every painted frame is exactly `H` rows ×
  `≤W` cols; `layout` (chat_tui.go) is the single geometry truth shared by
  paint, viewport sync, and mouse hit-test. Keep the three in lockstep;
  `scrollbars_test.go` and `TestResizeSweep*` enforce it.
- **Status line vs transcript:** user-facing notices (command results,
  usage, moderation outcomes, engine/media events, server-down alert) go to
  the **status line**, never as transcript rows. Only real peer messages,
  own echoes (with delivery annotations), and file cards paint the
  transcript.
- **Server-down alert:** `isServerDown(err)` (transport errors + 502/503/504
  only; mesh/WebRTC-local failures excluded) → `serverDownMsg` held by the
  2s roster tick via engine `lastBeatErr`, cleared on recovery.
- **Tests:** colocated `*_test.go`, `newFilterScreen` (bare UI) vs
  `wireTestEngine` (fake `httptest` signaling server); `tea` program loop
  is driven via `Update` messages, never run headless in tests.
- **Go style:** `nil`-then-close channel teardown, fail-closed validation,
  bounded queues/channels everywhere new code is added.
- **Branch state:** active work is on `UI` (≈25 commits ahead of
  `origin/audio-video-integration`); `main` is the last merged base.
  Push branches with `git push --all origin`; the maintainer merges.

## 9. Known limitations (audit-backed, not yet fixed)

Member auth is header-only (no join tokens); quota `confirm` trusts
declared size; LAN discovery is mDNS-trust-on-first-use; WAN receive path
is non-functional; unbounded-download and P2P caps are partially bounded
(chat assembly capped, cloud/LAN/WAN streams are not); no forward secrecy
on inbox boxes; no key-continuity store. Details live in the audit thread;
fix in priority order: signaling auth → quota lifecycle → LAN trust →
transfer bounds.
