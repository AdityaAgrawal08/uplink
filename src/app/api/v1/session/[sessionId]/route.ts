import { NextRequest, NextResponse } from "next/server";
import { apiError } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  deleteRoom,
  requireCreator,
  checkSendLimit,
  clientIpHash,
  assertRoomCode,
  assertUsernameHeader,
  getMemberPubkey,
} from "@/lib/rooms";
import { requireRequestSignature } from "@/lib/request-signature";

// DELETE /api/v1/session/{id} — dissolve a group for everyone (creator only;
// admins and members get 403). destroyRoom purges the meta doc, roster,
// seat counter, invites, bans, reactions and per-member queues, and the code
// leaves the sweep index. Survivors see the room gone on their next
// heartbeat (404 "Session not found").
//
// Responses: 200 { ok: true }; 400 bad code/header; 401 missing/invalid
// signature or no roster anchor; 403 non-creator; 429 send budget.
export async function DELETE(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv();
    const { sessionId } = await props.params;
    assertRoomCode(sessionId);
    const actor = req.headers.get("X-Uplink-Username") || "";
    if (!actor) return apiError("X-Uplink-Username header is required", 400);
    assertUsernameHeader(actor);
    // Signature gate first: the crown only obeys the rostered key.
    const anchorPubkey = await getMemberPubkey(sessionId, actor);
    const sigGate = await requireRequestSignature(req, actor, anchorPubkey);
    if (sigGate !== true) return sigGate;
    // Creator check before the budget; deleteRoom re-checks it so the
    // mutation is self-guarding.
    await requireCreator(sessionId, actor);
    await checkSendLimit("meta", clientIpHash(req), actor);

    await deleteRoom(sessionId, actor);
    return NextResponse.json({ ok: true });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in DELETE /api/v1/session/[sessionId]:", error);
    return apiError("Internal server error", 500);
  }
}
