import { NextRequest, NextResponse } from "next/server";
import { apiError } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  getInvitesForUser,
  checkReadLimit,
  clientIpHash,
  assertUsernameHeader,
  getUserSigKey,
  claimUserSigKey,
} from "@/lib/rooms";
import { requireRequestSignature } from "@/lib/request-signature";

// GET /api/v1/invites/mine — every pending invite for the caller, across
// all rooms, from the per-user index (capped at 50, newest first).
//
// Response (200): { "invites": [
//   { "code": "123456", "groupName": "Squad" | null,
//     "groupDesc": "..." | null, "by": "alice", "at": 1720000000000 }
// ] }
// Rooms that died since the invite was sent are skipped and pruned from
// the index. Reads ride the shared read budget (like inbox/reaction polls).
export async function GET(req: NextRequest) {
  try {
    validateSignalingEnv();
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);
    assertUsernameHeader(username); // validate before budget keying
    // Signature gate with a self-claim anchor: an invitee has no roster
    // entry, so the FIRST signed poll binds the username to a device key
    // (SET NX, first claim wins — same trust pattern as roster claims).
    // Later polls must match that claim; an impostor's key never does.
    const claimed = await getUserSigKey(username);
    const headerPubkey = req.headers.get("X-Uplink-Pubkey") ?? "";
    const anchor = claimed ?? (headerPubkey || null);
    const sigGate = await requireRequestSignature(req, username, anchor);
    if (sigGate !== true) return sigGate;
    if (!claimed && anchor) await claimUserSigKey(username, anchor);
    await checkReadLimit(clientIpHash(req), username);

    const invites = await getInvitesForUser(username);
    return NextResponse.json({ invites });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in GET /api/v1/invites/mine:", error);
    return apiError("Internal server error", 500);
  }
}