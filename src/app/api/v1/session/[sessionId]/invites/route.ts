import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  createInvite,
  checkSendLimit,
  scopedBudgetKey,
  assertRoomCode,
  assertUsername,
  assertUsernameHeader,
} from "@/lib/rooms";

// POST /api/v1/session/{id}/invites — invite a user to this room.
// Body: { username: string } (the invitee; must be 3-20
// alphanumeric/underscore chars). ANY member of the room may invite;
// existing members and banned users cannot be invited; re-inviting an
// already-pending user refreshes the invite in place. Sends ride the
// shared per-user+IP send budget.
//
// Response: { "ok": true } (201). Errors: 400 bad shape, 403 not a member
// / banned invitee, 404 room not found, 409 already a member, 429 rate or
// per-user pending-invite cap (50).
export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv();
    const { sessionId } = await props.params;
    assertRoomCode(sessionId);
    const inviter = req.headers.get("X-Uplink-Username") || "";
    if (!inviter) return apiError("X-Uplink-Username header is required", 400);
    assertUsernameHeader(inviter); // validate before budget keying (outer catch maps 400)

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { username } = parsed.body as { username?: unknown };
    assertUsername(username); // invitee format gate (400 on bad usernames)

    await checkSendLimit("invites", scopedBudgetKey(req, inviter));
    await createInvite(sessionId, inviter, username as string);
    return NextResponse.json({ ok: true }, { status: 201 });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/invites:", error);
    return apiError("Internal server error", 500);
  }
}