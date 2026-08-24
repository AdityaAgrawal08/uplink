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
  type ChatDoc,
  type SessionAliveDoc,
} from "@/lib/sessionChat";

const BACKLOG_LIMIT = 50;
const POLL_LIMIT = 200;
const RATE_LIMIT = 20; // messages per window
const RATE_WINDOW_SEC = 10;

// Instance-level debounce so the shared cleanup sweep runs at most once a
// minute no matter how many poll requests arrive.
declare global {
  // eslint-disable-next-line no-var
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

    const db = await getDb();

    // Per-user flood guard (Redis-backed; degrades to per-instance in mock
    // mode). The limiter is abuse DEFENSE, never correctness: if the counter
    // itself is unreachable we fail OPEN so chat delivery cannot 500.
    let hits = 1;
    try {
      const rateKey = `chat:${sessionId}:${username}`;
      hits = await redis.incr(rateKey);
      if (hits === 1) await redis.expire(rateKey, RATE_WINDOW_SEC);
    } catch (limiterErr) {
      console.warn("chat rate-limiter unavailable; failing open:", limiterErr);
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

    const seq = await appendMessage(db, sessionId, username, "chat", text);
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

    // Long-poll: hold the request open (≤2.5 s) until data shows up, then
    // return immediately. Cuts perceived delivery to <~300 ms while lowering
    // total request rate versus a fixed-interval client tick.
    const waitMs = Math.min(Math.max(Number(req.nextUrl.searchParams.get("wait") ?? 0) || 0, 0), 2500);
    const deadline = Date.now() + waitMs;

    const queryNew = () =>
      db
        .collection("session_messages")
        .find({ sessionId, seq: { $gt: afterSeq as number } })
        .sort({ seq: 1 })
        .limit(POLL_LIMIT)
        .toArray() as unknown as Promise<ChatDoc[]>;

    if (afterSeq !== null) {
      maybeCleanup();
      let docs = await queryNew();
      for (;;) {
        if (docs.length > 0 || Date.now() >= deadline) break;
        await new Promise((r) => setTimeout(r, 200));
        // Surface room termination without waiting out the full hold.
        const fresh = (await db
          .collection("sessions")
          .findOne({ sessionId }, { projection: { status: 1, expiresAt: 1 } })) as unknown as SessionAliveDoc | null;
        if (!isSessionAlive(fresh)) break;
        docs = await queryNew();
      }
      return NextResponse.json({
        messages: docs.map(toMessageDTO),
        ended: !alive,
      });
    }

    // No cursor → backlog: latest BACKLOG_LIMIT messages, oldest-first.
    const docs = await db
      .collection("session_messages")
      .find({ sessionId })
      .sort({ seq: -1 })
      .limit(BACKLOG_LIMIT)
      .toArray() as unknown as ChatDoc[];
    return NextResponse.json({
      messages: docs.reverse().map(toMessageDTO),
      ended: !alive,
    });
  } catch (error) {
    console.error("Error in GET /api/v1/session/[sessionId]/messages:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
