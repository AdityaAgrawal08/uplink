import { NextRequest, NextResponse, after } from "next/server";
import { getDb } from "@/lib/mongodb";
import { redis } from "@/lib/redis";
import { performSessionCleanup } from "../../cleanup/route";
import { apiError } from "@/lib/api-utils";
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

    const bodyText = await req.text();
    const body = bodyText ? JSON.parse(bodyText) : {};
    const text = sanitizeChatText(body.text);
    if (!text) return apiError("text must be 1-500 printable characters", 400);

    const db = await getDb();

    // Per-user flood guard (Redis-backed; degrades to per-instance in mock mode).
    const rateKey = `chat:${sessionId}:${username}`;
    const hits = await redis.incr(rateKey);
    if (hits === 1) await redis.expire(rateKey, RATE_WINDOW_SEC);
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
    const filter: Record<string, unknown> = { sessionId };
    if (afterParam !== null) {
      const afterSeq = Number(afterParam);
      if (!Number.isSafeInteger(afterSeq)) {
        return apiError("after must be an integer sequence number", 400);
      }
      filter.seq = { $gt: afterSeq };
      const docs = await db
        .collection("session_messages")
        .find(filter)
        .sort({ seq: 1 })
        .limit(POLL_LIMIT)
        .toArray() as unknown as ChatDoc[];
      return NextResponse.json({
        messages: docs.map(toMessageDTO),
        ended: !alive,
      });
    }

    // No cursor → backlog: latest BACKLOG_LIMIT messages, oldest-first.
    const docs = await db
      .collection("session_messages")
      .find(filter)
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
