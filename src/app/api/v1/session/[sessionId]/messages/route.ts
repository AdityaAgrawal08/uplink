import { NextRequest, NextResponse, after } from "next/server";
import { getDb } from "@/lib/mongodb";
import { redis } from "@/lib/redis";
import { performSessionCleanup } from "../../cleanup/route";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import {
  isSessionAlive,
  isMember,
  appendMessage,
  sanitizeChatText,
  toMessageDTO,
  GENERAL_CONV,
  isPairConv,
  type ChatDoc,
  type SessionAliveDoc,
} from "@/lib/sessionChat";

export const dynamic = "force-dynamic";
export const maxDuration = 10;

const USERNAME_RE = /^[a-zA-Z0-9_]{3,20}$/;
const usernameRegex = USERNAME_RE;
const BACKLOG_LIMIT = 50;
const POLL_LIMIT = 200;
const RATE_LIMIT = 20; // messages per window
const RATE_WINDOW_SEC = 10;

// Instance-level debounce so the shared cleanup sweep runs at most once a
// minute no matter how many poll requests arrive.
declare global {
  var __chatCleanupAt: number | undefined;
}

function maybeCleanup() {
  const now = Date.now();
  const last = globalThis.__chatCleanupAt ?? 0;
  if (now - last < 60_000) return;
  globalThis.__chatCleanupAt = now;
  after(async () => {
    await performSessionCleanup().catch(() => {});
  });
}

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const text = sanitizeChatText(parsed.body.text);
    if (!text) return apiError("text must be 1-500 printable characters", 400);

    // Optional 1:1 recipient. Absent/empty => broadcast to the whole room.
    let to = "";
    if (parsed.body.to !== undefined && parsed.body.to !== null && parsed.body.to !== "") {
      if (typeof parsed.body.to !== "string" || !usernameRegex.test(parsed.body.to)) {
        return apiError("to must be a valid username", 400);
      }
      if (parsed.body.to === username) {
        return apiError("cannot send a private message to yourself", 400);
      }
      to = parsed.body.to;
    }

    const db = await getDb();

    // Per-user flood guard — non-blocking with 80ms timeout, fail-open
    let hits = 1;
    try {
      const rateKey = `chat:${sessionId}:${username}`;
      const timeout = new Promise<number>((_, rej) => setTimeout(() => rej(new Error("redis timeout")), 80));
      hits = await Promise.race([redis.incr(rateKey), timeout]);
      if (hits === 1) {
        // expire fire-and-forget, don't block
        redis.expire(rateKey, RATE_WINDOW_SEC).catch(() => {});
      }
    } catch {
      hits = 1;
    }
    if (hits > RATE_LIMIT) {
      return apiError("You are sending messages too quickly", 429);
    }

    const session = (await db.collection("sessions").findOne({ sessionId })) as unknown as SessionAliveDoc | null;
    if (!isSessionAlive(session)) {
      return apiError("Session has ended", 410);
    }
    if (!(await isMember(db, sessionId, username))) {
      return apiError("Not in this session", 403);
    }

    maybeCleanup();

    if (to) {
      const recipient = await db.collection("session_participants").findOne({
        sessionId,
        username: to,
        status: "ACTIVE",
      });
      if (!recipient) return apiError("recipient is not in this session", 404);
    }

    const seq = await appendMessage(db, sessionId, username, "chat", text, to || undefined);
    return NextResponse.json({ seq }, { status: 201 });
  } catch (error) {
    console.error("Error in POST /api/v1/session/[sessionId]/messages:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    const db = await getDb();

    const session = (await db.collection("sessions").findOne({ sessionId })) as unknown as SessionAliveDoc | null;
    const alive = isSessionAlive(session);

    if (alive && !(await isMember(db, sessionId, username))) {
      return apiError("Not in this session", 403);
    }

    maybeCleanup();

    const afterParam = req.nextUrl.searchParams.get("after");
    const afterSeq = afterParam === null ? null : Number(afterParam);
    if (afterParam !== null && !Number.isSafeInteger(afterSeq)) {
      return apiError("after must be an integer sequence number", 400);
    }

    // Optional conversation scope. Absent => everything visible to me
    // (general + every 1:1 thread I belong to). "general" => room channel
    // only. "a|b" => that exact thread, and I MUST be one of the two.
    const convParam = req.nextUrl.searchParams.get("conv");
    let convScope: string | null = null;
    if (convParam !== null && convParam !== "") {
      if (convParam === GENERAL_CONV) {
        convScope = GENERAL_CONV;
      } else if (isPairConv(convParam)) {
        const [u1, u2] = convParam.split("|");
        if (username !== u1 && username !== u2) {
          return apiError("not a participant of this conversation", 403);
        }
        convScope = convParam;
      } else {
        return apiError('conv must be "general" or a "userA|userB" pair', 400);
      }
    }

    // Long-poll: hold the request open (≤2.5 s) until data shows up, then
    // return immediately. Cuts perceived delivery to <~300 ms while lowering
    // total request rate versus a fixed-interval client tick.
    const waitMs = Math.min(Math.max(Number(req.nextUrl.searchParams.get("wait") ?? 0) || 0, 0), 2500);
    const deadline = Date.now() + waitMs;

    // Delivery visibility: system lines and public broadcasts reach everyone;
    // a message with `to` reaches ONLY sender and recipient.
    const visibleFilter = () => ({
      sessionId,
      $or: [
        { kind: "system" },
        { to: { $in: [null, ""] } },
        { to: username },
        { username },
      ],
    });

    const baseFilter = () =>
      convScope === null ? visibleFilter() : { sessionId, convId: convScope };

    const queryNew = () =>
      db
        .collection("session_messages")
        .find({ ...baseFilter(), seq: { $gt: afterSeq as number } })
        .sort({ seq: 1 })
        .limit(POLL_LIMIT)
        .toArray() as unknown as Promise<ChatDoc[]>;

    if (afterSeq !== null) {
      // cleanup after response, not before
      after(() => maybeCleanup());
      let docs = await queryNew();
      for (;;) {
        if (docs.length > 0 || Date.now() >= deadline) break;
        await new Promise((r) => setTimeout(r, 50));
        const fresh = (await db
          .collection("sessions")
          .findOne({ sessionId }, { projection: { status: 1, expiresAt: 1 } })) as unknown as SessionAliveDoc | null;
        if (!isSessionAlive(fresh)) break;
        docs = await queryNew();
      }
      const roster = await db
        .collection("session_participants")
        .find({ sessionId, status: "ACTIVE" })
        .project({ username: 1, _id: 0 })
        .toArray();
      return NextResponse.json({
        messages: docs.map(toMessageDTO),
        activeUsers: roster.map((r) => r.username).sort(),
        ended: !alive,
      });
    }

    const [docs, roster] = await Promise.all([
      db.collection("session_messages").find(baseFilter()).sort({ seq: -1 }).limit(BACKLOG_LIMIT).toArray() as unknown as Promise<ChatDoc[]>,
      db.collection("session_participants").find({ sessionId, status: "ACTIVE" }).project({ username: 1, _id: 0 }).toArray(),
    ]);

    return NextResponse.json({
      messages: docs.reverse().map(toMessageDTO),
      activeUsers: roster.map((r) => r.username).sort(),
      ended: !alive,
    });
  } catch (error) {
    console.error("Error in GET /api/v1/session/[sessionId]/messages:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
