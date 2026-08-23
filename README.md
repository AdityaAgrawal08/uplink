# Uplink-Delta 🚀

A fast, secure file-sharing platform with a built-in **terminal chat** — one Go binary, four commands:

```
uplink send <file>            # share any file or folder
uplink receive <code>         # download a share
uplink create session         # start a terminal chat room
uplink join <6-digit-key>     # join one
```

Files are streamed straight to Cloudflare R2 via presigned URLs; share metadata lives in MongoDB. Optional client-side encryption keeps the server zero-knowledge.

---

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/AdityaAgrawal08/uplink-delta/main/install.sh | sh
```

Installs to `/usr/local/bin` (falls back to `~/.local/bin`, honors `PREFIX`).
Build from source instead: `cd cli && go build -o uplink .`

---

## Sharing Files

```bash
# Share a file — returns a 10-digit code + link
uplink send report.pdf

# Folders are packed into a tarball automatically
uplink send ./my-project/

# End-to-end encrypt before upload (key appended to the code)
uplink send invoice.xlsx --encrypt

# Custom expiry (5m / 30m / 2h / 1d) and password protection
uplink send report.pdf --expire 30m --password hunter2
```

```bash
# Download by code into the current directory
uplink receive 4827165038

# Into a specific destination
uplink receive 4827165038 ~/Downloads

# Handle existing targets
uplink receive 4827165038 --force    # overwrite (-f)
uplink receive 4827165038 --rename   # save as "report (1).pdf" (-r)

# Encrypted shares decrypt automatically when the key is in the code
uplink receive 4827165038:7c4a8d8e9...
```

Integrity is verified end-to-end with SHA-256 on every download; large files
upload in resumable multipart chunks (S3-safe 5 MiB minimum part size).

## Direct LAN Transfers

Skip the cloud entirely between machines on the same network:

```bash
uplink send video.mp4 --lan           # mDNS discovery + ephemeral TLS
uplink receive <share-code> --lan     # falls back to cloud if peer not found
```

## Terminal Chat

```bash
uplink create session      # pick a nickname → get a 6-digit room key
uplink join 482716         # friends join anywhere on the internet
```

Full-screen chat UI (OpenCode-style chrome): messages deliver in ~0.5 s,
live online roster (`/users`), graceful `/exit` or Ctrl+C.
The room ends automatically the moment its last member leaves, and the
transcript is purged. In scripts/CI, set `UPLINK_CHAT_PLAIN=1` for plain-line
rendering.

## Web Previews

Every share gets a browser page with inline previews for text/code, images,
video, audio, and PDFs — plus a QR code for phone downloads.

---

## Configuration (optional)

| Env var | Default | Purpose |
|---|---|---|
| `UPLINK_SERVER` | `https://uplink-delta-xi.vercel.app` | Backend to talk to |
| `UPLINK_EXPIRY` | `1h` | Default share expiry |
| `UPLINK_LAN_PORT` | `9090` | Base port for LAN transfers |
| `UPLINK_SHOW_QR` | `auto` | QR display after uploads |
| `UPLINK_CHAT_PLAIN` | unset | Force plain-text chat rendering |

A JSON config file at `~/.uplink/config.json` mirrors these settings.

---

## Development Setup

### Next.js backend
```bash
npm install
npm run dev        # dev server on :3000 (mock storage without R2 creds)
npm run lint
npm run build
npx tsc --noEmit
```

### Go CLI
```bash
cd cli
go vet ./...
go test ./...
go build -o build/uplink .
```

### End-to-end tests (need a local server on :3000)
```bash
npm start &                                # terminal 1 (uses mock storage)
SERVER=http://localhost:3000 ./scratch/e2e_phase0.sh            # file matrix
SERVER=http://localhost:3000 ./scratch/session_flow_test.sh     # sessions API
SERVER=http://localhost:3000 ./scratch/chat_two_clients.sh      # two-client chat
```

CI runs all of this on every push (Go cross-build matrix, lint/typecheck/
unit/build, and the full E2E suite against a MongoDB service container).

---

## License

MIT — see [LICENSE](LICENSE).
