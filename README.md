# Uplink-Delta 🚀

Uplink-Delta is a resilient, offline-first, client-side encrypted file-sharing platform designed for both direct P2P transfers (LAN & WAN) and secure cloud-mediated sharing.

Built with a **Go stdlib-first** philosophy, the CLI client performs zero-buffering streaming uploads, NAT hole-punching, and client-side encryption, matching a glassmorphic **Next.js** web interface with CDN-powered inline file previews.

> [!NOTE]
> **Latest Release: v0.0.1**
> * **Security & Timing Attack Protection**: Implemented constant-time checks for P2P authentication.
> * **Download Limit Counter Enforcement**: Refactored the web preview component to dynamically authorize downloads, preventing download limit bypasses.
> * **Critical E2EE Overflow Fix**: Resolved a 2-byte chunk size limit overflow issue by adjusting the chunk limit to 65,519 bytes, permitting decryption of files larger than 64KB.
> * **E2EE warnings**: Added security notifications inside CLI stdout highlighting key exposure risks in command history.
> * **R2 Hash Verification Fallback**: Computes actual SHA-256 from object storage when hash headers are absent.
> * **Accurate Directory Size Checks**: Replaced the directory metadata size check with a recursive size calculation of all files inside the directory before uploading or queueing.
> * **Atomic LAN Share Limits**: Enforced atomic compare-and-swap checks for LAN download limits to resolve race conditions under concurrent client downloads.
> * **Lifecycle Context Propagation**: Propagated cancellation context down to all HTTP and multipart upload calls.
> * **Idempotency Cache Correction**: Aligned Next.js API idempotency key expiration with the 2-hour presigned URL duration to prevent expired presigned URLs from being cached and reused.
> * **Secure PRNG for Share Codes**: Replaced weak pseudo-random generation with cryptographically secure generation for share codes.

---

## Key Features

### 1. Peer-to-Peer Transfers (LAN & WAN)
* **Direct LAN Mode**: Share files directly over local networks. Uses mDNS service discovery with hashed share codes (`sha256(code)[:8]`) to prevent sniffing, serving files over ephemeral TLS using on-the-fly self-signed certificates.
* **WAN Mode via DHT**: Connects NAT-isolated peers using `go-libp2p` and Kademlia DHT content routing. Performs direct UDP hole-punching over QUIC connections.
* **Secure Transfers**: Enforces transfer integrity checks using streaming SHA-256 verification and implements strict access passwords and download limits.

### 2. Resiliency & Offline Queue
* **Resumable Transfers**: Preserves exact chunk ETags and CRC64NVMe checksums from S3/R2 multipart uploads, allowing interrupted uploads or downloads to resume exactly where they left off.
* **Offline Queue**: When the network is unavailable, uploads are automatically queued under `~/.uplink/queue/`. A background polling worker monitors reachability and retries uploads.

### 3. Zero-Knowledge Security
* **End-to-End Encryption (E2EE)**: Files can be encrypted client-side using 256-bit AES-GCM in ~64 KB chunks (65,519 bytes) before upload.
* **Key Preservation**: The decryption key is appended to the download code (`<code>:<keyHex>`) and is never sent to the server.
* **Web Notice**: The browser preview page detects E2EE files and displays a warning, directing the recipient to decrypt using the CLI client.

### 4. Interactive Previews & Automation
* **Rich Web Previews**: Glassmorphic web previews for image, video, audio, text, PDF, and source code files with CDN-loaded `highlight.js` syntax highlighting.
* **Watch Directory Mode**: Monitor local directories using `fsnotify`. Automatically uploads new or modified files with a 500ms write-debouncing filter.
* **Shell Completions**: Generates autocomplete helpers for Bash, Zsh, and Fish environments.

---

## CLI Usage

### Uploading Files & Directories
```bash
# Standard upload
uplink send report.pdf

# Upload folder (automatically packages to tarball)
uplink send ./documents

# Upload with client-side encryption
uplink send invoice.xlsx --encrypt

# Start direct LAN P2P transfer
uplink send video.mp4 --lan

# Queue upload locally (useful when offline)
uplink send backup.tar.gz --queue
```

### Downloading Assets
```bash
# Standard download
uplink receive 4827165038

# Download E2EE encrypted assets (auto-decrypts using local key)
uplink receive 4827165038:7c4a8d8e9...

# Download directly over LAN
uplink receive 4827165038 --lan
```

### Managing the Offline Queue
```bash
# List all queued items
uplink queue

# Pause / Resume / Cancel a queued item
uplink queue pause <id>
uplink queue resume <id>
uplink queue cancel <id>

# Clear completed or failed queue tasks
uplink queue clear
```

### Automation & Watch Mode
```bash
# Watch a directory and auto-upload any additions/changes
uplink watch /path/to/sync/folder

# Generate shell auto-completions
uplink completion zsh > ~/.zsh/completion/_uplink
```

---

## Development Setup

### Next.js Web Application
```bash
# Install package dependencies
npm install

# Start local Next.js dev server
npm run dev

# Run ESLint validation checks
npm run lint

# Compile production bundle
npm run build
```

### Go CLI Client
```bash
# Compile CLI binary
cd cli
go build -o build/uplink

# Run CLI unit test suites
go test ./...
```

### Database Testing
```bash
# Execute local mock database tests
npx tsx scratch/run_tests.ts
```
