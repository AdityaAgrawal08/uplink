import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  toggleReaction,
  fetchReactions,
  checkSendLimit,
  checkReadLimit,
  scopedBudgetKey,
  assertUsernameHeader,
} from "@/lib/rooms";

// Server-synced message reactions. One hash per room
// (`{msgId}|{emoji}|{username}` -> "1"), so the client's 2s poll gets every
// count for every on-screen message in a single GET. POST toggles one
// reaction: same emoji removes it, a different emoji replaces the caller's
// prior reaction on that message. Only members may read or write.
//
// GET response contract (backward compatible — counts/mine untouched):
//   { "reactions": [
//       { "msgId": "...",
//         "counts": { "👍": 2 },             // emoji -> total (allowlist order)
//         "mine": ["👍"],                    // caller's own emojis
//         "details": [                       // for the per-message detail bar
//           { "emoji": "👍", "usernames": ["alice", "bob"] }
//         ] } ] }
// `details` is present for every summary (empty array when no reactors):
// allowlist emoji order, usernames ascending, at most 20 per emoji (counts
// still carry the full total, so clients can render "+N more"). Like counts,
// details is room-scoped and keyed by sender-assigned msgId; broadcast and DM
// ids share one space, so DM privacy stays payload-level E2E (see rooms.ts).
export const dynamic = "force-dynamic"; // GET aggregates live counts: never cacheable

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);
    assertUsernameHeader(username); // validate before budget keying (outer catch maps 400)

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { msgId, emoji } = parsed.body as { msgId?: unknown; emoji?: unknown };

    await checkSendLimit("reactions", scopedBudgetKey(req, username));
    const result = await toggleReaction(sessionId, username, msgId as string, emoji as string);
    return NextResponse.json(result);
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/reactions:", error);
    return apiError("Internal server error", 500);
  }
}

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv();
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);
    assertUsernameHeader(username);
    await checkReadLimit(scopedBudgetKey(req, username)); // rides the same poll budget as inbox/signal

    // Optional narrowing: only the messages the caller currently renders.
    // `?msgIds=a,b,c` (max 50); absent = whole-room aggregate, capped in lib.
    const rawMsgIds = req.nextUrl.searchParams.get("msgIds");
    let msgIds: string[] | undefined;
    if (rawMsgIds !== null) {
      msgIds = rawMsgIds.split(",").map((id) => id.trim()).filter((id) => id.length > 0);
      if (msgIds.length === 0) return apiError("msgIds must contain at least one message ID", 400);
    }

    const { reactions } = await fetchReactions(sessionId, username, msgIds);
    return NextResponse.json({ reactions });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in GET /api/v1/session/[sessionId]/reactions:", error);
    return apiError("Internal server error", 500);
  }
}
