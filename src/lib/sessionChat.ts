import type { Db } from "mongodb";

// Conversation model: every transcript line belongs to exactly one
// conversation. "general" is the room-wide channel; 1:1 threads use a
// canonical pair key "a|b" (usernames are [a-zA-Z0-9_]{3,20}, so "|" is an
// unambiguous separator and sorting makes the key order-independent).
export const GENERAL_CONV = "general";

export function conversationKey(a: string, b: string): string {
  return [a, b].sort().join("|");
}

export function isPairConv(conv: unknown): conv is string {
  return typeof conv === "string" && /^[a-zA-Z0-9_]{3,20}\|[a-zA-Z0-9_]{3,20}$/.test(conv);
}

// resolveConvId normalizes any stored doc (including pre-convId legacy rows)
// to its conversation bucket.
export function resolveConvId(d: { username: string; kind: string; to?: string; convId?: string }): string {
  if (d.convId) return d.convId;
  if (d.kind === "system") return GENERAL_CONV;
  return d.to ? conversationKey(d.username, d.to) : GENERAL_CONV;
}

export interface ChatMemberGuard {
  ok: boolean;
  ended?: boolean;
}

export function isSessionAlive(session: { status: string; expiresAt: Date | string } | null): boolean {
  if (!session) return false;
  if (session.status !== "ACTIVE") return false;
  return new Date(session.expiresAt) > new Date();
}

export async function isMember(
  db: Db,
  sessionId: string,
  username: string
): Promise<boolean> {
  const p = await db.collection("session_participants").findOne({
    sessionId,
    username,
    status: "ACTIVE",
  });
  return !!p;
}

// Allocates the next monotonic sequence number for a room via an atomic $inc.
export async function nextSeq(db: Db, sessionId: string): Promise<number> {
  const updated = await db.collection("sessions").findOneAndUpdate(
    { sessionId },
    { $inc: { msgSeq: 1 } },
    { returnDocument: "after", projection: { msgSeq: 1 } }
  );
  return updated?.msgSeq ?? 0;
}

// appendMessage stores one transcript entry in its conversation bucket.
// `to` marks a private 1:1 message (recipient username); omitted/empty means
// the general room. System events always land in "general".
export async function appendMessage(
  db: Db,
  sessionId: string,
  username: string,
  kind: "chat" | "system",
  text: string,
  to?: string
): Promise<number> {
  const seq = await nextSeq(db, sessionId);
  const doc: Record<string, unknown> = {
    sessionId,
    seq,
    username,
    kind,
    text,
    convId: to ? conversationKey(username, to) : GENERAL_CONV,
    createdAt: new Date(),
  };
  if (to) doc.to = to;
  await db.collection("session_messages").insertOne(doc);
  return seq;
}

export function sanitizeChatText(raw: unknown): string | null {
  if (typeof raw !== "string") return null;
  // Strip control characters (except \n and \t) then clamp to 500 chars.
  const cleaned = raw.replace(/[\x00-\x08\x0B-\x1F\x7F]/g, "").trim();
  if (cleaned.length === 0 || cleaned.length > 500) return null;
  return cleaned;
}

export interface ChatMessageDTO {
  seq: number;
  username: string;
  kind: "chat" | "system";
  text: string;
  createdAt: string;
  to?: string;
  convId: string;
}

export interface ChatDoc {
  seq: number;
  username: string;
  kind: string;
  text: string;
  createdAt: Date;
  to?: string;
  convId?: string;
}

export interface SessionAliveDoc {
  status: string;
  expiresAt: Date | string;
}

export function toMessageDTO(m: {
  seq: number;
  username: string;
  kind: string;
  text: string;
  createdAt: Date;
  to?: string;
  convId?: string;
}): ChatMessageDTO {
  // Legacy docs (pre-convId) derive their bucket from `to`.
  const dto: ChatMessageDTO = {
    seq: m.seq,
    username: m.username,
    kind: m.kind === "system" ? "system" : "chat",
    text: m.text,
    createdAt: m.createdAt.toISOString(),
    convId: resolveConvId(m),
  };
  if (m.to) dto.to = m.to;
  return dto;
}
