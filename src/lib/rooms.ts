import crypto from "crypto";
import { redis } from "./redis";
import { anonymizeIp } from "./crypto";

// ─── Signaling-plane state machine (Vercel-native architecture) ─────────────
//
// Rooms live in Redis ONLY. There is no permanent record: a room exists from
// creation until its last member leaves, plus a sliding 24h TTL as a safety
// net against crashed clients that never send leave. Member rosters carry
// each device's long-lived X25519 static public key so peers can E2E-encrypt
// without any global key directory (code-only rooms need nothing else).
//
// Key layout:
//   room:{code}                 hash  {passwordHash?, createdAt}
//   room:{code}:members         hash  username -> JSON {pubkey, beat, peerId?, addrs?}
//   room:{code}:sig:{username}  list  JSON notes {from, type, payload, ts} (5 min TTL)
//   room:{code}:inbox:{username} hash msgId -> JSON box {msgId, from, kind, payload, ts} (1h TTL)
//
// Two delivery paths share the inbox: offline message boxes AND the
// serverless fallback relay (when P2P fails). Both are ciphertext the
// server cannot read; both die on ACK or TTL.

export const USERNAME_RE = /^[a-zA-Z0-9_]{3,20}$/;
export const ROOM_CODE_RE = /^[0-9]{6}$/;

export const ROOM_TTL_SEC = 24 * 3600; // sliding safety net; rooms die on empty, not time
export const SIG_TTL_SEC = 5 * 60; // connection notes live minutes
export const INBOX_TTL_SEC = 60 * 60; // frozen spec: undelivered boxes evaporate after 1h
export const PRESENCE_TIMEOUT_MS = 20 * 1000; // ~4 missed 5s heartbeats = offline (fast join/leave visibility; beats are cheap pipelined reads)

export const MAX_SIG_QUEUE = 50; // signaling notes queued per user
export const MAX_INBOX = 200; // undelivered boxes held per user
export const MAX_SIG_PAYLOAD = 16 * 1024; // SDP/ICE notes are small
export const MAX_BOX_PAYLOAD = 256 * 1024; // fallback relay: text + small files only

export const CREATE_LIMIT_PER_HOUR = 10; // room creations per IP
export const SEND_LIMIT_PER_WINDOW = 120; // signal/inbox sends per IP per 5 min
export const SEND_WINDOW_SEC = 5 * 60;
export const JOIN_LIMIT_PER_WINDOW = 120; // joins + leaves per IP per 5 min
export const READ_LIMIT_PER_WINDOW = 600; // heartbeats + polls + fetches + acks per IP per 5 min (~2/s sustained; normal use ≈0.3/s)

export class RoomError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export interface MemberInfo {
  username: string;
  pubkey: string;
  beat: number;
  online: boolean;
  peerId?: string;
  addrs?: string[];
}

export interface SignalNote {
  from: string;
  type: string;
  payload: string;
  ts: number;
}

export interface InboxBox {
  msgId: string;
  from: string;
  kind: string;
  payload: string;
  ts: number;
}

const roomKey = (code: string) => `room:${code}`;
const membersKey = (code: string) => `room:${code}:members`;
const sigKey = (code: string, username: string) => `room:${code}:sig:${username}`;
const inboxKey = (code: string, username: string) => `room:${code}:inbox:${username}`;

// hgetall normalizes backend differences: MockRedis returns null for
// missing keys, but real Upstash Redis returns an empty object. Without
// this, roomExists() is true for ghost rooms in production (members get
// claimed into rooms with no metadata, 404s become 403s, sweeps leak).
async function hgetall(key: string): Promise<Record<string, string> | null> {
  const v = await redis.hgetall(key);
  if (!v || Object.keys(v).length === 0) return null;
  return v;
}

export function generateRoomCode(): string {
  return String(crypto.randomInt(0, 1000000)).padStart(6, "0");
}

export function assertUsername(username: unknown): asserts username is string {
  if (typeof username !== "string" || !USERNAME_RE.test(username)) {
    throw new RoomError(400, "Username must be 3-20 alphanumeric/underscore characters");
  }
}

export function assertRoomCode(code: unknown): asserts code is string {
  if (typeof code !== "string" || !ROOM_CODE_RE.test(code)) {
    throw new RoomError(400, "Invalid session code");
  }
}

// X25519 static public keys are 32 bytes; clients send base64.
export function assertPubkey(pubkey: unknown): asserts pubkey is string {
  if (typeof pubkey !== "string" || pubkey.length === 0 || pubkey.length > 128) {
    throw new RoomError(400, "Identity public key is required");
  }
  let raw: Buffer;
  try {
    raw = Buffer.from(pubkey, "base64");
  } catch {
    throw new RoomError(400, "Identity public key must be base64");
  }
  if (raw.length !== 32) {
    throw new RoomError(400, "Identity public key must decode to 32 bytes (X25519 static)");
  }
}

// parseStored decodes a value read back from Redis, tolerating backend
// differences: MockRedis returns our JSON strings verbatim, but the real
// Upstash client auto-parses JSON-looking strings into objects. Without
// this, every read on real Upstash mistypes (objects where strings were
// stored) and rooms/members/boxes silently vanish — while MockRedis tests
// stay green. Centralize ALL stored-JSON decoding here.
export function parseStored<T>(raw: unknown): T | null {
  try {
    if (raw !== null && typeof raw === "object") return raw as T;
    if (typeof raw !== "string") return null;
    return JSON.parse(raw) as T;
  } catch {
    return null;
  }
}

function parseMember(username: string, raw: unknown): MemberInfo | null {
  const m = parseStored<{ pubkey?: unknown; beat?: unknown; peerId?: unknown; addrs?: unknown }>(raw);
  if (!m) return null;
    if (typeof m.pubkey !== "string") return null;
    const beat = typeof m.beat === "number" ? m.beat : 0;
    const info: MemberInfo = {
      username,
      pubkey: m.pubkey,
      beat,
      online: Date.now() - beat <= PRESENCE_TIMEOUT_MS,
    };
    if (typeof m.peerId === "string") info.peerId = m.peerId;
    if (Array.isArray(m.addrs)) info.addrs = m.addrs.filter((a): a is string => typeof a === "string");
    return info;
}

// Refresh the sliding TTL safety net on room + roster (one round trip).
async function touchRoom(code: string): Promise<void> {
  await redis.pipeline([
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
  ]);
}

export async function roomExists(code: string): Promise<boolean> {
  const meta = await hgetall(roomKey(code));
  return meta !== null;
}

export async function getRoomMeta(code: string): Promise<{ passwordHash: string | null; createdAt: string } | null> {
  const meta = await hgetall(roomKey(code));
  if (!meta) return null;
  // BUGFIX x2: (1) room metadata lives under the single "meta" field, not
  // top-level; (2) real Upstash auto-parses the JSON string into an object,
  // so a typeof-string gate kills it. parseStored tolerates both shapes.
  const parsed = parseStored<{ passwordHash?: unknown; createdAt?: unknown }>(
    (meta as Record<string, unknown>).meta
  );
  if (!parsed) return null;
  return {
    passwordHash: typeof parsed.passwordHash === "string" ? parsed.passwordHash : null,
    createdAt: typeof parsed.createdAt === "string" ? parsed.createdAt : "",
  };
}

export async function checkCreateLimit(ipHash: string): Promise<void> {
  const key = `rate:create:${ipHash}`;
  const hits = await redis.incr(key);
  if (hits === 1) await redis.expire(key, 3600);
  if (hits > CREATE_LIMIT_PER_HOUR) {
    throw new RoomError(429, "Too many rooms created. Try again later.");
  }
}

export async function checkSendLimit(kind: "sig" | "inbox", ipHash: string): Promise<void> {
  const key = `rate:${kind}:${ipHash}`;
  const hits = await redis.incr(key);
  if (hits === 1) await redis.expire(key, SEND_WINDOW_SEC);
  if (hits > SEND_LIMIT_PER_WINDOW) {
    throw new RoomError(429, "Too many requests. Slow down and try again.");
  }
}

// checkReadLimit budgets the high-frequency read path (heartbeats, signal
// drains, inbox fetches/acks). Members hit these constantly, so the budget
// is generous — it exists to stop floods, not normal use.
export async function checkReadLimit(ipHash: string): Promise<void> {
  const key = `rate:read:${ipHash}`;
  const hits = await redis.incr(key);
  if (hits === 1) await redis.expire(key, SEND_WINDOW_SEC);
  if (hits > READ_LIMIT_PER_WINDOW) {
    throw new RoomError(429, "Too many requests. Slow down and try again.");
  }
}

// checkJoinLimit budgets membership mutations (joins + leaves). Joining is
// otherwise an unbounded Redis-op faucet for any room member.
export async function checkJoinLimit(ipHash: string): Promise<void> {
  const key = `rate:join:${ipHash}`;
  const hits = await redis.incr(key);
  if (hits === 1) await redis.expire(key, SEND_WINDOW_SEC);
  if (hits > JOIN_LIMIT_PER_WINDOW) {
    throw new RoomError(429, "Too many requests. Slow down and try again.");
  }
}

export function clientIpHash(req: Request): string {
  const raw = req.headers.get("x-forwarded-for") || "127.0.0.1";
  const ip = raw.split(",")[0].trim() || "127.0.0.1";
  return anonymizeIp(ip);
}

// scopedBudgetKey extends a budget key with the caller's username so members
// behind one NAT (office, dorm) don't share a single throttle bucket.
// Usernames can't contain ":" (validated charset), so the composition is
// unambiguous. Join/leave/create deliberately stay IP-scoped: budgets that
// gate Sybil-able actions must not be dodgeable by minting names.
export function scopedBudgetKey(req: Request, username: string): string {
  return clientIpHash(req) + ":" + username;
}

export async function createRoom(
  username: string,
  pubkey: string,
  passwordHash: string | null
): Promise<{ sessionId: string }> {
  assertUsername(username);
  assertPubkey(pubkey);

  for (let attempt = 0; attempt < 10; attempt++) {
    const code = generateRoomCode();
    const created = await redis.hsetnx(roomKey(code), "meta", JSON.stringify({
      passwordHash,
      createdAt: new Date().toISOString(),
    }));
    if (created === 0) continue; // collision — regenerate
    // Claim the creator and arm TTLs in one round trip. The hsetnx result
    // tells us if our own username somehow raced us (rollback + retry).
    const [claimed] = (await redis.pipeline([
      { cmd: "hsetnx", key: membersKey(code), args: [username, JSON.stringify({ pubkey, beat: Date.now() })] },
      { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
      { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    ])) as [number, unknown, unknown];
    if (claimed === 0) {
      // Vanishingly unlikely (fresh code, same username raced itself);
      // roll back and retry with a new code.
      await redis.del(roomKey(code));
      continue;
    }
    await indexRoom(code);
    return { sessionId: code };
  }
  throw new RoomError(500, "Failed to generate a unique session code");
}

// Atomically claim a username in a live room. Returns the roster.
export async function joinRoom(code: string, username: string, pubkey: string): Promise<MemberInfo[]> {
  assertRoomCode(code);
  assertUsername(username);
  assertPubkey(pubkey);

  if (!(await roomExists(code))) {
    throw new RoomError(404, "Session not found");
  }
  // Claim + TTLs + fresh roster in one round trip.
  const [claimed, , , rosterRaw] = (await redis.pipeline([
    { cmd: "hsetnx", key: membersKey(code), args: [username, JSON.stringify({ pubkey, beat: Date.now() })] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "hgetall", key: membersKey(code), args: [] },
  ])) as [number, unknown, unknown, Record<string, string> | null];
  if (claimed === 0) {
    throw new RoomError(409, "Username already taken");
  }
  return rosterFrom(rosterRaw);
}

export async function getRoster(code: string): Promise<MemberInfo[]> {
  const all = await hgetall(membersKey(code));
  return rosterFrom(all);
}

// rosterFrom parses a members hash (null = no members). Shared by getRoster
// and the pipelined join path so both shapes stay identical.
function rosterFrom(all: Record<string, string> | null): MemberInfo[] {
  if (!all) return [];
  const out: MemberInfo[] = [];
  for (const [username, raw] of Object.entries(all)) {
    const m = parseMember(username, raw);
    if (m) out.push(m);
  }
  out.sort((a, b) => (a.username < b.username ? -1 : 1));
  return out;
}

async function requireMember(code: string, username: string): Promise<Record<string, string>> {
  const all = await hgetall(membersKey(code));
  if (!all) throw new RoomError(404, "Session not found");
  if (!all[username]) throw new RoomError(403, "Not in this session");
  return all;
}

export async function heartbeat(
  code: string,
  username: string,
  patch: { peerId?: string; addrs?: string[] }
): Promise<MemberInfo[]> {
  assertRoomCode(code);
  assertUsername(username);
  if (patch.peerId !== undefined && (typeof patch.peerId !== "string" || patch.peerId.length > 256)) {
    throw new RoomError(400, "peerId must be a string of at most 256 characters");
  }
  if (patch.addrs !== undefined) {
    if (!Array.isArray(patch.addrs) || patch.addrs.length > 32 ||
        !patch.addrs.every((a) => typeof a === "string" && a.length <= 512)) {
      throw new RoomError(400, "addrs must be an array of at most 32 strings (each at most 512 chars)");
    }
  }
  // Room + roster in one round trip; the roster read doubles as the
  // membership check (missing room or user both surface below).
  const [roomRaw, membersRaw] = (await redis.pipeline([
    { cmd: "hgetall", key: roomKey(code), args: [] },
    { cmd: "hgetall", key: membersKey(code), args: [] },
  ])) as [Record<string, string> | null, Record<string, string> | null];
  if (!roomRaw) {
    throw new RoomError(404, "Session not found");
  }
  if (!membersRaw || !membersRaw[username]) throw new RoomError(403, "Not in this session");
  const current = parseMember(username, membersRaw[username]);
  const entry: Record<string, unknown> = {
    pubkey: current?.pubkey ?? "",
    beat: Date.now(),
  };
  if (patch.peerId !== undefined) entry.peerId = patch.peerId;
  else if (current?.peerId !== undefined) entry.peerId = current.peerId;
  if (patch.addrs !== undefined) entry.addrs = patch.addrs;
  else if (current?.addrs !== undefined) entry.addrs = current.addrs;
  const [, , , rosterRaw] = (await redis.pipeline([
    { cmd: "hset", key: membersKey(code), args: [username, JSON.stringify(entry)] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "hgetall", key: membersKey(code), args: [] },
  ])) as [unknown, unknown, unknown, Record<string, string> | null];
  return rosterFrom(rosterRaw);
}

// Remove a member. When the room empties, destroy it immediately — rooms
// live until the last user leaves, never by timeout.
export async function leaveRoom(code: string, username: string): Promise<{ remaining: number; ended: boolean }> {
  assertRoomCode(code);
  assertUsername(username);
  const all = await hgetall(membersKey(code));
  if (!all || !all[username]) {
    // Idempotent leave: already gone still reports the true count.
    const count = all ? Object.keys(all).length : 0;
    return { remaining: count, ended: count === 0 };
  }
  // Best-effort cleanup of this member's transient queues (their failure
  // must never block the leave itself).
  await redis.pipeline([
    { cmd: "del", key: sigKey(code, username), args: [] },
    { cmd: "del", key: inboxKey(code, username), args: [] },
  ]).catch(() => [] as unknown[]);
  await redis.hdel(membersKey(code), username);
  const remaining = await redis.hlen(membersKey(code));
  let ended = false;
  if (remaining === 0) {
    await destroyRoom(code);
    ended = true;
  } else {
    await touchRoom(code);
  }
  return { remaining, ended };
}

export async function destroyRoom(code: string): Promise<void> {
  const members = (await hgetall(membersKey(code))) || {};
  const keys = [roomKey(code), membersKey(code)];
  for (const username of Object.keys(members)) {
    keys.push(sigKey(code, username), inboxKey(code, username));
  }
  if (keys.length === 0) return;
  await redis.pipeline(keys.map((key) => ({ cmd: "del" as const, key, args: [] as (string | number)[] }))).catch(() => [] as unknown[]);
}

// ─── Signaling notes (WebRTC SDP/ICE rendezvous) ────────────────────────────

export async function depositSignal(
  code: string,
  from: string,
  to: string,
  type: string,
  payload: string
): Promise<void> {
  assertRoomCode(code);
  assertUsername(from);
  assertUsername(to);
  if (typeof type !== "string" || type.length === 0 || type.length > 32) {
    throw new RoomError(400, "Signal type is required (max 32 chars)");
  }
  if (typeof payload !== "string" || payload.length === 0 || payload.length > MAX_SIG_PAYLOAD) {
    throw new RoomError(400, `Signal payload must be 1-${MAX_SIG_PAYLOAD} chars`);
  }
  if (from === to) throw new RoomError(400, "Cannot signal yourself");
  // Recipient check + queue depth in one round trip.
  const [membersRaw, depth] = (await redis.pipeline([
    { cmd: "hgetall", key: membersKey(code), args: [] },
    { cmd: "llen", key: sigKey(code, to), args: [] },
  ])) as [Record<string, string> | null, number];
  if (!membersRaw) throw new RoomError(404, "Session not found");
  if (!membersRaw[from]) throw new RoomError(403, "Not in this session");
  if (!membersRaw[to]) throw new RoomError(404, "Recipient is not in this session");
  if (depth >= MAX_SIG_QUEUE) {
    throw new RoomError(429, "Recipient signaling queue is full. Try again shortly.");
  }
  await redis.pipeline([
    { cmd: "rpush", key: sigKey(code, to), args: [JSON.stringify({ from, type, payload, ts: Date.now() })] },
    { cmd: "expire", key: sigKey(code, to), args: [SIG_TTL_SEC] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
  ]);
}

// Drain (fetch-and-clear) my signaling queue. At-least-once duplicates are
// possible under races; receivers dedup by (from, type, ts).
export async function drainSignals(code: string, username: string): Promise<SignalNote[]> {
  assertRoomCode(code);
  assertUsername(username);
  await requireMember(code, username);
  // Read-and-clear in one round trip (ordered server-side). Duplicates
  // under races are safe: receivers dedup by (from, type, ts).
  const [raw] = (await redis.pipeline([
    { cmd: "lrange", key: sigKey(code, username), args: [0, -1] },
    { cmd: "del", key: sigKey(code, username), args: [] },
  ])) as [string[], unknown];
  if (raw.length === 0) return [];
  const out: SignalNote[] = [];
  for (const item of raw) {
    const n = parseStored<SignalNote>(item);
    if (n && typeof n.from === "string" && typeof n.type === "string" && typeof n.payload === "string") {
      out.push(n);
    }
    // else: skip corrupt entries
  }
  return out;
}

// ─── Offline/fallback inbox (ciphertext boxes, 1h TTL) ──────────────────────

export async function depositBox(
  code: string,
  from: string,
  to: string,
  msgId: string,
  kind: string,
  payload: string
): Promise<void> {
  assertRoomCode(code);
  assertUsername(from);
  assertUsername(to);
  if (typeof msgId !== "string" || msgId.length === 0 || msgId.length > 128) {
    throw new RoomError(400, "msgId is required (max 128 chars)");
  }
  if (typeof kind !== "string" || kind.length === 0 || kind.length > 32) {
    throw new RoomError(400, "kind is required (max 32 chars)");
  }
  if (typeof payload !== "string" || payload.length === 0 || payload.length > MAX_BOX_PAYLOAD) {
    throw new RoomError(400, `payload must be 1-${MAX_BOX_PAYLOAD} chars`);
  }
  if (from === to) throw new RoomError(400, "Cannot box a message to yourself");
  // Recipient check + inbox depth in one round trip.
  const [membersRaw, depth] = (await redis.pipeline([
    { cmd: "hgetall", key: membersKey(code), args: [] },
    { cmd: "hlen", key: inboxKey(code, to), args: [] },
  ])) as [Record<string, string> | null, number];
  if (!membersRaw) throw new RoomError(404, "Session not found");
  if (!membersRaw[from]) throw new RoomError(403, "Not in this session");
  if (!membersRaw[to]) throw new RoomError(404, "Recipient is not in this session");
  if (depth >= MAX_INBOX) {
    throw new RoomError(429, "Recipient inbox is full. Try again later.");
  }
  // HSET is idempotent on msgId: retries never duplicate. One round trip
  // for the write + all TTL refreshes.
  await redis.pipeline([
    { cmd: "hset", key: inboxKey(code, to), args: [msgId, JSON.stringify({ msgId, from, kind, payload, ts: Date.now() })] },
    { cmd: "expire", key: inboxKey(code, to), args: [INBOX_TTL_SEC] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
  ]);
}

export async function fetchBoxes(code: string, username: string): Promise<InboxBox[]> {
  assertRoomCode(code);
  assertUsername(username);
  await requireMember(code, username);
  const all = await hgetall(inboxKey(code, username));
  if (!all) return [];
  const out: InboxBox[] = [];
  for (const raw of Object.values(all)) {
    const b = parseStored<InboxBox>(raw);
    if (b && typeof b.msgId === "string" && typeof b.from === "string" && typeof b.payload === "string") {
      out.push(b);
    }
    // else: skip corrupt entries
  }
  out.sort((a, b) => a.ts - b.ts);
  return out;
}

// Explicit ACK: delete exactly the acknowledged boxes. Fetch-then-ACK (not
// fetch-and-clear) so a client crash between fetch and processing loses
// nothing — unacked boxes are simply returned again.
export async function ackBoxes(code: string, username: string, ids: unknown): Promise<{ removed: number }> {
  assertRoomCode(code);
  assertUsername(username);
  if (!Array.isArray(ids) || ids.length === 0 || ids.length > 500) {
    throw new RoomError(400, "ids must be an array of 1-500 message IDs");
  }
  const clean = [...new Set(ids.filter((id): id is string => typeof id === "string" && id.length > 0 && id.length <= 128))];
  if (clean.length === 0) throw new RoomError(400, "No valid message IDs");
  await requireMember(code, username);
  const removed = await redis.hdel(inboxKey(code, username), ...clean);
  return { removed };
}

// Sweep: drop members whose heartbeat lapsed, destroy emptied rooms. Runs
// on cron; per-room isolation so one corrupt room cannot abort the sweep.
// At most SWEEP_BATCH rooms per run (Vercel timeout safety); the rest wait
// for the next tick. Rooms are tracked in a bounded global index
// (serverless Redis has no cheap keyscan); entries for vanished rooms are
// pruned as encountered.
const ROOM_INDEX = "room:index";
const SWEEP_BATCH = 50;

export async function indexRoom(code: string): Promise<void> {
  await redis.pipeline([
    { cmd: "rpush", key: ROOM_INDEX, args: [code] },
    { cmd: "ltrim", key: ROOM_INDEX, args: [-5000, -1] },
  ]);
}

async function unindexRoom(code: string): Promise<void> {
  const all = await redis.lrange(ROOM_INDEX, 0, -1);
  const kept = all.filter((c) => c !== code);
  await redis.del(ROOM_INDEX);
  if (kept.length > 0) await redis.rpush(ROOM_INDEX, ...kept);
}

export async function sweepRooms(): Promise<{ processed: number; prunedMembers: number; destroyed: number }> {
  const codes = [...new Set(await redis.lrange(ROOM_INDEX, 0, -1))];
  // Bound the sweep: Vercel functions time out, and the per-room work is
  // several round trips. Unprocessed rooms wait for the next cron tick;
  // last-leave destroy (the primary path) is unaffected. The global index
  // itself is capped at 5000, so backlog is structurally bounded.
  const batch = codes.slice(0, SWEEP_BATCH);
  let processed = 0;
  let prunedMembers = 0;
  let destroyed = 0;
  for (const code of batch) {
    try {
      if (!ROOM_CODE_RE.test(code)) {
        await unindexRoom(code);
        continue;
      }
      // Room meta + roster in one round trip.
      const [meta, membersRaw] = (await redis.pipeline([
        { cmd: "hgetall", key: roomKey(code), args: [] },
        { cmd: "hgetall", key: membersKey(code), args: [] },
      ])) as [Record<string, string> | null, Record<string, string> | null];
      if (!meta) {
        await unindexRoom(code); // room key gone (TTL) — drop index entry
        continue;
      }
      const members = membersRaw || {};
      const stale: string[] = [];
      for (const [username, raw] of Object.entries(members)) {
        const m = parseMember(username, raw);
        if (!m || !m.online) stale.push(username);
      }
      for (const username of stale) {
        await redis.pipeline([
          { cmd: "hdel", key: membersKey(code), args: [username] },
          { cmd: "del", key: sigKey(code, username), args: [] },
          { cmd: "del", key: inboxKey(code, username), args: [] },
        ]).catch(() => [] as unknown[]);
        prunedMembers++;
      }
      const remaining = await redis.hlen(membersKey(code));
      if (remaining === 0) {
        await destroyRoom(code);
        await unindexRoom(code);
        destroyed++;
      }
      processed++;
    } catch {
      // per-room isolation: a corrupt room never aborts the sweep
      processed++;
    }
  }
  return { processed, prunedMembers, destroyed };
}
