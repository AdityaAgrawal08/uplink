# Uplink WebSocket Server

Standalone Go WebSocket server for real-time chat sessions. Replaces HTTP long-polling with persistent bidirectional connections.

## Architecture

- **In-memory relay** — no database dependency for chat messages
- **Application-layer encryption** — X25519 key exchange + AES-256-GCM per message
- **Ephemeral sessions** — 6-digit codes, auto-expire after inactivity
- **File transfer** — chunked over WebSocket (up to 256MB)
- **Delete-for-everyone** — instant broadcast, no tombstones

## Quick Start

### Local Development

```bash
cd server
go run .
# Server starts on :8080
```

### Docker

```bash
docker compose up --build
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Listen port |
| `UPLINK_MAX_USERS` | `50` | Max users per session |
| `UPLINK_MSG_BUF` | `100` | Message ring buffer size |
| `UPLINK_SESSION_TTL` | `30m` | Session expiry duration |
| `UPLINK_HB_TIMEOUT` | `45s` | Heartbeat timeout |
| `UPLINK_RATE_LIMIT` | `20` | Max messages per second per user |
| `UPLINK_MAX_FILE_MB` | `256` | Max file size in MB |

## API

### Health Check

```
GET /health
→ {"status": "ok", "sessions": 3}
```

### Create Session

```
POST /api/v1/session/create
Body: {"username": "alice", "password": "optional", "duration": 600}
→ {"sessionId": "123456"}
```

### WebSocket Connect

```
GET /api/v1/session/{id}/ws
Header: X-Uplink-Username: alice
Query: ?password=optional
```

## WebSocket Protocol

### Client → Server

```json
{"type": "join", "clientPublicKey": "<base64>"}
{"type": "chat", "text": "hello", "to": ""}
{"type": "file", "msgId": "...", "filename": "doc.pdf", "size": 12345, "sha256": "...", "totalChunks": 5, "to": ""}
{"type": "file-chunk", "msgId": "...", "chunkIndex": 0, "data": "<base64>"}
{"type": "file-complete", "msgId": "..."}
{"type": "delete", "deleteMsgId": "uuid"}
{"type": "heartbeat"}
```

### Server → Client

```json
{"type": "welcome", "sessionId": "123456", "sessionPublicKey": "<b64>", "users": ["alice"]}
{"type": "chat", "msgId": "...", "username": "alice", "text": "hello", "createdAt": "..."}
{"type": "file", "msgId": "...", "username": "alice", "filename": "doc.pdf", "size": 12345, "totalChunks": 5, "createdAt": "..."}
{"type": "file-chunk", "msgId": "...", "chunkIndex": 0, "data": "<base64>"}
{"type": "file-complete", "msgId": "..."}
{"type": "delete", "deleteMsgId": "...", "username": "alice"}
{"type": "users", "users": ["alice", "bob"]}
{"type": "system", "text": "bob joined"}
{"type": "heartbeat-ack", "users": ["alice", "bob"]}
{"type": "error", "message": "rate limited"}
```

## Encryption

Each session generates an ephemeral X25519 keypair on the server. Clients exchange public keys during join and derive shared secrets via ECDH. Messages are encrypted with AES-256-GCM using keys derived via HKDF-SHA256.

The server relays opaque ciphertext — it cannot read message content.

## Testing

```bash
go test -v ./...
```

## Deployment

### Railway

```bash
railway init
railway up
```

### Fly.io

```bash
fly launch
fly deploy
```

### Vercel (not recommended)

Vercel does not support WebSocket servers. Use Railway, Fly.io, or a VPS.
