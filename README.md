# Uplink Delta

Uplink Delta is a private file-sharing and chat tool that runs in your terminal, backed by a small self-hosted web service. You can send a file or folder to someone and pass along a short code or link, open a chat room where a group can message, share files, and talk over voice, and receive anything through an ordinary browser. Rooms are ephemeral, transfers are verifiable, and the service in the middle coordinates devices without ever being able to read message or file contents.

## What it is

The project has three cooperating parts:

- **The terminal client** — one small program for Linux, macOS, and Windows that sends and receives files, runs the chat interface, carries voice calls, shows QR codes, and can update itself. This is where everything is created and controlled.
- **The web service** — a signaling plane that helps devices find each other, a small store of encrypted offline messages, and the storage and preview layer behind share links. It is self-hosted as a single small cloud instance behind a reverse proxy, using managed Redis, MongoDB, and object storage.
- **A shelved relay** — an alternative always-on WebSocket relay that exists in the project but is not deployed. It is kept only as a documented fallback; see the note near the end.

The core promise is simple: message text, file contents, and voice audio are encrypted on one device and decrypted only on the devices of the people involved. The service sees routing information — usernames, public keys, presence, room membership, names of groups, file sizes and timing — but not message text, not file bytes, and not call audio.

There are no accounts. On first run the client creates a private device identity on your machine, and that identity is what everything else is built on.

## File sharing

### Sending and receiving

You point the send command at any file or folder. Folders are packed into a compressed archive automatically and restored into a folder of the same name on the receiving side. The sender gets back a ten-digit numeric download code and a share link; either one works as the thing you pass to the recipient. The link points at a browser page that also works as the receive path.

The receive command accepts the full link, the ten-digit code, or a link that carries an encryption key suffix. A destination path is optional, and you can point the client at a standing download directory instead of naming one every time. Before writing anything the client checks whether the target already exists; you choose between overwriting and saving under a suffixed name, and the client can create missing destination directories on request. Downloads are verified against the hash recorded at upload time, and a mismatch deletes the file and reports an integrity failure rather than keeping bad bytes.

### Cloud transfers

By default a share is uploaded to the service's object storage and stays there until it expires. Small files go up in one request; anything larger is split into parts of an adaptive chunk size — measured against the server's bandwidth test and clamped between five and ten megabytes, targeting roughly five seconds per part — with a maximum of one hundred parts. Upload sessions live for two hours, the signed upload and download URLs are valid for two hours and one hour respectively, and a repeat send of the same file with the same password and expiry reuses the first result through an idempotency key instead of creating a duplicate.

Uploads are resumable. The client keeps a per-file resume record locally: completed parts are skipped on a retry, the server can report which parts it already holds, and if conditions change (such as the adaptive chunk size being re-measured differently) the client safely starts a fresh upload rather than posting mismatched byte ranges. A resumed upload older than two hours is discarded as stale, and old resume records are cleaned up after a day. Folder archives are unpacked with a size cap derived from the download size as a defense against decompression bombs.

### End-to-end encryption

If you turn on encryption, the file is encrypted on your machine before a single byte leaves it. The tool generates a random 256-bit key, splits the file into chunks, and encrypts each chunk with AES-256-GCM using a nonce derived from a random master nonce and the chunk index, with the chunk index also bound into the authentication tag so chunks cannot be reordered or forged. A SHA-256 digest of the original plaintext is recorded alongside. The key is never sent to the service: it rides only in the link (and in the code you copy), appended after the share identifier. The service stores ciphertext and can tell the file is encrypted, but cannot preview, inspect, or decrypt it. The CLI decrypts after the download completes and verifies the plaintext digest before the file is considered good. The browser page detects an encrypted share and explains that decryption requires the terminal client.

### Passwords

A share can be password-protected. The password is hashed by the service with Argon2id (three iterations, sixteen megabytes of memory, a random salt, a 32-byte hash) and never stored or logged in clear. The recipient's client prompts for it when the service says one is required, and wrong guesses are rate-limited: five failed attempts per address and share within five minutes, applied before verification so parallel guesses cannot slip past the check. The interactive browser preview shares the same failure budget, so the two password surfaces cannot be used to double the guessing allowance.

### Expiry and download limits

Every share carries an expiry, written as minutes, hours, or days. The CLI default is one hour; the service default and hard maximum are twenty-four hours, and anything outside the valid range falls back to the default. A share also carries a download limit, ten by default. Downloads increment an atomic counter, and when the limit is reached the share is marked expired and refuses further access. Opening the page for an inline preview of a safe media type does not consume a credit, but text previews do count against the limit, so a share cannot be farmed indefinitely through the preview path. Expired and deleted shares are swept automatically: a background housekeeping pass runs opportunistically on share requests, a scheduled job runs daily, and objects are deleted from storage with the storage and operations ledgers adjusted accordingly. Upload sessions and their metadata are also cleaned up after expiry.

### Integrity

All shares carry a SHA-256 hash of the file; multipart shares additionally carry a CRC-64 checksum used when the pieces are stitched together. The sending client computes them while streaming, the service stores them, and the receiving client recomputes and compares before announcing success. The browser download path relies on the same recorded hash, and the CLI's resumable download probes whether the storage supports byte ranges and can continue a partial download when it does.

### LAN transfers

On the same local network, the client can send directly between the two devices with no cloud storage involved. The sender announces a temporary service over local network discovery, using a hashed form of the share code so the code itself is not broadcast; the announcement carries the file name, size, hash, and a fingerprint of a freshly generated temporary TLS certificate. The receiver finds the peer, pins the certificate to the advertised fingerprint, and downloads over an encrypted local connection. The transfer is hash-verified; passwords are supported, with the receiver asked interactively when the share is protected; and the sender prints the fingerprint so both ends can compare it. If no local peer connects within about a second, the sender falls back to the ordinary cloud upload so the share always completes. Folders travel as archives and are unpacked on arrival. Note that the local announcement itself is plaintext on the network segment — only the transfer is encrypted — so sensitive file names are visible to a local observer.

### WAN transfers (experimental)

There is an experimental mode that bypasses the cloud entirely and uses a peer-to-peer network overlay with a distributed hash table to rendezvous: the share code is hashed into a content identifier, published to the network, and the sender serves the file directly. Treat it as unfinished: the send side initializes and publishes, but the receive path is known not to work reliably, so it is not a substitute for cloud or LAN transfers yet.

### QR codes and convenience

Share links can be shown as a QR code right in the terminal, rendered with a compact half-block style. By default it appears only when the output is a real terminal at least eighty columns wide with UTF-8 support, but it can be forced on or suppressed. The link is also copied to the system clipboard when possible, and desktop notifications announce completed or failed transfers.

## Chat rooms

### Creating and joining

Creating a room mints a six-digit code on the service and prints it in a small panel along with the join command. The creator can set an optional room password and is automatically crowned the room's main administrator. Joining asks for a username — three to twenty characters, letters, digits, and underscores — and the six-digit code. If the room is protected, the password is requested only after the service confirms it is needed, and a wrong password can be retried in place. A username already taken in the room is refused and you are asked for another; a vanished room explains that rooms disappear when emptied. The terminal client retries transient join failures with backoff. Joining is also available from the app's start screen, which offers Create and Join tabs: the join tab asks for username and code first and shows the password prompt as a modal overlay only when the service reports the room is protected, so an unnecessary password field never appears.

### Lifecycle

Rooms are ephemeral by construction. They live in the service's key-value store only — there is no permanent record and no transcript — from creation until the last member leaves. Membership is maintained by heartbeats every five seconds; a member whose heartbeat is more than twenty seconds old is treated as offline, so joins and departures become visible quickly. A sliding twenty-four-hour expiry acts only as a safety net against crashed clients that never send a leave, refreshed by any activity in the room. When the last member leaves, the room is destroyed immediately: the roster, bans, reactions, invites, and any queued messages are purged. A periodic sweep prunes lapsed members and destroys emptied rooms. The service also tracks a roster generation counter so members notice a join or departure immediately instead of waiting for the next poll.

### Terminal interface versus plain mode

When standard input is a real terminal, the client opens a full-screen interface. When it is not — piped output, scripts, or continuous integration — or when plain mode is explicitly requested through its environment switch (UPLINK_CHAT_PLAIN), the same protocol engine runs in a line-based headless mode instead. The plain mode prints one line per event: messages with a timestamp and sender, files with sender, name, size, and save location, encrypted-channel and lost-line notices, and an initial online list. It accepts typed lines as messages and understands a small exit or quit command; everything else broadcasts. Plain mode acknowledges every message so senders' retries settle. The room is live from the moment you join — the service keeps no backlog, so there is nothing to scroll back to that predates you.

### Sidebar, transcript, and composer

The full interface is built around a left sidebar and a transcript with a composer. The sidebar lists a searchable set of conversations: the general room, one-to-one threads with each member, and any groups you have joined, with unread badges and short previews of recent activity. It shows who is present ("Live now") and, during a voice call, a call indicator with elapsed time. The header area shows microphone state as small chips when you are publishing audio.

The transcript paints sender names, timestamps, grouped consecutive messages from the same sender, right-aligned own messages, file cards, and system notices kept out of the message flow on a dedicated status line rather than mixed into the conversation. The composer sends on Enter, has a command palette that opens with a control shortcut or by typing a slash, supports fuzzy filtering and a small recent-command list, clears the screen on another shortcut, scrolls with arrow keys, and supports the mouse for clicking messages, quotes, reactions, and menus. The layout is engineered to paint exactly the terminal's height and never exceed its width at any size, and there are automated tests sweeping terminal resizes to keep it that way. Colors adapt to light and dark terminal themes.

### Slash commands

- **General** — help, reply (quoted replies, described below), settings (the invites and notifications window).
- **Groups** — create a group, invite room members to it, rename/transfer/delete it, list its members, leave it.
- **Files** — upload files into the current conversation, download or browse the files received this session.
- **Voice** — toggle your microphone: to a one-to-one peer when a direct conversation is open, or to the room.
- **Moderation** — kick a user, grant an admin, revoke an admin. The client enforces the visible rules; the service enforces the real ones.

Moderation outcomes, command results, engine and media events, and a server-down alert all surface on the status line, not as fake messages in the transcript.

### Sharing files inside a room

Files sent in a conversation travel peer-to-peer, encrypted end to end — there is no server-side storage step at all. An upload picker lets you browse your machine, select several files, and include folders, which are packed into archives client-side. Each item is capped at twenty-five megabytes in this version, and transfers run one at a time so the progress display stays readable. Small files can also take the encrypted offline path; larger ones need a direct connection. Received files are saved automatically into your Downloads folder under collision-safe names and appear as cards in the transcript and in a downloads drawer that lists everything received in the session, newest first.

### Moderation and leaving

The room creator — the main administrator — can kick anyone except themselves, and can grant and revoke admin status. Admins can kick ordinary members only, never other admins and never the creator. Kicked users are barred from rejoining while the room lives, and their queued messages are cleared. When the creator leaves a room that still has members, the crown transfers automatically to the longest-joined admin, or to the longest-joined member if no admin remains; the successor's privileges follow the crown. Leaving from the home room shuts the app down; inside a group, leaving just steps back to the home room and keeps the app running.

## Messages

### How messages travel

For each pair of devices, the client establishes an authenticated Noise handshake session over a WebRTC data channel, using the device identity keys, with ChaCha20-Poly1305 and SHA-256. Every message, file piece, and reaction nudge travels inside that per-peer encrypted channel as a compact frame whose header fields — type, message identifier, sender, recipient — remain plaintext for routing but carry no content. Sends to a peer are serialized so the encryption order matches the wire order. A frame that fails to decrypt tears the session down rather than risking anything: failures are closed, never displayed. Both sides also derive a short, human-comparable safety code from the two identity keys; reading it aloud and comparing once rules out a key-swap attack by the service.

### Formatting

Message text supports lightweight Markdown: bold, inline code, fenced code blocks with an optional language label, and labeled links. Terminal escape sequences and control characters are stripped from all untrusted text — messages, names, filenames — before rendering, so a malicious message cannot execute terminal control sequences on the receiving end.

### Replies and quotes

A message can be answered as a quote reply, WhatsApp style. Right-clicking a message opens a small context menu with Reply, which pins a quote card above the composer showing a colored bar, the quoted author, and an excerpt of the original. Reply-Privately pins the same card and switches into a direct conversation with that author. There is a keyboard path as well: the reply command slides a pointer beside the newest message, arrows move it across messages, and Enter opens the same menu. Escape, clicking elsewhere, or simply typing dismisses the selection.

Sending with a pinned card attaches a citation to the message: the original's identifier, author, and a one-line excerpt capped at 120 characters with whitespace folded and control characters stripped. Recipients see the quote above the bubble in both the room and direct conversations, and clicking a quote jumps to the original message — switching conversations if needed, scrolling it to the top, and highlighting it briefly; if the original has been trimmed from the view, an inline note says so instead. The quoted author receives exactly one desktop notification phrased as "X replied to you"; replying to your own message stays silent. Older clients that do not know about quotes simply ignore the extra fields.

### Mentions

Typing an at-sign in the general room opens a member-suggestion dropdown; Tab or Enter completes the name and Escape dismisses it. Any valid name token paints as a highlighted chip inside message bubbles, in the room and in direct conversations alike. When someone mentions your name in the general room you get a desktop notification — always, even while you are looking at the chat — and direct messages never ping this way, where the at-sign is treated as plain text. Mentions are ordinary plain text inside ordinary encrypted messages: nothing extra is sent to or stored by the service.

### Reactions

Any message can be reacted to with one of six emoji. Each user has at most one reaction per message: choosing the same emoji again removes it, choosing a different one replaces it. Counts are aggregated per message and shown as small chips under the bubble; clicking a chip opens a per-reactor breakdown. Reactions are server-tracked as lightweight metadata — the message identifier, the emoji, and the reacting username — never the message content. The client reconciles counts against the server every two seconds, while a live nudge frame makes an incoming reaction appear immediately, and the picker's toggles are optimistic until the next sync.

### Notifications

Desktop notifications cover replies, mentions, group invites, and transfer completion or failure. They are always attempted, even when the chat is focused. If no desktop notification path exists — headless sessions, remote shells, minimal desktops — the client prints a one-line warning and rings the terminal bell so the event is still noticed.

### Offline delivery and deduplication

If a peer has no live connection, messages take a fallback path: each message is sealed for the recipient with a pairwise key derived from the two device identities (static Diffie-Hellman to a symmetric key, then AES-256-GCM per message with a random nonce) and deposited in the recipient's server-side inbox as opaque ciphertext. The recipient fetches the inbox on its two-second poll and acknowledges after processing; the service deletes acknowledged boxes. Boxes live up to an hour, refreshed while new ones arrive, and each recipient's inbox holds at most fifty boxes of at most 128 kilobytes each. This path deliberately sacrifices per-message forward secrecy — the keys are static — which is acceptable because it is the fallback, not the primary channel; the same safety-code comparison still authenticates the peer's key.

On the live path, every mesh-sent message waits for a delivery acknowledgment. If no acknowledgment arrives, the sender re-sends through the durable inbox up to three times before giving up, so a message that hits a dead connection still lands. Because delivery is at-least-once, receivers deduplicate by message identifier with a bounded recent-ID set, so retries and racing paths surface each message exactly once. Plain mode does the same. Files small enough for one box — roughly 160 kilobytes of raw data — can travel this way; larger files need the direct line, where they stream in 64-kilobyte encrypted chunks with per-chunk verification and a whole-file digest, with a cap on how many reassemblies can be in flight and a fifteen-minute timeout for stale partial transfers.

## Groups

### What groups are

A group is a named room of its own, created inside the room you are currently in. It gets its own six-digit code, its own roster, and its own full chat engine on every member's machine, so messages, reactions, mentions, quotes, files, and moderation work exactly as they do in the main room. The group records which room it branched from.

### Creating and inviting

Group creation is a two-stage window: first a member multi-select where typing filters the list and Space toggles picks, then a form with a required name (one to sixty-four characters), an optional description (up to 256 characters), and an optional maximum member count — "any" or a whole number of at least two. The service crowns the creator as the group's main administrator automatically. After creation, members are added through invites: any room member can be invited to the group, the invitee gets a desktop ping, and a badge on the settings button counts pending invites. Pending invites are also polled in the background so new ones ring once and move the badge; each user can hold at most fifty pending invites.

### Accepting and declining

The settings window lists every pending invite with the inviter's name and the group name, and offers accept and decline actions by keyboard or mouse. Accepting a protected group runs the same two-step join as the main room: the first attempt reports that a password is required and opens a modal, a wrong password can be retried in place, and Escape backs out with the invite still available. A full group reports that the maximum allowance is reached; the invite is consumed at that point and is terminal — the row disappears and trying again reports the invite gone rather than retrying. Declining removes the invite.

### Membership roles and moderation

Roles are ranked: creator (main administrator) above admin above member. The creator can rename the group, edit its description, transfer the crown, kick anyone except themselves, and delete the group. Admins can rename, edit, transfer, and kick ordinary members only. Ordinary members can leave but cannot change anything. The last remaining admin cannot leave — the client refuses with a message telling you to promote someone else first — so a group can never be accidentally left leaderless. A kick bans that username for the group's lifetime.

### Persistence and re-joining

The service keeps no "groups I belong to" list, so the client remembers your memberships locally, keyed by the home room code, so one machine can hold memberships for several rooms without collisions. The store is written atomically and user-only readable, and it is updated when you create a group, accept an invite, rename, leave, get kicked, or see a group dissolved. Group passwords are stored in that local file in plain text by deliberate tradeoff — that is what lets a protected group re-join automatically after a restart without asking again — and the file's permissions are the protection.

On startup every saved group appears in the sidebar immediately and is re-joined in the background with its saved password. A successful re-join keeps the row; rejections — gone, kicked, full, wrong password — drop the row and remove it from the store so no ghost entries linger; a server outage at launch keeps the row for the next attempt. A group that survived the restart counts as a live seat.

### Tombstones

Leaving a group, being kicked, or having it dissolved leaves a dimmed placeholder row in the sidebar describing what happened — you left, you were removed by an admin, the group filled up, or the group ended — until a fresh invite or join re-establishes the live row. Tombstones exist only for the session and are never written to disk, so a restart never resurrects a ghost.

### Group administration and deletion

The group administration window lets an admin or the creator edit the name and description; saving sends only the changed fields and updates the local view and sidebar immediately. Transferring admin opens the same member picker used elsewhere; each pick performs the same grant the moderation command performs. Deletion is creator-only and asks for confirmation with the consequences spelled out: for everyone, immediately, with members losing access on their next heartbeat. Confirming purges the group for every member. Ordinary members see the administration window read-only.

One known wrinkle: because the service offers no group-metadata read endpoint, a rename or description change is seen immediately by the editor, but other members pick it up when they rejoin; the invite and creation payloads are the only other source of the group's metadata.

## Voice chat

### The publish model

Voice uses a publish-only model. There is no ringing, no accepting, and no busy signal: toggling the microphone command starts publishing your audio to your current scope — the peer of a direct conversation, or every online member of the room — and toggling it again stops. Receivers dial the publisher back directly and play what arrives. Announcements are idempotent, late joiners are admitted through the same announcement flow, watchers answer with their own connection coordinates when asked, and handshake roles follow a username ordering rule so two people starting at the same moment never deadlock.

Only one speaker is heard at a time. Mixing two voices cannot be done cleanly, so the first active publisher is pinned and the pin holds until two seconds of silence, after which another publisher can take over.

### Audio pipeline and backends

Audio is mono, 48 kHz, in 20-millisecond frames, encoded with Opus at about 32 kilobits per second. Capture prefers the native Linux sound path and falls back to a bundled multimedia tool everywhere else; a synthesized test tone is available for testing. Playback always goes through the multimedia tool's player over a pipe, which is mature on every platform and needs no native bindings. Devices that deliver other sample rates are resampled, and stereo input is folded to mono. A microphone level gate keeps silence from being sent. Incoming audio passes through a small jitter buffer — nominally sixty milliseconds of playout delay, twelve frames maximum before a resync — that reorders packets, recovers single lost frames from the next packet's in-band data, and requests repairs instead of stalling.

### Transport and call healing

Call audio travels as direct UDP datagrams between the two devices, wrapped in a Noise handshake using the same device identities as chat. Because UDP can lose and reorder, each datagram carries an explicit random nonce (unlike the sequential nonces of the chat channel) and a replay cache rejects duplicates. Before a session exists, unencrypted cookie-matched probes nominate a reachable address. Datagrams stay under a typical network MTU; handshake messages ride the authenticated room signaling queue, so there is no way to spoof a call from outside the room. Every two seconds the client checks each expected call peer; a silent session is re-probed at most once every thirty seconds, and each outcome is reported honestly on the status line.

### Requirements

Voice needs a working microphone. Linux capture uses the native sound interface when available and the multimedia tool otherwise; other platforms use the multimedia tool, and playback always requires it. Everything else in the app is keyboard-only, and the app is audio-only: video calling was removed from the product.

## The browser share experience

A share link opens in any ordinary browser, so receiving requires no app at all. The page shows the file's name, size, upload date, and expiry, along with a small QR code of the page itself ("scan to share"), a prominent download button, and a share button that uses the device's native share sheet when available and copies the link otherwise.

Inline previews adapt to the file type: text and source files get a syntax-highlighted preview capped at the first hundred kilobytes with copy and "save as text" actions; images, video, and audio render in place; PDFs render in an embedded frame; everything else shows a clear "no preview" notice with the download still available.

A password-protected share shows an unlock form first; the wrong password is reported inline and repeated guessing is locked out for five minutes after five failures. An end-to-end-encrypted share shows an encrypted notice instead of any preview and directs the recipient to the terminal client, because the decryption key never reached the service. Requests for missing or expired shares land on a not-found page. The service's own front page explains that uploading is terminal-only and links to the dependency-graph page; that page renders an interactive map of the project's source dependencies, generated automatically when the service is built.

Preview authorization consumes the same download counter as real downloads where it matters, so a share cannot be previewed indefinitely, and both the preview endpoints and the authorization endpoint are rate-limited and verified against the same password-failure budget.

## Identity, encryption, and the trust model

### Device identity

There are no accounts and no sign-up. On first run the client generates a 32-byte X25519 identity key and stores it user-only readable in your home directory; every launch reuses it, which keeps your safety codes stable across sessions. A damaged identity file produces a clear error rather than running half-configured. The public half of this key is advertised in room rosters so peers can encrypt to you, and it is the anchor for every authenticated signaling call: joining a room claims your username against your public key immutably, and the service will not act for a username without proof of the matching private key.

### What is encrypted, and where the keys live

Different paths use different cryptography, chosen for their situation:

- **Live chat and peer-to-peer files** use per-peer Noise sessions established with the device identity keys (X25519 key agreement, ChaCha20-Poly1305, SHA-256) over WebRTC data channels. Sessions are re-established per connection, and a decrypt failure tears the session down instead of displaying data.
- **Offline messages and fallback files** use pairwise boxes: static identity-key Diffie-Hellman, HKDF-SHA256, and AES-256-GCM per message. This path has no per-message forward secrecy by design; it exists only for peers who are not connected.
- **Encrypted file shares** use per-file AES-256-GCM chunk encryption with a random key that travels only in the link, never to the service. The plaintext digest is verified after decryption.
- **Voice** uses a per-call Noise handshake over the same device identities, with replay-protected datagrams.
- **Room and share passwords** are Argon2id-hashed by the service and never stored in clear.
- **Reaction metadata and offline message envelopes** are the only message-adjacent things the service can read, and reactions carry only identifiers and emoji.

### Request signatures and replay protection

Every signaling call that acts on behalf of a username must be signed. The client signs a canonical string of the HTTP method, path, millisecond timestamp, and a random nonce with the device identity key — using the Edwards form of the same scalar, so the service can verify with a stock Ed25519 check against the public key it already holds for that username, with no key conversion and no extra keys on the client. The signature, timestamp, nonce, and username travel as request headers.

The service rejects any call whose signature is missing, stale (more than thirty seconds off), invalid, or replayed; each timestamp-and-nonce pair is single-use per username, remembered for two minutes. Failed verification returns a distinct error, and the client treats it as "not a member" and rejoins with its identity. Verification anchors differ per call family: most calls verify against the roster public key claimed at join; accepting an invite verifies against the public key carried in the request itself, binding the seat to the key that signed, not just the name; and invite-scoped calls use a per-user first-use key claim. Crown transfers during leave are signature-verified too, so a crown can only move on the leaver's own authority. Media handshakes carry generation numbers so a redelivered signaling note can never tear down a healthy session.

### What the service can and cannot see

The service necessarily sees: usernames and public keys on a roster; presence and heartbeat timing; room membership, roles, bans, and roster changes; group names, descriptions, member caps, and which room a group branches from; message identifiers, senders, recipients, sizes, and timing; reactions as message-identifier/emoji/username triples; share filenames, sizes, hashes, download counts, and timing; and hashed client addresses used for rate budgets. It also verifies against a dummy password hash when a room has no password, so protected and unprotected rooms take comparable time to answer, closing a timing side channel that could otherwise be used to map which rooms are protected.

The service cannot see: message text, file bytes, voice audio, encryption keys for encrypted shares, or anything inside an offline message box. Share passwords are only present as hashes. Client addresses are stored only as keyed hashes, with a configurable trust posture for proxies in front.

## Limits and defaults

A consolidated reference of the numbers that shape behavior, all adjustable only where noted:

**Transfers and shares**

- File uploads up to 200 MB; folder archives up to 500 MB (enforced by both client and service).
- Multipart uploads: at most 100 parts; part sizes adapt between 5 MB and 10 MB based on measured bandwidth, targeting about five seconds per part.
- Upload session lifetime and signed URL validity: two hours; signed download URLs: one hour; resume records expire after two hours and are groomed after one day.
- Share expiry: one hour by default, twenty-four hours maximum.
- Download limit: ten by default, counted atomically per download, with safe-type inline previews exempt and text previews counted.
- Storage budget: about 9.4 GB total across active and reserved space, with a 9 GB recovery margin before uploads re-enable after a quota stop; Class A operations budgeted at 900,000 per month and Class B at 250,000 per day, resetting monthly and daily.
- Per-address in-flight upload reservations: about 1 GB, with a two-hour backstop.
- Share identifiers are 22-character random strings; download codes are ten digits.
- Text previews are capped at about 100 KB; the bandwidth-test payload is 256 KB.

**Rooms and messages**

- Room codes are six digits; usernames are 3–20 characters of letters, digits, and underscores.
- Room creation: ten per address per hour.
- Presence: five-second heartbeats; a member is offline after twenty seconds of silence; the room's sliding safety expiry is twenty-four hours.
- Send budgets: 120 sends per address per five minutes, with a 60-per-five-minute fairness share per username; join and leave: 120 per address per five minutes plus 120 join attempts per room per five minutes against code scanning; reads (heartbeats, polls, fetches, acknowledgments): 1,200 per address per five minutes with a 600 share per username.
- Signaling queue: 150 notes per user, each up to 16 KB.
- Inbox: 50 boxes per recipient, each up to 128 KB, one-hour sliding lifetime, fetched and acknowledged on a two-second poll.
- Message deduplication remembers the 5,000 most recent identifiers; file deduplication 2,000.
- The transcript keeps 5,000 rows of history, 500 local status lines, an outbox of 100 pending messages, and a received-files list of 200; inactive direct conversations are trimmed.
- In-room file sends are capped at 25 MB per item; the single-box fallback caps at about 160 KB; streamed files move in 64 KB encrypted chunks; at most 16 reassemblies run at once with a fifteen-minute stale timeout.
- Quote excerpts are capped at 120 characters; replies to a quote notify the quoted author with one exact message.
- Reactions use a fixed six-emoji set, one per user per message; counts are aggregated and a per-reactor list is capped at 20 names per emoji; the client polls at most 50 message identifiers at a time.

**Groups**

- Group names: 1–64 characters; descriptions: up to 256 characters; member cap: unlimited or an integer of at least two, enforced atomically so concurrent joiners cannot oversubscribe.
- Pending invites: at most 50 per user, polled every two seconds; invite lists show at most a handful of rows at a time in the settings window.
- Group passwords are protected the same way as room passwords and are remembered locally for automatic re-join.

**Voice**

- 48 kHz mono, 20 ms frames, roughly 32 kbps Opus.
- Jitter buffer: sixty milliseconds nominal, twelve frames maximum before resync; datagrams up to 1,400 bytes; replay cache 2,048 nonces; handshake attempts expire after sixty seconds with at most five per address; restart requests are throttled to one per ten seconds; session healing checks run every two seconds with a ten-second silence threshold and a thirty-second re-probe interval.
- One speaker is pinned until two seconds of silence.

**Browser and service**

- Share initialization: ten requests per address per five minutes; public download authorization: 120 per address and share per five minutes; password failures: five per five minutes across both password surfaces; text previews: thirty per address per five minutes; the bandwidth test: ten per five minutes; the daily cleanup worker holds a five-minute lock per share it touches.

## Install, updates, platforms, and requirements

Installers exist for all three platforms and verify downloads against the release checksums before installing anything; a missing or mismatched checksum aborts the install. On Linux and macOS a shell installer detects the operating system and processor architecture, downloads the matching archive, verifies it, and installs the binary into the system binary directory or a user prefix, using elevated permissions only when needed. On Windows a PowerShell installer downloads the matching archive, verifies it, installs into the user's local application data, and adds that directory to the user's PATH without requiring administrator rights. Releases are versioned archives for Linux, macOS, and Windows on both Intel-style and ARM processors, plus a checksums file — five tarballs from the release build, with continuous integration additionally cross-compiling six platform/architecture combinations as a validity check.

The client updates itself from the project's releases: the update command checks the latest release, compares versions, downloads the matching archive for the current platform, verifies the checksum, and replaces the binary, asking for elevated permission when the install location requires it. A token can be supplied for private-repository updates. The version command prints the version, architecture, commit, and build date. Builds from source produce a static binary with no runtime dependencies.

Platform requirements are light: a terminal is all that is needed for files and chat. Voice additionally needs a working microphone and the platform's audio tooling — native sound support on Linux, and the bundled multimedia tool for capture elsewhere and for playback everywhere. The interface sizes itself to the terminal and adapts its palette to light and dark themes.

## Deployment, configuration, and web surfaces

The web side is designed to run as a single small cloud instance with Docker: one container runs the application server bound to the loopback interface only, and a second runs a reverse proxy on ports 80 and 443. The proxy rewrites the forwarded-for header to the real client address, so per-address budgets work correctly, and when a domain is pointed at the instance the proxy obtains and renews HTTPS certificates automatically — the cutover is a one-line configuration change. The application container is minimal, runs as a non-root user, and exposes a health check built on the bandwidth-test endpoint. A retired hosted deployment remains configured as a rollback path until the self-hosted cutover has proven stable, and the client's default server address can be switched, per user, without reinstalling.

The instance relies on managed services rather than hosting them: Redis for rooms, presence, and rate limiting; MongoDB for share metadata, quotas, and indexes; Cloudflare-compatible object storage for file bytes with presigned upload and download URLs and multipart completion. A daily cleanup job runs as a scheduled system service at 03:00 UTC (with a small random delay and catch-up after downtime), replacing the old hosted cron, and accepts the same credentials used for manual cleanup. Share requests additionally trigger opportunistic background cleanup so expired content does not linger until the daily sweep.

Configuration flows in layers for the terminal client: built-in defaults, then a per-user configuration file, then environment variables. The configurable settings are the signaling server address, the default share expiry, a standing download directory, the local-network port, whether upload chunk sizing adapts to measured bandwidth, and whether QR codes display automatically, always, or never. Environment variables cover those and additionally the microphone backend (automatic, a synthesized test tone, or a specific device), a speaker capture hook used in testing, the WebRTC STUN server list, and a GitHub token for private-repository self-updates. The names are UPLINK_SERVER, UPLINK_EXPIRY, UPLINK_DOWNLOAD_DIR, UPLINK_LAN_PORT, UPLINK_ADAPTIVE_CHUNKS, and UPLINK_SHOW_QR for the settings above, UPLINK_MIC and UPLINK_SPEAKER_OUT for audio capture, UPLINK_STUN for the server list, and GITHUB_TOKEN for private-repository updates. The client stores its identity, resume records, group memberships, and configuration under a user directory with user-only permissions.

On the service side, the environment provides the Redis REST endpoint and token (UPSTASH_REDIS_REST_URL and UPSTASH_REDIS_REST_TOKEN); the MongoDB connection string (MONGODB_URI); the object-storage credentials, bucket, and endpoint (R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET_NAME, R2_ENDPOINT_URL); an IP anonymization secret used to hash client addresses for budgets (IP_ANONYMIZATION_SECRET, required in production); an admin API key for the quota inspection surface (ADMIN_API_KEY); a cron secret accepted by cleanup (CRON_SECRET); a trust-proxy switch that must be enabled behind the reverse proxy (TRUST_PROXY); the application URL used for development mock URLs (APP_URL); log level (LOG_LEVEL); and upload-size and expiry/URL-lifetime settings. Development and test runs can opt into in-memory mock backends when the managed services are absent or placeholder credentials are detected (ALLOW_MOCK_REDIS and FORCE_MOCK_STORAGE), with production refusing mock Redis unless explicitly allowed because it would split room state across instances.

The web surfaces are deliberately small: the front page explains that uploading is terminal-only and links to installation and the dependency-graph page; the graph page renders an interactive dependency map of the project's sources, generated automatically during the build; share pages provide the preview and download experience described earlier; and a bearer-key-protected quota endpoint exposes the storage and operations ledger for operators. The bandwidth-test endpoint serves the measured payload used both by operators and by the client's adaptive chunk sizing.

## Tests, guarantees, and continuous integration

Testing is layered. The terminal client and the shelved relay both have extensive Go unit suites, run with the race detector; the TUI has dedicated tests for its exact-row painting contract across resize sweeps, quote/reply flows, reactions, mentions, unread accounting, hover and mouse hit-testing, group persistence, and invite flows, plus headless engine tests that drive the full chat loop against a fake signaling server. The web side has unit suites for cryptographic helpers (including filename-sanitization parity with the client), Redis mock semantics and production gating, room signaling (creation, roles, bans, inbox, invites, quotas), share lifecycle, request signatures, group administration, and graph generation. End-to-end scripts exercise the transfer matrix (send and receive, folders, encryption, multipart, idempotency, and negative contracts), the signaling lifecycle (create, join, heartbeat, signal, inbox, guards, leave), and a real two-client chat with cross-delivery, roster, departure propagation, and room destruction on the last leave. Manual probes for storage inspection and quota walkthroughs exist but are deliberately not part of automated runs.

Continuous integration runs on every push and pull request: server-side vet and race-enabled tests for both modules, cross-compilation of all release targets, web lint, type-checking, unit suites, and a production build, followed by a full end-to-end matrix against a real database instance and mock object storage behind a single pipeline gate. Every third-party build action is pinned to a full commit for supply-chain safety, with automated dependency updates proposing bumps. Only a failure-only end-to-end server log with short retention is uploaded as an artifact, to stay within account quotas. The release workflow, triggered by version tags, builds the platform archives and a checksums file; both the installers and the self-updater verify against those checksums and fail closed on any mismatch.

The guarantees the project stands behind: message, file, and voice content is unreadable to the service; encrypted shares are unreadable even to storage; verification failures close rather than degrade; upload, expiry, and quota bookkeeping is atomic with clamps so double events cannot corrupt ledgers; rate budgets cannot be reset by rotating usernames; and the interface is provably exact in its rendering at every terminal size the tests sweep.

## Known limitations

These are documented, audit-backed gaps, not hidden ones:

- The upload confirmation step trusts the size the client declares; the quota lifecycle hardening that follows from this is the top item on the fix list.
- Local-network discovery is trust-on-first-use: a local observer sees plaintext announcement metadata (file name, size, hash, certificate fingerprint), and the share code is only weakly hashed in the announcement.
- The WAN peer-to-peer receive path is non-functional; treat that mode as a laboratory feature.
- Download streams are unbounded in size on all three transfer paths; there is no per-transfer byte cap beyond the upload limits.
- Offline message boxes have no forward secrecy (static keys by design); they exist only as the fallback path.
- There is no key-continuity store: if a device identity is regenerated, peers see a new device, and comparing safety codes out of band is the only defense against a swapped key.
- Group renames and description edits reach other members only when they rejoin, because the service exposes no group-metadata read endpoint.
- Group passwords are stored in plain text in the client's user-only local membership file (the documented tradeoff that enables automatic re-join).
- The client does not display a typing indicator even though the protocol supports one; messages arrive without a "composing" hint.
- Voice capture on macOS and Windows depends on the external multimedia tool; Linux capture prefers the native sound interface.
- The headless plain chat mode has no history, no files browsing beyond automatic saving, and a minimal command set.

## The shelved relay

An alternative always-on WebSocket relay is preserved in the project as a documented fallback for a host that can keep long-lived processes running — a feature the current serverless-style signaling plane deliberately does not need, since it holds state in managed Redis and pushes transport to peer-to-peer connections. The relay is not deployed, is not built by any release workflow, and is exercised only by its own test suite. Status: shelved, kept for reference, not part of any user-facing path.
