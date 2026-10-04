# UPLINK-Delta — Documentation

> Describes the repository as of the `UI` branch tip.
> Companion file: `FILEMAP.md` (per-file index of the whole tree).

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
  AES-GCM boxes (`cli/p2p_box.go`), deposited/fetched via the server
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
cli/lan/        mDNS discovery + ephemeral-TLS file server/client
cli/wan/        libp2p DHT wide-area transfer
cli/pkg/crc64   NVMe CRC64 helper
cli/pkg/tarball Safe directory pack/unpack
src/app/api/v1  19 Next.js API routes (admin/cleanup/mock-r2/session/share/speedtest)
src/app/share   Share landing/preview pages
src/components  FilePreview, SyntaxHighlighter (React)
src/lib         auth, rooms, redis (+mock), r2 (+mock), crypto, quota, mongodb,
                env, api-utils, crc64 (vitest suites live under tests/web/)
server/         SHELVED Go WebSocket relay (own module) — not deployed
tests/          Web/E2E suites + probes (see tests/README.md; `make test`, `make e2e`)
.github/workflows  ci.yml, release.yml
Makefile        build/install/clean/release + test targets (test-go, test-web, test, e2e, test-all)
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

### Chat TUI

Left chat list (search + General room + DM threads), center transcript +
composer. Keybindings: `Enter` send, `Ctrl+K` command palette, `Ctrl+L`
clear, `↑↓` scroll, mouse supported. There is no right video panel and no
pinned call card (both removed); liveness shows in the sidebar (`Live now`,
`Voice call • MM:SS`) and header mic chips.

Slash commands: `/help`, `/reply`, `/upload`, `/download`, `/audio`,
`/kick`, `/admin`, `/unadmin` (last three role-gated: creator/admin only;
`/admin` creator-only). Notices (command results, moderation outcomes,
engine/media events, server-down alert) surface on the **status line**,
never as transcript rows — only peer messages, own echoes, and file cards
paint the transcript.

### Reply / quote (WhatsApp-style)

Quote-replies have two entry points that share one flow:

- **Right-click** a transcript message (mouse) opens a small context menu
  near the click: **Reply** pins a quote card — colored bar, quoted
  sender, excerpt — above the composer; **Reply-Privately** pins the same
  card and jumps into a DM with that message's author, carrying the card
  into that composer. Own messages offer Reply only (a self-reply never
  notifies the author). `Esc`, a click anywhere outside the menu, or
  typing dismisses it.
- **`/reply`** is the keyboard entry: an animated `<` pointer slides in
  beside the newest message (a few `tea.Tick` frames, mirroring the
  reaction dropdown reveal), `↑/↓` moves it across messages, `Enter` on a
  message opens the same Reply / Reply-Privately menu, `Esc` exits. Any
  send or mode switch exits the selection too.

Sending with a pinned card attaches the citation to the outgoing chat
frame as optional JSON fields — `replyTo` (quoted MsgId), `replyAuthor`,
`replyExcerpt` (first line, capped at 120 chars). Old clients ignore the
unknown fields; our decode of older quote-less messages yields an empty
quote, so nothing breaks either direction. The card's `✕` clears it
before sending. Recipients render the quoted card (author + excerpt)
above the bubble in both the general room and DMs, and the **quoted
author gets a desktop ping** with exactly `X replied to you` when someone
replies to their message — self-replies stay silent.

Clicking a rendered quote card **jumps** to the quoted message: the view
switches to its conversation (general chat when it lives in the room),
scrolls it to the top of the viewport, and highlights the entire row
(name, time, text) blue for 3 seconds. If the original was trimmed from
history, an inline note — `original message no longer in view` — says so
instead.

### Landing

The CREATE/JOIN landing (`uplink` with no arguments) is two-step on the
JOIN tab: username + 6-digit session code only. The session password is
asked afterwards, in a modal overlay, only when the server reports the
session is password-protected (401 "password required"); Esc dismisses the
modal back to the form, and an incorrect password can be retried in place.
CREATE keeps its optional password field for new sessions.

### Environment variables (CLI)

| Variable | Effect |
|---|---|
| `UPLINK_SERVER` | Signaling base URL (overrides config) |
| `UPLINK_MIC` | Mic backend: unset = auto, `test` = synthetic sine, else exact ALSA/ffmpeg device |
| `UPLINK_SPEAKER_OUT` | Test hook: raw PCM capture path for speaker |
| `UPLINK_STUN` | Comma-separated STUN servers for WebRTC |
| `UPLINK_EXPIRY`, `UPLINK_DOWNLOAD_DIR`, `UPLINK_LAN_PORT`, `UPLINK_ADAPTIVE_CHUNKS`, `UPLINK_SHOW_QR` | Misc behavior overrides |
| `GITHUB_TOKEN` | Private-repo self-update auth |

Video variables are **retired** (video calling removed). Config lives in
`~/.uplink/` (identity `0600`, rest `0700/0600`).

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
  token — known limitation); file shares are bearer
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

CI (`ci.yml`, every push): Go vet + `go test -race ./...` +
cross-builds; web lint + typecheck + unit + build; e2e matrix (real Mongo
service container + mock R2); single `pipeline` gate. No per-run binary
artifacts are uploaded (quota); only failure-only `e2e-server-log`
(1-day retention). Release (`release.yml`, tags `v*`): GoReleaser →
versioned tarballs + `checksums.txt`, verified by both installers and
`uplink update`.

## 7. Protocols and crypto (summary)

- Peer identity: long-lived X25519 key, `~/.uplink/identity` (`0600`).
- Live chat/files: `Noise_XX` handshake per peer pair over WebRTC data
  channels; JSON frames (`p2p_proto.go`); per-peer send serialization;
  decrypt failure tears the session down (fail closed).
- Offline: AES-256-GCM boxes via server inbox, at-least-once delivery with
  sender retries, consumer-side dedup (`seenMsg` 5000), fetch-then-ACK.
- Voice: Opus over UDP Noise sessions, jitter buffer, mic-level gate.
- Share links: `id:KEY` (AES-256 file key, never sent to server in clear).

## 8. Implementation conventions (read before changing code)

- **TUI exact-row contract:** every painted frame is exactly `H` rows ×
  `≤W` cols; `layout` (chat_tui.go) is the single geometry truth shared by
  paint, viewport sync, and mouse hit-test. `scrollbars_test.go` and
  `TestResizeSweep*` enforce it.
- **Status line vs transcript:** notices go to the status line, never as
  transcript rows (see §4).
- **Server-down alert:** `isServerDown(err)` (transport errors + 502/503/504
  only; mesh/WebRTC-local failures excluded) → `serverDownMsg` held by the
  2s roster tick via engine `lastBeatErr`, cleared on recovery.
- **Tests:** colocated `*_test.go`; `newFilterScreen` (bare UI) vs
  `wireTestEngine` (fake `httptest` signaling server); drive the `tea`
  loop via `Update` messages, never headless.
- **Go style:** `nil`-then-close channel teardown, fail-closed validation,
  bounded queues/channels for all new code.
- **Branches:** active work is on `UI`; `main` is the last merged base.
  Push branches with `git push --all origin`; the maintainer merges.

## 9. Known limitations (audit-backed, not yet fixed)

Member auth is header-only (no join tokens); quota `confirm` trusts
declared size; LAN discovery is mDNS-trust-on-first-use; WAN receive path
is non-functional; cloud/LAN/WAN download streams are unbounded; no
forward secrecy on inbox boxes; no key-continuity store. Fix in priority
order: signaling auth → quota lifecycle → LAN trust → transfer bounds.
