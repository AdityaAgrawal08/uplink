import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  declineInvite,
  checkSendLimit,
  clientIpHash,
  assertRoomCode,
  assertUsernameHeader,
  getUserSigKey,
  claimUserSigKey,
} from "@/lib/rooms";
import { requireRequestSignature } from "@/lib/request-signature";

// POST /api/v1/session/{id}/invites/decline — decline a pending invite
// without joining. Body: { code: string } — `code` must equal the path
// sessionId (the room the invite names). The invite is consumed in the
// room hash AND the caller's per-user index; unknown invites are 404
// ("Invite not found"), same as accept. Declines ride the shared send
// budget.
//
// Response: { "ok": true } (200).
export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv();
    const { sessionId } = await props.params;
    assertRoomCode(sessionId);
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);
    assertUsernameHeader(username);
    // Signature gate with a self-claim anchor (see invites/mine): declining
    // consumes an invite, so the claimed name must own the signing key.
    const claimed = await getUserSigKey(username);
    const headerPubkey = req.headers.get("X-Uplink-Pubkey") ?? "";
    const anchor = claimed ?? (headerPubkey || null);
    const sigGate = await requireRequestSignature(req, username, anchor);
    if (sigGate !== true) return sigGate;
    if (!claimed && anchor) await claimUserSigKey(username, anchor);

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { code } = parsed.body as { code?: unknown };
    assertRoomCode(code);
    if (code !== sessionId) return apiError("Session code mismatch", 400);

    await checkSendLimit("invites", clientIpHash(req), username);
    await declineInvite(sessionId, username);
    return NextResponse.json({ ok: true });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/invites/decline:", error);
    return apiError("Internal server error", 500);
  }
}