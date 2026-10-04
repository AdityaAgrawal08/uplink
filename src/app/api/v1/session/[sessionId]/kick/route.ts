import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  kickMember,
  checkJoinLimit,
  clientIpHash,
  assertRoomCode,
  assertUsername,
  assertUsernameHeader,
  getMemberPubkey,
} from "@/lib/rooms";
import { requireRequestSignature } from "@/lib/request-signature";

// POST /api/v1/session/{id}/kick — remove a user (creator/admin only).
// The target is banned while the room lives and their transient queues are
// wiped. Returns the fresh roster + epoch so the actor's client refreshes
// without waiting for the next beat.
export async function POST(
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
    // Signature gate: only a key actually rostered for `actor` can kick.
    const anchorPubkey = await getMemberPubkey(sessionId, actor);
    const sigGate = await requireRequestSignature(req, actor, anchorPubkey);
    if (sigGate !== true) return sigGate;
    await checkJoinLimit(clientIpHash(req)); // membership mutation faucet

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { target } = parsed.body as { target?: unknown };
    assertUsername(target);

    const { roster, epoch, remaining } = await kickMember(sessionId, actor, target as string);
    return NextResponse.json({ ok: true, roster, epoch, remaining });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/kick:", error);
    return apiError("Internal server error", 500);
  }
}
