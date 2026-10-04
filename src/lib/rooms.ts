import crypto from "crypto";
import { redis, type PipeOp } from "./redis";
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
//   room:{code}:reactions       hash  {msgId}|{emoji}|{username} -> "1" (room-lifetime TTL)
//   room:{code}:invites         hash  invitee username -> JSON {by, at, groupName} (pending invites; room-lifetime TTL)
//   user:{username}:invites     hash  room code -> "1" (per-user pending-invite index; capped, room-lifetime TTL)
//
// Meta extension (group rooms): the room hash's JSON doc carries optional
// groupName (1-64), groupDesc (0-256), maxMembers (int >= 2, or null/absent
// = unlimited) and parentCode (the main room a group branches from).
//
// Roles: creator (main admin, rank 2) > admin (rank 1) > member (rank 0).
// Kick: creator kicks anyone but self; admins kick members only; members
// kick nobody; the creator cannot be kicked. Grant/revoke: creator only.
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
// B54 FIX (finding 5): the inbox is byte-budgeted, not just count-budgeted.
// Previously 200 boxes × 256KB payloads allowed ~51MB of ciphertext per
// victim recipient (a flood fills the 1h inbox TTL with full-capacity
// boxes). Now 50 boxes × 128KB ≈ 6.4MB worst case per recipient — one
// fetch returns the whole inbox (FETCH_BOX_CAP = 50), and the ACK path
// stays free so victims can always clear it.
export const MAX_INBOX = 50; // undelivered boxes held per user
export const MAX_SIG_PAYLOAD = 16 * 1024; // SDP/ICE notes are small
export const MAX_BOX_PAYLOAD = 128 * 1024; // fallback relay: text + small files only

export const CREATE_LIMIT_PER_HOUR = 10; // room creations per IP
// B55 FIX (finding 6): send/read budgets now enforce BOTH a per-IP total
// (the anti-Sybil term — rotating usernames cannot mint fresh buckets) AND a
// per-username share (a fairness sub-bucket so several members behind one
// NAT don't starve each other). Membership mutations (join/leave/create/
// kick/grant) stay purely IP-scoped: budgets that gate Sybil-able actions
// must not be dodgeable by minting names.
export const SEND_LIMIT_PER_WINDOW = 120; // signal/inbox/reaction/invite sends per IP per 5 min
export const SEND_USER_SHARE_PER_WINDOW = 60; // per-username share of the send budget (fairness sub-bucket)
export const SEND_WINDOW_SEC = 5 * 60;
export const JOIN_LIMIT_PER_WINDOW = 120; // joins + leaves per IP per 5 min
export const READ_LIMIT_PER_WINDOW = 1200; // heartbeats + polls + fetches + acks per IP per 5 min (~2/s sustained; normal use ≈0.3/s)
export const READ_USER_SHARE_PER_WINDOW = 600; // per-username share of the read budget (~2/s per user: two same-user tabs + ack overhead)

// Reactions are cosmetic metadata on ciphertext messages: one hash per room.
// Field = `${msgId}|${emoji}|${username}`, value "1" (presence is the datum;
// counts are a single HGETALL away, which is what the client's 2s poll does).
export const REACTION_EMOJI = ["👍", "❤️", "😂", "😮", "😢", "🙏"] as const;
export type ReactionEmoji = (typeof REACTION_EMOJI)[number];
export const MAX_REACTION_MESSAGES = 50; // one fetch never exceeds ~50 messages (mirrors FETCH_BOX_CAP)
export const MAX_REACTION_REACTORS = 20; // per-emoji usernames in a `details` entry (counts carry the true total)
export const MAX_PENDING_INVITES = 50; // per-user pending-invite index cap (GET /invites/mine stays bounded)
export const GROUP_NAME_MAX = 64; // groupName length cap (1-64 at create)
export const GROUP_DESC_MAX = 256; // groupDesc length cap (0-256 at create)

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
  joinedAt: number; // join timestamp; immutable (join order breaks creator-successor ties)
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
const reactionsKey = (code: string) => `room:${code}:reactions`;
const invitesKey = (code: string) => `room:${code}:invites`;
const userInvitesKey = (username: string) => `user:${username}:invites`;
// Seat counter for maxMembers enforcement (finding 8): a plain int kept in
// lockstep with the members hash, claimed via atomic INCR so concurrent
// joiners can never oversubscribe a capped room. Released on leave/kick/
// sweep-prune and dropped with the room (see below).
const seatsKey = (code: string) => `room:${code}:seats`;

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
  const m = parseStored<{ pubkey?: unknown; beat?: unknown; peerId?: unknown; addrs?: unknown; role?: unknown; joinedAt?: unknown }>(raw);
  if (!m) return null;
    if (typeof m.pubkey !== "string") return null;
    const beat = typeof m.beat === "number" ? m.beat : 0;
    const info: MemberInfo = {
      username,
      pubkey: m.pubkey,
      beat,
      online: Date.now() - beat <= PRESENCE_TIMEOUT_MS,
      role: parseRole(m.role),
      joinedAt: typeof m.joinedAt === "number" ? m.joinedAt : beat, // legacy pre-joinedAt members: stored beat is the only proxy
    };
    if (typeof m.peerId === "string") info.peerId = m.peerId;
    if (Array.isArray(m.addrs)) info.addrs = m.addrs.filter((a): a is string => typeof a === "string");
    return info;
}

// Refresh the sliding TTL safety net on room + roster + epoch + bans +
// reactions + invites (one round trip). Skipping epoch/bans/invites here
// would let them decay under an active room: the generation would rewind
// and kicked users could return; pending invites would silently die.
async function touchRoom(code: string): Promise<void> {
  await redis.pipeline([
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: epochKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: bannedKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: reactionsKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: invitesKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: seatsKey(code), args: [ROOM_TTL_SEC] },
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
  groupName: string | null; // group display name (1-64 at create; null = plain room)
  groupDesc: string | null; // group description (0-256 at create; null = none)
  maxMembers: number | null; // hard member cap (int >= 2; null = unlimited)
  parentCode: string | null; // main room this group branches from (optional)
}

// Stored room metadata lives in ONE field ("meta") of the room hash as a
// JSON doc. parseRoomMeta decodes it from a raw HGETALL, tolerating backend
// differences via parseStored (real Upstash auto-parses JSON-ish strings
// into objects; MockRedis returns the stored string verbatim).
interface StoredRoomMeta {
  passwordHash?: unknown;
  createdAt?: unknown;
  creator?: unknown;
  groupName?: unknown;
  groupDesc?: unknown;
  maxMembers?: unknown;
  parentCode?: unknown;
}

function parseRoomMeta(meta: Record<string, string> | null): RoomMeta | null {
  if (!meta) return null;
  // BUGFIX x2: (1) room metadata lives under the single "meta" field, not
  // top-level; (2) real Upstash auto-parses the JSON string into an object,
  // so a typeof-string gate kills it. parseStored tolerates both shapes.
  const parsed = parseStored<StoredRoomMeta>((meta as Record<string, unknown>).meta);
  if (!parsed) return null;
  return {
    passwordHash: typeof parsed.passwordHash === "string" ? parsed.passwordHash : null,
    createdAt: typeof parsed.createdAt === "string" ? parsed.createdAt : "",
    creator: typeof parsed.creator === "string" ? parsed.creator : null,
    groupName: typeof parsed.groupName === "string" ? parsed.groupName : null,
    groupDesc: typeof parsed.groupDesc === "string" ? parsed.groupDesc : null,
    maxMembers: typeof parsed.maxMembers === "number" && Number.isInteger(parsed.maxMembers) && parsed.maxMembers >= 2
      ? parsed.maxMembers
      : null,
    parentCode: typeof parsed.parentCode === "string" ? parsed.parentCode : null,
  };
}

export async function getRoomMeta(code: string): Promise<RoomMeta | null> {
  return parseRoomMeta(await hgetall(roomKey(code)));
}

export async function checkCreateLimit(ipHash: string): Promise<void> {
  const key = `rate:create:${ipHash}`;
  const hits = await redis.incr(key);
  if (hits === 1) await redis.expire(key, 3600);
  if (hits > CREATE_LIMIT_PER_HOUR) {
    throw new RoomError(429, "Too many rooms created. Try again later.");
  }
}

// B55 FIX (finding 6): checkSendLimit enforces TWO buckets — a per-IP total
// (the anti-Sybil term: fresh usernames cannot dodge it) and a per-username
// share (fairness among members behind one NAT). Usernames are validated
// upstream (assertUsernameHeader) before they ever key a bucket, and the IP
// component comes from clientIpHash (TRUST_PROXY-aware — see finding 10).
export async function checkSendLimit(kind: "sig" | "inbox" | "reactions" | "invites", ipHash: string, username?: string): Promise<void> {
  if (username) {
    const userKey = `rate:${kind}:user:${ipHash}:${username}`;
    const userHits = await redis.incr(userKey);
    if (userHits === 1) await redis.expire(userKey, SEND_WINDOW_SEC);
    if (userHits > SEND_USER_SHARE_PER_WINDOW) {
      throw new RoomError(429, "Too many requests. Slow down and try again.");
    }
  }
  const key = `rate:${kind}:ip:${ipHash}`;
  const hits = await redis.incr(key);
  if (hits === 1) await redis.expire(key, SEND_WINDOW_SEC);
  if (hits > SEND_LIMIT_PER_WINDOW) {
    throw new RoomError(429, "Too many requests. Slow down and try again.");
  }
}

// checkReadLimit budgets the high-frequency read path (heartbeats, signal
// drains, inbox fetches/acks) with the same two-bucket structure as
// checkSendLimit: per-IP total + per-username fairness share.
export async function checkReadLimit(ipHash: string, username?: string): Promise<void> {
  if (username) {
    const userKey = `rate:read:user:${ipHash}:${username}`;
    const userHits = await redis.incr(userKey);
    if (userHits === 1) await redis.expire(userKey, SEND_WINDOW_SEC);
    if (userHits > READ_USER_SHARE_PER_WINDOW) {
      throw new RoomError(429, "Too many requests. Slow down and try again.");
    }
  }
  const key = `rate:read:ip:${ipHash}`;
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

// trustProxyHeader decides whether x-forwarded-for is a trusted source for
// the client's real IP (finding 10). Default: trust the platform only —
// Vercel's proxy overwrites XFF, so the header is platform-controlled there.
// TRUST_PROXY overrides the default: "true"/"1" trusts a self-hosted reverse
// proxy (nginx/Caddy); any other explicit value (e.g. "false") never trusts
// the header.
export function trustProxyHeader(): boolean {
  const mode = process.env.TRUST_PROXY;
  if (mode !== undefined && mode !== "") {
    return mode === "true" || mode === "1";
  }
  return process.env.VERCEL === "1";
}

// clientIpHash derives the budget key's IP component (finding 10): when a
// trusted proxy is in front, the leftmost x-forwarded-for entry is the
// client's real IP ("client, proxy1, proxy2"); otherwise XFF is
// attacker-controlled — spoofable per request, which would mint unlimited
// fresh budget buckets — so it is ignored entirely and every client shares
// ONE bucket ("unknown"). Budgets stay functional (anti-abuse) at the cost
// of per-IP granularity; operators behind their own proxy must set
// TRUST_PROXY=true so their (trusted) proxy's XFF is honored again.
export function clientIpHash(req: Request): string {
  if (trustProxyHeader()) {
    const raw = req.headers.get("x-forwarded-for") || "127.0.0.1";
    const ip = raw.split(",")[0].trim() || "127.0.0.1";
    return anonymizeIp(ip);
  }
  return anonymizeIp("unknown");
}

// Group-room extensions accepted at create. All optional; maxMembers null
// = unlimited. parentCode links a group to its main room (format-checked
// only — the parent need not exist for the group to function).
export interface CreateRoomOptions {
  groupName?: string;
  groupDesc?: string;
  maxMembers?: number | null;
  parentCode?: string | null;
}

export async function createRoom(
  username: string,
  pubkey: string,
  passwordHash: string | null,
  opts: CreateRoomOptions = {}
): Promise<{ sessionId: string }> {
  assertUsername(username);
  assertPubkey(pubkey);
  // Group meta validation: lengths are enforced here (not in the route) so
  // every caller — HTTP and tests alike — meets the same contract.
  if (opts.groupName !== undefined) {
    if (typeof opts.groupName !== "string" || opts.groupName.length < 1 || opts.groupName.length > GROUP_NAME_MAX) {
      throw new RoomError(400, `groupName must be a string of 1-${GROUP_NAME_MAX} characters`);
    }
  }
  if (opts.groupDesc !== undefined) {
    if (typeof opts.groupDesc !== "string" || opts.groupDesc.length > GROUP_DESC_MAX) {
      throw new RoomError(400, `groupDesc must be a string of at most ${GROUP_DESC_MAX} characters`);
    }
  }
  if (opts.maxMembers !== undefined && opts.maxMembers !== null) {
    if (typeof opts.maxMembers !== "number" || !Number.isInteger(opts.maxMembers) || opts.maxMembers < 2) {
      throw new RoomError(400, "maxMembers must be an integer of at least 2, or null for unlimited");
    }
  }
  if (opts.parentCode !== undefined && opts.parentCode !== null) {
    assertRoomCode(opts.parentCode);
  }
  const now = Date.now();
  const metaDoc: Record<string, unknown> = {
    passwordHash,
    createdAt: new Date().toISOString(),
    creator: username,
  };
  if (opts.groupName !== undefined) metaDoc.groupName = opts.groupName;
  if (opts.groupDesc !== undefined) metaDoc.groupDesc = opts.groupDesc;
  if (opts.maxMembers !== undefined && opts.maxMembers !== null) metaDoc.maxMembers = opts.maxMembers;
  if (opts.parentCode !== undefined && opts.parentCode !== null) metaDoc.parentCode = opts.parentCode;

  for (let attempt = 0; attempt < 10; attempt++) {
    const code = generateRoomCode();
    const created = await redis.hsetnx(roomKey(code), "meta", JSON.stringify(metaDoc));
    if (created === 0) continue; // collision — regenerate
    // Claim the creator and arm TTLs in one round trip. The hsetnx result
    // tells us if our own username somehow raced us (rollback + retry).
    // The creator owns the room: member role for moderation rank.
    const [claimed] = (await redis.pipeline([
      { cmd: "hsetnx", key: membersKey(code), args: [username, JSON.stringify({ pubkey, beat: now, role: "creator", joinedAt: now })] },
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
    // Finding 8: capped rooms start with the creator's seat claimed (the
    // creator is member #1) — the seats counter is minted here so the first
    // joiner's claim lands at 2, never 1, and maxMembers stays exact.
    if (claimed === 1 && opts.maxMembers !== undefined && opts.maxMembers !== null) {
      await redis.set(seatsKey(code), 1);
      await redis.expire(seatsKey(code), ROOM_TTL_SEC);
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

  // Room meta + ban state + member count in one round trip: the meta doubles
  // as the existence proof (no separate roomExists round trip). The member
  // count only seeds the maxMembers seat counter for legacy rooms that
  // predate it (finding 8) — the counter itself is authoritative for the
  // gate.
  const [metaRaw, banned, memberCount] = (await redis.pipeline([
    { cmd: "hgetall", key: roomKey(code), args: [] },
    { cmd: "hget", key: bannedKey(code), args: [username] },
    { cmd: "hlen", key: membersKey(code), args: [] },
  ])) as [Record<string, string> | null, string | null, number];
  if (isEmptyRecord(metaRaw)) {
    throw new RoomError(404, "Session not found");
  }
  // Kicked users stay out while the room lives.
  if (banned) {
    throw new RoomError(403, "You were kicked from this session");
  }
  const meta = parseRoomMeta(metaRaw);
  // Max-capacity gate (finding 8): the seat is claimed with an atomic Redis
  // INCR so concurrent joiners can never both pass the gate — the old
  // hlen-then-hsetnx was a check-then-add TOCTOU that oversubscribed capped
  // rooms on real Redis (MockRedis pipelines run sequentially in-process, so
  // tests saw an atomicity the real backend does not guarantee). Over the
  // cap → refund the claim and reject with the exact 403 message. Seats are
  // released on every departure path (leaveRoom/kickMember/sweepRooms) and
  // dropped with the room; a claim whose join crashes mid-flight self-heals
  // via sweep (the member never heartbeats, gets pruned, seat released).
  if (meta?.maxMembers != null) {
    const seats = await redis.incr(seatsKey(code));
    let total = seats;
    if (seats === 1) {
      // Fresh counter: fold in members who joined before the seats counter
      // existed (legacy rooms — their seats were never counted) and arm the
      // sliding TTL backstop.
      if (memberCount > 0) {
        await redis.incrBy(seatsKey(code), memberCount);
        total = 1 + memberCount;
      }
      await redis.expire(seatsKey(code), ROOM_TTL_SEC);
    }
    if (total > meta.maxMembers) {
      // Refund the claim — and only the claim. The legacy seed (if any)
      // represents REAL pre-existing members and stays on the counter.
      await redis.decr(seatsKey(code));
      throw new RoomError(403, "Maximum allowance is reached");
    }
  }
  const now = Date.now();
  // Claim + TTLs + fresh roster in one round trip. Joiners start as
  // members; roles are granted explicitly (existing roles survive rejoins
  // via the 409 path below — re-claiming never demotes).
  const [claimed, , , rosterRaw] = (await redis.pipeline([
    { cmd: "hsetnx", key: membersKey(code), args: [username, JSON.stringify({ pubkey, beat: now, role: "member", joinedAt: now })] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "hgetall", key: membersKey(code), args: [] },
  ])) as [number, unknown, unknown, Record<string, string> | null];
  if (claimed === 0) {
    // This username is already present: the seat claim above must be
    // refunded (the members hash did not grow). Both sub-paths keep the
    // roster size — and therefore the seat count — unchanged.
    if (meta?.maxMembers != null) await redis.decr(seatsKey(code));
    const existing = await redis.hget(membersKey(code), username);
    if (existing && !parseMember(username, existing)) {
      await redis.hset(membersKey(code), username, JSON.stringify({ pubkey, beat: now, role: "member", joinedAt: now }));
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
    joinedAt: current?.joinedAt ?? Date.now(), // join order is immutable: heartbeats never rewrite it
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
// live until the last user leaves, never by timeout. When the CREATOR
// leaves a non-empty room, the crown passes to the oldest surviving admin
// (by join time); with no admins, to the oldest surviving member (see
// transferCreator).
export async function leaveRoom(code: string, username: string): Promise<{ remaining: number; ended: boolean }> {
  assertRoomCode(code);
  assertUsername(username);
  const all = await hgetall(membersKey(code));
  if (!all || !all[username]) {
    // Idempotent leave: already gone still reports the true count.
    const count = all ? Object.keys(all).length : 0;
    return { remaining: count, ended: count === 0 };
  }
  const leaverIsCreator = parseMember(username, all[username])?.role === "creator";
  // Best-effort cleanup of this member's transient queues (their failure
  // must never block the leave itself).
  await redis.pipeline([
    { cmd: "del", key: sigKey(code, username), args: [] },
    { cmd: "del", key: inboxKey(code, username), args: [] },
  ]).catch(() => [] as unknown[]);
  await clearUserReactions(code, username).catch(() => 0);
  await redis.hdel(membersKey(code), username);
  const remaining = await redis.hlen(membersKey(code));
  let ended = false;
  if (remaining === 0) {
    await destroyRoom(code);
    await unindexRoom(code);
    ended = true;
  } else {
    // Finding 8: release the departed member's seat (the room survives; a
    // destroyed room drops the whole counter in destroyRoom).
    await redis.decr(seatsKey(code)).catch(() => 0);
    // The creator leaving transfers the crown (best-effort: a failed
    // transfer degrades to "no creator" — moderation gated off — and must
    // never block the leave itself).
    if (leaverIsCreator) {
      await transferCreator(code, all, username).catch(() => {});
    }
    await touchRoom(code);
    await bumpEpochBestEffort(code); // survivors must learn the departure ASAP
  }
  return { remaining, ended };
}

// Crown transfer: the oldest surviving admin (by joinedAt — the immutable
// join timestamp stamped at create/join and preserved through heartbeats
// and role changes) inherits the room; if no admin survives, the oldest
// surviving member does. Both the meta `creator` field and the successor's
// member role move, so moderation powers (kick/grant) follow the crown —
// a successor with a stale "admin"/"member" role would hold the title
// without the privileges.
async function transferCreator(
  code: string,
  all: Record<string, string>,
  leaver: string
): Promise<void> {
  const survivors: MemberInfo[] = [];
  for (const [name, raw] of Object.entries(all)) {
    if (name === leaver) continue;
    const m = parseMember(name, raw);
    if (m) survivors.push(m);
  }
  if (survivors.length === 0) return;
  const admins = survivors.filter((m) => m.role === "admin");
  const pool = admins.length > 0 ? admins : survivors;
  const successor = pool.reduce((a, b) => (b.joinedAt < a.joinedAt ? b : a));

  const entry: Record<string, unknown> = {
    pubkey: successor.pubkey,
    beat: Date.now(), // fresh beat: the new creator is present by definition
    role: "creator",
    joinedAt: successor.joinedAt,
  };
  if (successor.peerId !== undefined) entry.peerId = successor.peerId;
  if (successor.addrs !== undefined) entry.addrs = successor.addrs;
  const meta = await getRoomMeta(code);
  if (!meta) return; // meta vanished mid-flight: nothing to crown
  await redis.pipeline([
    { cmd: "hset", key: membersKey(code), args: [successor.username, JSON.stringify(entry)] },
    {
      cmd: "hset",
      key: roomKey(code),
      args: [
        "meta",
        JSON.stringify({
          passwordHash: meta.passwordHash,
          createdAt: meta.createdAt,
          creator: successor.username,
          groupName: meta.groupName,
          groupDesc: meta.groupDesc,
          maxMembers: meta.maxMembers,
          parentCode: meta.parentCode,
        }),
      ],
    },
  ]);
}

// ─── Moderation (creator/admin privileges) ─────────────────────────────────
//
// The room creator (main admin) holds every privilege: kick anyone except
// themselves, grant/revoke admin. Granted admins may kick regular members
// only — never another admin, never the creator. Members moderate nobody.
// The creator is immune and immutable. Kicks ban the username while the
// room lives (rejoin returns 403) and wipe the target's transient queues.

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
  const { member: actorMember } = requireRole(all, actor);
  const { member: targetMember } = requireRole(all, target);
  if (actor === target) throw new RoomError(403, "You cannot kick yourself");
  if (targetMember.role === "creator") throw new RoomError(403, "Nobody can kick the room creator");
  if (actorMember.role === "creator") {
    // Main admin: full privilege, anyone but themselves (checked above).
  } else if (actorMember.role === "admin" && targetMember.role === "member") {
    // Granted admins may kick regular members only.
  } else if (actorMember.role === "admin") {
    throw new RoomError(403, "Only the room creator can kick an admin");
  } else {
    throw new RoomError(403, "Only the room creator or an admin can kick users");
  }
  // Best-effort transient cleanup (mirrors leaveRoom): failures must never
  // block the kick itself.
  await redis.pipeline([
    { cmd: "del", key: sigKey(code, target), args: [] },
    { cmd: "del", key: inboxKey(code, target), args: [] },
  ]).catch(() => [] as unknown[]);
  await clearUserReactions(code, target).catch(() => 0);
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
  // Finding 8: release the kicked member's seat (the room survives).
  await redis.decr(seatsKey(code)).catch(() => 0);
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
    joinedAt: targetMember.joinedAt, // role changes never reorder join time
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
  const invites = (await hgetall(invitesKey(code))) || {};
  const ops: PipeOp[] = [
    { cmd: "del", key: roomKey(code), args: [] },
    { cmd: "del", key: membersKey(code), args: [] },
    { cmd: "del", key: epochKey(code), args: [] },
    { cmd: "del", key: bannedKey(code), args: [] },
    { cmd: "del", key: reactionsKey(code), args: [] },
    { cmd: "del", key: invitesKey(code), args: [] },
    { cmd: "del", key: seatsKey(code), args: [] }, // finding 8: seats die with the room
  ];
  for (const username of Object.keys(members)) {
    ops.push(
      { cmd: "del", key: sigKey(code, username), args: [] },
      { cmd: "del", key: inboxKey(code, username), args: [] }
    );
  }
  // Reap this room's fields from each pending invitee's per-user index
  // (best-effort: a vanished index key costs nothing; other rooms' invites
  // on the same index survive).
  for (const invitee of Object.keys(invites)) {
    ops.push({ cmd: "hdel", key: userInvitesKey(invitee), args: [code] });
  }
  if (ops.length === 0) return;
  await redis.pipeline(ops).catch(() => [] as unknown[]);
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

// ─── Message reactions (cosmetic, room-scoped) ──────────────────────────────
//
// Reactions are the one piece of chat state the server aggregates: they ride
// on ciphertext message IDs, contain no user content, and live in ONE hash
// per room (`{msgId}|{emoji}|{username}` -> "1"), so a client's 2s poll reads
// every count in one HGETALL. Message IDs are sender-assigned and shared
// across broadcast and DM spaces (DM privacy is payload-level E2E), so counts
// are keyed by msgId alone. One reaction per user per message: reacting again
// with a different emoji replaces the prior one; reacting with the same emoji
// toggles it off. Reactions die with the room (24h sliding TTL, refreshed by
// every room touch), and departures clean up after themselves.
//
// `details` backs the per-message detail bar (emoji -> who reacted). It is
// room-scoped exactly like counts: the server never sees message bodies, so
// it cannot tell a broadcast msgId from a DM one — the only server-side
// msgId mapping is the short-lived inbox hash for undelivered boxes, which
// does not cover P2P-delivered messages. Every member who can see a msgId's
// counts can therefore also see its reactors; DM msgIds are already exposed
// room-wide by counts. Closing this gap needs a client-supplied conversation
// tag on the reaction itself (e.g. a `to` field), which the protocol does not
// carry today — documented limitation, revisited when reactions gain scope.

export interface ReactionDetail {
  emoji: string; // allowlisted emoji (allowlist order)
  usernames: string[]; // reactors, ascending, capped at MAX_REACTION_REACTORS
}

export interface ReactionSummary {
  msgId: string;
  counts: Record<string, number>; // emoji -> count (allowlist order)
  mine: string[]; // emojis the requesting user reacted with
  details: ReactionDetail[]; // who reacted with what (allowlist order)
}

export interface ToggleReactionResult {
  msgId: string;
  emoji: string;
  reacted: boolean; // false = the toggle removed this emoji
}

function isAllowedReactionEmoji(emoji: unknown): emoji is ReactionEmoji {
  return typeof emoji === "string" && (REACTION_EMOJI as readonly string[]).includes(emoji);
}

export function assertReactionEmoji(emoji: unknown): asserts emoji is ReactionEmoji {
  if (!isAllowedReactionEmoji(emoji)) {
    throw new RoomError(400, `emoji must be one of ${REACTION_EMOJI.join(" ")}`);
  }
}

function assertReactionMsgId(msgId: unknown): asserts msgId is string {
  if (typeof msgId !== "string" || msgId.length === 0 || msgId.length > 128) {
    throw new RoomError(400, "msgId is required (max 128 chars)");
  }
}

// parseReactionField splits a stored field right-to-left: usernames and
// allowlisted emojis cannot contain "|", so the last two separators are
// unambiguous even when a sender-assigned msgId itself contains pipes.
function parseReactionField(field: string): { msgId: string; emoji: ReactionEmoji; username: string } | null {
  const i = field.lastIndexOf("|");
  if (i <= 0 || i === field.length - 1) return null;
  const username = field.slice(i + 1);
  const j = field.lastIndexOf("|", i - 1);
  if (j <= 0) return null;
  const emoji = field.slice(j + 1, i);
  const msgId = field.slice(0, j);
  if (!isAllowedReactionEmoji(emoji)) return null;
  if (!USERNAME_RE.test(username)) return null;
  if (msgId.length === 0 || msgId.length > 128) return null;
  return { msgId, emoji, username };
}

// Remove every reaction of a departing member (leave/kick). Rooms are small
// and the hash is one HGETALL; best-effort because a cleanup failure must
// never block the departure itself.
async function clearUserReactions(code: string, username: string): Promise<void> {
  const all = await hgetall(reactionsKey(code));
  if (!all) return;
  const suffix = `|${username}`;
  const theirs = Object.keys(all).filter((field) => field.endsWith(suffix));
  if (theirs.length === 0) return;
  await redis.hdel(reactionsKey(code), ...theirs);
}

// Toggle one reaction. Reads membership + the whole reactions hash in one
// round trip, then writes the delta + TTL refreshes in another. Membership is
// re-checked on every call because reactions are authenticated state, not
// fire-and-forget signaling.
export async function toggleReaction(
  code: string,
  username: string,
  msgId: string,
  emoji: string
): Promise<ToggleReactionResult> {
  assertRoomCode(code);
  assertUsername(username);
  assertReactionMsgId(msgId);
  assertReactionEmoji(emoji);

  const [membersRaw, reactionsRaw] = (await redis.pipeline([
    { cmd: "hgetall", key: membersKey(code), args: [] },
    { cmd: "hgetall", key: reactionsKey(code), args: [] },
  ])) as [Record<string, string> | null, Record<string, string> | null];
  if (isEmptyRecord(membersRaw)) throw new RoomError(404, "Session not found");
  if (!membersRaw[username]) throw new RoomError(403, "Not in this session");

  const prefix = `${msgId}|`;
  const suffix = `|${username}`;
  const own: string[] = [];
  for (const field of Object.keys(reactionsRaw || {})) {
    if (!field.startsWith(prefix) || !field.endsWith(suffix)) continue;
    // Guard against a piped msgId whose tail happens to look like a suffix:
    // only exact `{msgId}|{emoji}|{username}` fields are ours.
    const mid = field.slice(prefix.length, field.length - suffix.length);
    if (isAllowedReactionEmoji(mid)) own.push(field);
  }
  // Same emoji twice = toggle off; any other prior emoji = replace it
  // (single reaction per user per message, like most chat clients).
  const field = `${msgId}|${emoji}|${username}`;
  const same = own.length === 1 && own[0] === field;
  const ops: PipeOp[] = [];
  if (own.length > 0) ops.push({ cmd: "hdel", key: reactionsKey(code), args: own });
  if (!same) ops.push({ cmd: "hset", key: reactionsKey(code), args: [field, "1"] });
  ops.push(
    { cmd: "expire", key: reactionsKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] }
  );
  await redis.pipeline(ops);
  return { msgId, emoji, reacted: !same };
}

// Aggregate the room's reaction hash for the requesting member. Optional
// `msgIds` narrows the hash scan to messages an on-screen UI actually shows;
// without it the response is capped at MAX_REACTION_MESSAGES (msgId order).
// Each summary carries `details` (emoji -> reactor usernames) for the detail
// view: allowlist order, usernames ascending, capped at
// MAX_REACTION_REACTORS per emoji (counts keep the true total). Unparseable
// fields are reaped like fetchBoxes: they would otherwise shadow cap slots
// forever.
export async function fetchReactions(
  code: string,
  username: string,
  msgIds?: string[]
): Promise<{ reactions: ReactionSummary[] }> {
  assertRoomCode(code);
  assertUsername(username);
  await requireMember(code, username);

  let filter: Set<string> | null = null;
  if (msgIds !== undefined) {
    if (!Array.isArray(msgIds) || msgIds.length === 0 || msgIds.length > MAX_REACTION_MESSAGES) {
      throw new RoomError(400, `msgIds must be an array of 1-${MAX_REACTION_MESSAGES} message IDs`);
    }
    const clean = new Set(msgIds.filter((id): id is string => typeof id === "string" && id.length > 0 && id.length <= 128));
    if (clean.size === 0) throw new RoomError(400, "No valid message IDs");
    filter = clean;
  }

  const all = await hgetall(reactionsKey(code));
  if (!all) return { reactions: [] };
  const byMsg = new Map<
    string,
    { counts: Map<string, number>; mine: Set<string>; reactors: Map<string, string[]> }
  >();
  const corrupt: string[] = [];
  for (const field of Object.keys(all)) {
    const parsed = parseReactionField(field);
    if (!parsed) {
      corrupt.push(field); // reap: unparseable fields would squat cap slots forever
      continue;
    }
    if (filter && !filter.has(parsed.msgId)) continue;
    let entry = byMsg.get(parsed.msgId);
    if (!entry) {
      entry = { counts: new Map(), mine: new Set(), reactors: new Map() };
      byMsg.set(parsed.msgId, entry);
    }
    entry.counts.set(parsed.emoji, (entry.counts.get(parsed.emoji) || 0) + 1);
    if (parsed.username === username) entry.mine.add(parsed.emoji);
    const reactors = entry.reactors.get(parsed.emoji);
    if (reactors) reactors.push(parsed.username);
    else entry.reactors.set(parsed.emoji, [parsed.username]);
  }
  if (corrupt.length > 0) await redis.hdel(reactionsKey(code), ...corrupt).catch(() => 0);

  const reactions: ReactionSummary[] = [];
  for (const [msgId, entry] of byMsg) {
    const counts: Record<string, number> = {};
    const mine: string[] = [];
    const details: ReactionDetail[] = [];
    for (const emoji of REACTION_EMOJI) {
      const n = entry.counts.get(emoji);
      if (n) counts[emoji] = n;
      if (entry.mine.has(emoji)) mine.push(emoji);
      const reactors = entry.reactors.get(emoji);
      if (reactors && reactors.length > 0) {
        // One field per (msgId, emoji, username), so the list is already
        // unique; sort for a stable UI order before capping.
        reactors.sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
        details.push({ emoji, usernames: reactors.slice(0, MAX_REACTION_REACTORS) });
      }
    }
    reactions.push({ msgId, counts, mine, details });
  }
  reactions.sort((a, b) => (a.msgId < b.msgId ? -1 : a.msgId > b.msgId ? 1 : 0));
  return { reactions: reactions.slice(0, MAX_REACTION_MESSAGES) };
}

// ─── Group invites (any member may invite; per-room hash) ───────────────────
//
// Invites are room-scoped like reactions: ONE hash per room
// (`room:{code}:invites`, field = invitee username -> JSON {by, at,
// groupName}), so a room's pending invites read in one HGETALL and die with
// the room. groupName is snapshotted into the invite at send time (the
// "what is this room" line clients show); groupDesc is read from live room
// meta at listing time (too long to snapshot).
//
// A bounded per-user index (`user:{username}:invites`, field = room code ->
// "1") backs GET /invites/mine: without it, listing pending invites would
// require scanning the global room index. The index is capped at
// MAX_PENDING_INVITES per user (new invites past the cap are 429; re-invites
// of an existing field refresh without growing), refreshed on every
// invite/accept/decline, and its fields are reaped when rooms die
// (destroyRoom) and defensively on read when a room has vanished. The index
// rides the same sliding TTL as the room hashes it points at, so a stale
// entry never outlives its room by more than the room's own lifetime.

export interface InviteRecord {
  code: string;
  groupName: string | null; // snapshot at invite time ("" rooms read as null)
  groupDesc: string | null; // live from room meta
  by: string; // inviter username
  at: number; // epoch ms of the (latest) invite
}

const MAX_INVITE_INDEX_SCAN = MAX_PENDING_INVITES; // defense in depth: index is capped at write, re-capped here

// createInvite: any member of the room may invite any non-member, non-banned
// username. Re-inviting an already-pending invitee is idempotent — it
// refreshes {by, at} in place (and never grows the index past its cap).
export async function createInvite(code: string, inviter: string, invitee: string): Promise<void> {
  assertRoomCode(code);
  assertUsername(inviter);
  assertUsername(invitee);
  if (inviter === invitee) throw new RoomError(400, "You cannot invite yourself");

  // Members + meta + ban state + existing invite + index depth in one round
  // trip: the invitee checks are all read-only gates.
  const [membersRaw, metaRaw, banned, existingRaw, indexCount] = (await redis.pipeline([
    { cmd: "hgetall", key: membersKey(code), args: [] },
    { cmd: "hgetall", key: roomKey(code), args: [] },
    { cmd: "hget", key: bannedKey(code), args: [invitee] },
    { cmd: "hget", key: invitesKey(code), args: [invitee] },
    { cmd: "hlen", key: userInvitesKey(invitee), args: [] },
  ])) as [Record<string, string> | null, Record<string, string> | null, string | null, string | null, number];
  if (isEmptyRecord(membersRaw) || isEmptyRecord(metaRaw)) throw new RoomError(404, "Session not found");
  if (!membersRaw![inviter]) throw new RoomError(403, "Not in this session");
  if (membersRaw![invitee]) throw new RoomError(409, "User is already in this session");
  if (banned) throw new RoomError(403, "This user was kicked from this session");
  // New invites past the per-user cap are refused; re-invites of a pending
  // field never grow the index, so they pass even at the cap.
  if (!existingRaw && indexCount >= MAX_PENDING_INVITES) {
    throw new RoomError(429, "Too many pending invites. Accept or decline some before inviting more.");
  }
  const meta = parseRoomMeta(metaRaw);
  const groupName = meta?.groupName ?? "";
  await redis.pipeline([
    { cmd: "hset", key: invitesKey(code), args: [invitee, JSON.stringify({ by: inviter, at: Date.now(), groupName })] },
    { cmd: "expire", key: invitesKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "hset", key: userInvitesKey(invitee), args: [code, "1"] },
    { cmd: "expire", key: userInvitesKey(invitee), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: roomKey(code), args: [ROOM_TTL_SEC] },
    { cmd: "expire", key: membersKey(code), args: [ROOM_TTL_SEC] },
  ]);
}

// getInvitesForUser lists every pending invite for the caller across rooms
// (the per-user index), newest first. Rooms that vanished between index
// write and read are skipped AND pruned from the index — the same
// ROOM_INDEX-style pruning the sweep does for rooms themselves, done
// defensively on read because sweep cannot iterate user indexes.
export async function getInvitesForUser(username: string): Promise<InviteRecord[]> {
  assertUsername(username);
  const index = await hgetall(userInvitesKey(username));
  if (!index) return [];
  const codes = Object.keys(index)
    .filter((c) => ROOM_CODE_RE.test(c))
    .slice(0, MAX_INVITE_INDEX_SCAN);
  if (codes.length === 0) return [];
  // All room metas + invite hashes in one pipelined round trip (2 ops/room).
  const ops: PipeOp[] = [];
  for (const c of codes) {
    ops.push({ cmd: "hgetall", key: roomKey(c), args: [] });
    ops.push({ cmd: "hgetall", key: invitesKey(c), args: [] });
  }
  const results = await redis.pipeline(ops);
  const out: InviteRecord[] = [];
  const vanished: string[] = [];
  for (let i = 0; i < codes.length; i++) {
    const meta = parseRoomMeta(results[i * 2] as Record<string, string> | null);
    if (!meta) {
      vanished.push(codes[i]); // room died without cleanup (TTL) — reap index entry
      continue;
    }
    const raw = (results[i * 2 + 1] as Record<string, string> | null)?.[username];
    const stored = parseStored<{ by?: unknown; at?: unknown; groupName?: unknown }>(raw);
    if (!stored || typeof stored.by !== "string" || typeof stored.at !== "number") {
      vanished.push(codes[i]); // invite consumed/corrupt while the room lives — reap index entry
      continue;
    }
    out.push({
      code: codes[i],
      groupName: typeof stored.groupName === "string" && stored.groupName.length > 0 ? stored.groupName : null,
      groupDesc: meta.groupDesc,
      by: stored.by,
      at: stored.at,
    });
  }
  if (vanished.length > 0) {
    await redis.hdel(userInvitesKey(username), ...vanished).catch(() => 0);
  }
  out.sort((a, b) => b.at - a.at);
  return out;
}

// consumeInvite removes a pending invite from both the room hash and the
// invitee's index. Best-effort on purpose: cleanup failure must never fail
// the accept/decline that triggered it.
async function consumeInvite(code: string, username: string): Promise<void> {
  await redis.pipeline([
    { cmd: "hdel", key: invitesKey(code), args: [username] },
    { cmd: "hdel", key: userInvitesKey(username), args: [code] },
    { cmd: "expire", key: userInvitesKey(username), args: [ROOM_TTL_SEC] }, // keep the safety net on remaining entries
  ]).catch(() => [] as unknown[]);
}

// acceptInvite joins the caller to the room the invite names. The invite
// must exist (404 otherwise). A full room rejects with the exact
// "Maximum allowance is reached" message AND consumes the invite — a
// full room never strands a pending seat. On success the join happens
// first, then the invite is consumed (every path consumes: a stale invite
// is worse than an unnecessary delete, and re-accepting is then a clean
// 404).
export async function acceptInvite(
  code: string,
  username: string,
  pubkey: string
): Promise<{ roster: MemberInfo[]; epoch: number }> {
  assertRoomCode(code);
  assertUsername(username);
  assertPubkey(pubkey);

  const raw = await redis.hget(invitesKey(code), username);
  if (!raw) throw new RoomError(404, "Invite not found");

  // Full-room rejection consumes the invite (mirrors the joinRoom gate:
  // count before insert). The password gate lives in the route, exactly
  // like the plain join path, so the 401 comes first for protected rooms.
  const meta = await getRoomMeta(code);
  if (meta?.maxMembers != null) {
    const count = await redis.hlen(membersKey(code));
    if (count >= meta.maxMembers) {
      await consumeInvite(code, username);
      throw new RoomError(403, "Maximum allowance is reached");
    }
  }
  try {
    return await joinRoom(code, username, pubkey);
  } finally {
    await consumeInvite(code, username);
  }
}

// declineInvite consumes the invite without joining. Requires the invite to
// exist (404 otherwise), same as accept.
export async function declineInvite(code: string, username: string): Promise<void> {
  assertRoomCode(code);
  assertUsername(username);
  const raw = await redis.hget(invitesKey(code), username);
  if (!raw) throw new RoomError(404, "Invite not found");
  await consumeInvite(code, username);
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
        // Finding 8: a pruned member frees their maxMembers seat (this also
        // self-heals seats claimed by joins that crashed mid-flight).
        await redis.decr(seatsKey(code)).catch(() => 0);
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
