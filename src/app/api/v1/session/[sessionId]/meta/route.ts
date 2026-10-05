import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  updateRoomMeta,
  requireMember,
  checkSendLimit,
  clientIpHash,
  assertRoomCode,
  assertUsernameHeader,
  getMemberPubkey,
} from "@/lib/rooms";
import { requireRequestSignature } from "@/lib/request-signature";

// PATCH /api/v1/session/{id}/meta — rename a group / edit its description
// (creator or admin only; members get 403). Body: { groupName?, groupDesc? }
// — both optional, groupName 1-64 chars, groupDesc at most 256 (the same
// messages createRoom uses). Only the display fields move: passwordHash,
// creator, createdAt, maxMembers and parentCode are preserved.
//
// Responses: 200 { groupName, groupDesc } (null when never set); 400 bad
// shape/lengths; 401 missing/invalid signature or no roster anchor; 403
// member; 404 room gone; 429 send budget.
export async function PATCH(
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
    // Signature gate first: only the rostered key of `actor` may wield its
    // role (the roster entry is the trust anchor).
    const anchorPubkey = await getMemberPubkey(sessionId, actor);
    const sigGate = await requireRequestSignature(req, actor, anchorPubkey);
    if (sigGate !== true) return sigGate;
    // Membership before the budget: only members spend the send bucket.
    await requireMember(sessionId, actor);
    await checkSendLimit("meta", clientIpHash(req), actor);

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { groupName, groupDesc } = parsed.body;

    const meta = await updateRoomMeta(sessionId, actor, { groupName, groupDesc });
    return NextResponse.json(meta);
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in PATCH /api/v1/session/[sessionId]/meta:", error);
    return apiError("Internal server error", 500);
  }
}
