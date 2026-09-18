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
//   room:{code}                 hash  {passwordHash?, createdAt, creator?}
//   room:{code}:members         hash  username -> JSON {pubkey, beat, peerId?, addrs?, role?}
//   room:{code}:banned          hash  username -> JSON {at, by} (kicked users; dies with the room)
//   room:{code}:epoch           string  monotonic roster generation (join/leave/prune bumps)
//   room:{code}:sig:{username}  list  JSON notes {from, type, payload, ts} (5 min TTL)
//   room:{code}:inbox:{username} hash msgId -> JSON box {msgId, from, kind, payload, ts} (1h TTL)
//
// Roles: creator (room maker, rank 2) > admin (rank 1) > member (rank 0).
// Moderation (kick/grant) requires strictly higher rank; creator is
// immutable. Pre-roles rooms (no creator in meta, no role on members)
// default to member and refuse moderation until recreated.
//
// Two delivery paths share the inbox: offline message boxes AND the
// serverless fallback relay (when P2P fails). Both are ciphertext the
// server cannot read; both die on ACK or TTL.

export const USERNAME_RE = /^[a-zA-Z0-9_]{3,20}$/;
export const ROOM_CODE_RE = /^[0-9]{6}$/;

export const ROOM_TTL_SEC = 24 * 3600; // sliding safety net; rooms die on empty, not time
export const SIG_TTL_SEC = 5 * 60; // key-level sliding TTL, refreshed per deposit
export const INBOX_TTL_SEC = 60 * 60; // key-level sliding TTL: refreshed per deposit, so a trickling inbox outlives idle boxes
export const PRESENCE_TIMEOUT_MS = 20 * 1000; // ~4 missed 5s heartbeats = offline (fast join/leave visibility; beats are cheap pipelined reads)

export const MAX_SIG_QUEUE = 150; // signaling notes queued per user (media sessions fan out: announce + 3-way handshake + healing retries per peer — 50 dropped chat notes under a room video)
export const MAX_INBOX = 200; // undelivered boxes held per user
export const MAX_SIG_PAYLOAD = 16 * 1024; // SDP/ICE notes are small
export const MAX_BOX_PAYLOAD = 256 * 1024; // fallback relay: text + small files only

export const CREATE_LIMIT_PER_HOUR = 10; // room creations per IP
export const SEND_LIMIT_PER_WINDOW = 120; // signal/inbox sends per IP per 5 min
export const SEND_WINDOW_SEC = 5 * 60;
export const JOIN_LIMIT_PER_WINDOW = 120; // joins + leaves per IP per 5 min
export const READ_LIMIT_PER_WINDOW = 1200; // two same-user tabs + ack overhead stay under budget // heartbeats + polls + fetches + acks per IP per 5 min (~2/s sustained; normal use ≈0.3/s)

export class RoomError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export type Role = "creator" | "admin" | "member";

const ROLE_RANK: Record<Role, number> = { creator: 2, admin: 1, member: 0 };

export function roleRank(role: Role): number {
  return ROLE_RANK[role] ?? 0;
}

function parseRole(raw: unknown): Role {
  return raw === "creator" || raw === "admin" ? raw : "member";
}

export interface MemberInfo {
  username: string;
  pubkey: string;
  beat: number;
  online: boolean;
  role: Role;
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
const bannedKey = (code: string) => `room:${code}:banned`;
const epochKey = (code: string) => `room:${code}:epoch`;
const sigKey = (code: string, username: string) => `room:${code}:sig:${username}`;
const inboxKey = (code: string, username: string) => `room:${code}:inbox:${username}`;

// bumpEpoch advances the roster generation after any membership change
// (join, leave, stale-prune). Polling clients compare it against their last
// seen value and refresh the roster immediately on change — join visibility
// without shortening the poll cadence (no extra QPS under pressure: the
// epoch rides inside responses the client already fetches).
async function bumpEpoch(code: string): Promise<number> {
  const [epoch] = (await redis.pipeline([
    { cmd: "incr", key: epochKey(code), args: [] },
    { cmd: "expire", key: epochKey(code), args: [ROOM_TTL_SEC] },
  ])) as [number, unknown];
  return epoch;
}

// getEpoch reads the roster generation (0 when the key never existed —
// pre-epoch rooms and brand-new rooms both start there).
export async function getEpoch(code: string): Promise<number> {
  const raw = await redis.get(epochKey(code));
  const n = typeof raw === "string" ? parseInt(raw, 10) : typeof raw === "number" ? raw : 0;
  return Number.isFinite(n) && n > 0 ? n : 0;
}

// bumpEpochBestEffort advances the generation without failing the caller:
// a bump failure after a successful membership write must degrade to
// beat-cadence discovery, never to a 500 for a join that already happened.
async function bumpEpochBestEffort(code: string): Promise<number> {
  try {
    return await bumpEpoch(code);
  } catch {
    return 0;
  }
}

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

// assertUsernameHeader validates the identity header BEFORE it is used for
// rate-budget keying: raw headers are attacker-chosen, and each distinct
// value would otherwise mint a fresh budget bucket (Sybil-able throttles).
export function assertUsernameHeader(v: unknown): asserts v is string {
  assertUsername(v);
}

// isEmptyRecord normalizes the Mock-vs-Upstash hgetall shape at pipelined
// call sites: missing keys yield null (Mock) or {} (Upstash). Bare truthy
// checks read ghosts as live rooms on real Redis.
function isEmptyRecord(o: Record<string, string> | null | undefined): o is null {
  return !o || Object.keys(o).length === 0;
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
  const m = parseStored<{ pubkey?: unknown; beat?: unknown; peerId?: unknown; addrs?: unknown; role?: unknown }>(raw);
  if (!m) return null;
    if (typeof m.pubkey !== "string") return null;
    const beat = typeof m.beat === "number" ? m.beat : 0;
    const info: MemberInfo = {
      username,
      pubkey: m.pubkey,
      beat,
      online: Date.now() - beat <= PRESENCE_TIMEOUT_MS,
      role: parseRole(m.role),
    };
    if (typeof m.peerId === "string") info.peerId = m.peerId;
    if (Array.isArray(m.addrs)) info.addrs = m.addrs.filter((a): a is string => typeof a === "string");
    return info;
}

// Refresh the sliding TTL safety net on room + roster + epoch + bans
// (one round trip). Skipping epoch/bans here would let them decay under an
// active room: the generation would rewind and kicked users could return.
async function touchRoom(code: string): Promise<void> {
  await redis.pipeline([
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: epochKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: bannedKey(code), args: [ROOM_TTL_SEC] },
  ]);
}

export async function roomExists(code: string): Promise<boolean> {
  const meta = await hgetall(roomKey(code));
  return meta !== null;
}

export interface RoomMeta {
  passwordHash: string | null;
  createdAt: string;
  creator: string | null; // room creator (null = pre-roles room: moderation disabled)
}

export async function getRoomMeta(code: string): Promise<RoomMeta | null> {
  const meta = await hgetall(roomKey(code));
  if (!meta) return null;
  // BUGFIX x2: (1) room metadata lives under the single "meta" field, not
  // top-level; (2) real Upstash auto-parses the JSON string into an object,
  // so a typeof-string gate kills it. parseStored tolerates both shapes.
  const parsed = parseStored<{ passwordHash?: unknown; createdAt?: unknown; creator?: unknown }>(
    (meta as Record<string, unknown>).meta
  );
  if (!parsed) return null;
  return {
    passwordHash: typeof parsed.passwordHash === "string" ? parsed.passwordHash : null,
    createdAt: typeof parsed.createdAt === "string" ? parsed.createdAt : "",
    creator: typeof parsed.creator === "string" ? parsed.creator : null,
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
      creator: username,
    }));
    if (created === 0) continue; // collision — regenerate
    // Claim the creator and arm TTLs in one round trip. The hsetnx result
    // tells us if our own username somehow raced us (rollback + retry).
    // The creator owns the room: member role for moderation rank.
    const [claimed] = (await redis.pipeline([
      { cmd: "hsetnx", key: membersKey(code), args: [username, JSON.stringify({ pubkey, beat: Date.now(), role: "creator" })] },
      { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
      { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    ])) as [number, unknown, unknown];
    if (claimed === 0) {
      await redis.pipeline([
        { cmd: "del", key: roomKey(code), args: [] },
        { cmd: "hdel", key: membersKey(code), args: [username] },
      ]);
      continue;
    }
    await indexRoom(code);
    return { sessionId: code };
  }
  throw new RoomError(500, "Failed to generate a unique session code");
}

// Atomically claim a username in a live room. Returns the roster plus the
// roster epoch (bumped by this join) so the joiner seeds its change tracker
// without an extra round trip.
export async function joinRoom(code: string, username: string, pubkey: string): Promise<{ roster: MemberInfo[]; epoch: number }> {
  assertRoomCode(code);
  assertUsername(username);
  assertPubkey(pubkey);

  if (!(await roomExists(code))) {
    throw new RoomError(404, "Session not found");
  }
  // Kicked users stay out while the room lives.
  if (await redis.hget(bannedKey(code), username)) {
    throw new RoomError(403, "You were kicked from this session");
  }
  // Claim + TTLs + fresh roster in one round trip. Joiners start as
  // members; roles are granted explicitly (existing roles survive rejoins
  // via the 409 path below — re-claiming never demotes).
  const [claimed, , , rosterRaw] = (await redis.pipeline([
    { cmd: "hsetnx", key: membersKey(code), args: [username, JSON.stringify({ pubkey, beat: Date.now(), role: "member" })] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "hgetall", key: membersKey(code), args: [] },
  ])) as [number, unknown, unknown, Record<string, string> | null];
  if (claimed === 0) {
    const existing = await redis.hget(membersKey(code), username);
    if (existing && !parseMember(username, existing)) {
      await redis.hset(membersKey(code), username, JSON.stringify({ pubkey, beat: Date.now(), role: "member" }));
      return { roster: rosterFrom(await hgetall(membersKey(code))), epoch: await bumpEpochBestEffort(code) };
    }
    throw new RoomError(409, "Username already taken");
  }
  return { roster: rosterFrom(rosterRaw), epoch: await bumpEpochBestEffort(code) };
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
): Promise<{ roster: MemberInfo[]; epoch: number }> {
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
  if (isEmptyRecord(roomRaw)) {
    throw new RoomError(404, "Session not found");
  }
  if (!membersRaw || !membersRaw[username]) throw new RoomError(403, "Not in this session");
  const current = parseMember(username, membersRaw[username]);
  if (!current) throw new RoomError(403, "Session state corrupt — rejoining");
  const entry: Record<string, unknown> = {
    pubkey: current?.pubkey ?? "",
    beat: Date.now(),
    role: current?.role ?? "member", // beats never demote: role changes only via setRole
  };
  if (patch.peerId !== undefined) entry.peerId = patch.peerId;
  else if (current?.peerId !== undefined) entry.peerId = current.peerId;
  if (patch.addrs !== undefined) entry.addrs = patch.addrs;
  else if (current?.addrs !== undefined) entry.addrs = current.addrs;
  // Beat write + TTLs + roster + epoch in one round trip: the epoch costs
  // no extra QPS, it just rides the response the client already fetches.
  // The epoch TTL refreshes here so active rooms never decay to 0.
  const [, , , , rosterRaw, epochRaw] = (await redis.pipeline([
    { cmd: "hset", key: membersKey(code), args: [username, JSON.stringify(entry)] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: epochKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "hgetall", key: membersKey(code), args: [] },
    { cmd: "get", key: epochKey(code), args: [] },
  ])) as [unknown, unknown, unknown, unknown, Record<string, string> | null, unknown];
  const epoch = typeof epochRaw === "string" ? parseInt(epochRaw, 10) : typeof epochRaw === "number" ? epochRaw : 0;
  return { roster: rosterFrom(rosterRaw), epoch: Number.isFinite(epoch) && epoch > 0 ? epoch : 0 };
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
    await unindexRoom(code);
    ended = true;
  } else {
    await touchRoom(code);
    await bumpEpochBestEffort(code); // survivors must learn the departure ASAP
  }
  return { remaining, ended };
}

// ─── Moderation (creator/admin privileges) ─────────────────────────────────
//
// Rank: creator (2) > admin (1) > member (0). Moderation requires strictly
// higher rank than the target, so nobody can kick/demote themselves,
// peers, or superiors. The creator is immutable; admins are granted and
// revoked by the creator only. Kicks ban the username while the room lives
// (rejoin returns 403) and wipe the target's transient queues.

function requireRole(members: Record<string, string>, username: string): { member: MemberInfo; rank: number } {
  const raw = members[username];
  const member = raw ? parseMember(username, raw) : null;
  if (!member) throw new RoomError(403, "Not in this session");
  return { member, rank: roleRank(member.role) };
}

export async function kickMember(
  code: string,
  actor: string,
  target: string
): Promise<{ roster: MemberInfo[]; epoch: number; remaining: number }> {
  assertRoomCode(code);
  assertUsername(actor);
  assertUsername(target);
  const all = await hgetall(membersKey(code));
  if (!all) throw new RoomError(404, "Session not found");
  const meta = await getRoomMeta(code);
  if (!meta?.creator) throw new RoomError(403, "This room predates roles — recreate it to enable moderation");
  const { rank: actorRank } = requireRole(all, actor);
  const { rank: targetRank } = requireRole(all, target);
  if (actorRank <= targetRank) {
    throw new RoomError(403, actor === target ? "You cannot kick yourself" : "Only a higher rank can kick that user");
  }
  // Best-effort transient cleanup (mirrors leaveRoom): failures must never
  // block the kick itself.
  await redis.pipeline([
    { cmd: "del", key: sigKey(code, target), args: [] },
    { cmd: "del", key: inboxKey(code, target), args: [] },
  ]).catch(() => [] as unknown[]);
  await redis.hdel(membersKey(code), target);
  await redis.pipeline([
    { cmd: "hset", key: bannedKey(code), args: [target, JSON.stringify({ at: Date.now(), by: actor })] },
    { cmd: "expire", key: bannedKey(code), args: [ROOM_TTL_SEC] },
  ]).catch(() => [] as unknown[]);
  const remaining = await redis.hlen(membersKey(code));
  if (remaining === 0) {
    await destroyRoom(code);
    await unindexRoom(code);
    return { roster: [], epoch: 0, remaining: 0 };
  }
  await touchRoom(code);
  const epoch = await bumpEpochBestEffort(code);
  return { roster: rosterFrom(await hgetall(membersKey(code))), epoch, remaining };
}

export async function setRole(
  code: string,
  actor: string,
  target: string,
  role: Role
): Promise<{ roster: MemberInfo[]; epoch: number }> {
  assertRoomCode(code);
  assertUsername(actor);
  assertUsername(target);
  if (role !== "admin" && role !== "member") {
    throw new RoomError(400, "Role must be admin or member");
  }
  const all = await hgetall(membersKey(code));
  if (!all) throw new RoomError(404, "Session not found");
  const meta = await getRoomMeta(code);
  if (!meta?.creator) throw new RoomError(403, "This room predates roles — recreate it to enable moderation");
  const { member: actorMember } = requireRole(all, actor);
  if (actorMember.role !== "creator") {
    throw new RoomError(403, "Only the room creator can grant admin");
  }
  if (actor === target) throw new RoomError(403, "You cannot change your own role");
  const targetRaw = all[target];
  const targetMember = targetRaw ? parseMember(target, targetRaw) : null;
  if (!targetMember) throw new RoomError(404, "User is not in this session");
  if (targetMember.role === "creator") throw new RoomError(403, "The creator role cannot be changed");
  if (targetMember.role === role) return { roster: rosterFrom(all), epoch: await getEpoch(code) };
  const entry: Record<string, unknown> = {
    pubkey: targetMember.pubkey,
    beat: Date.now(),
    role,
  };
  if (targetMember.peerId !== undefined) entry.peerId = targetMember.peerId;
  if (targetMember.addrs !== undefined) entry.addrs = targetMember.addrs;
  await redis.hset(membersKey(code), target, JSON.stringify(entry));
  await touchRoom(code);
  const epoch = await bumpEpochBestEffort(code);
  return { roster: rosterFrom(await hgetall(membersKey(code))), epoch };
}

export async function destroyRoom(code: string): Promise<void> {
  const members = (await hgetall(membersKey(code))) || {};
  const keys = [roomKey(code), membersKey(code), epochKey(code), bannedKey(code)];
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
  if (isEmptyRecord(membersRaw)) throw new RoomError(404, "Session not found");
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

// Drain my signaling queue with one atomic pop per note. The old
// LRANGE+DEL pipeline dropped notes deposited between the read and the
// clear; pops are atomic, so concurrent deposits stay queued for the next
// drain instead of vanishing. RPOP yields newest-first: reversed back to
// send order. Bounded iterations; leftovers wait for the next poll.
export async function drainSignals(code: string, username: string): Promise<SignalNote[]> {
  assertRoomCode(code);
  assertUsername(username);
  await requireMember(code, username);
  const raw: string[] = [];
  for (let i = 0; i < MAX_SIG_QUEUE + 16; i++) {
    const item = await redis.rpop(sigKey(code, username));
    if (item == null) break;
    raw.push(item);
  }
  raw.reverse();
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
  // Recipient check + inbox depth + existing-field read in one round trip.
  const [membersRaw, depth, existing] = (await redis.pipeline([
    { cmd: "hgetall", key: membersKey(code), args: [] },
    { cmd: "hlen", key: inboxKey(code, to), args: [] },
    { cmd: "hget", key: inboxKey(code, to), args: [msgId] },
  ])) as [Record<string, string> | null, number, string | null];
  if (isEmptyRecord(membersRaw)) throw new RoomError(404, "Session not found");
  if (!membersRaw[from]) throw new RoomError(403, "Not in this session");
  if (!membersRaw[to]) throw new RoomError(404, "Recipient is not in this session");
  if (existing) {
    const prev = parseStored<{ from?: unknown }>(existing);
    if (!prev || prev.from !== from) throw new RoomError(409, "Message ID already claimed by another sender");
    // Idempotent retry of the same box: bypass the depth check (it counts
    // fields, which this write does not grow) or retries can never succeed.
  } else if (depth >= MAX_INBOX) {
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

const FETCH_BOX_CAP = 50; // one fetch never exceeds ~50 boxes; remainder self-paginates next poll
export async function fetchBoxes(code: string, username: string): Promise<{ boxes: InboxBox[]; epoch: number }> {
  assertRoomCode(code);
  assertUsername(username);
  await requireMember(code, username);
  const all = await hgetall(inboxKey(code, username));
  const epoch = await getEpoch(code); // piggyback: membership changes surface on the 2s inbox cadence
  if (!all) return { boxes: [], epoch };
  const out: InboxBox[] = [];
  const corrupt: string[] = [];
  for (const [field, raw] of Object.entries(all)) {
    const b = parseStored<InboxBox>(raw);
    if (b && typeof b.msgId === "string" && typeof b.from === "string" && typeof b.payload === "string") {
      out.push(b);
    } else {
      corrupt.push(field); // reap: unparseable fields would squat cap slots forever
    }
  }
  if (corrupt.length > 0) await redis.hdel(inboxKey(code, username), ...corrupt).catch(() => 0);
  out.sort((a, b) => a.ts - b.ts);
  return { boxes: out.slice(0, FETCH_BOX_CAP), epoch };
}

// Explicit ACK: delete exactly the acknowledged boxes. Fetch-then-ACK (not
// fetch-and-clear) so a client crash between fetch and processing loses
// nothing — unacked boxes are simply returned again.
export async function ackBoxes(code: string, username: string, ids: unknown): Promise<{ removed: number; skipped: number }> {
  assertRoomCode(code);
  assertUsername(username);
  if (!Array.isArray(ids) || ids.length === 0 || ids.length > 500) {
    throw new RoomError(400, "ids must be an array of 1-500 message IDs");
  }
  const clean = [...new Set(ids.filter((id): id is string => typeof id === "string" && id.length > 0 && id.length <= 128))];
  if (clean.length === 0) throw new RoomError(400, "No valid message IDs");
  await requireMember(code, username);
  const removed = await redis.hdel(inboxKey(code, username), ...clean);
  return { removed, skipped: ids.length - clean.length };
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
  await redis.lrem(ROOM_INDEX, 0, code); // atomic; the read-modify-write it replaces wiped concurrent indexRoom pushes
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
      if (isEmptyRecord(meta)) {
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
      if (stale.length > 0) await bumpEpochBestEffort(code); // one bump per swept room
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
