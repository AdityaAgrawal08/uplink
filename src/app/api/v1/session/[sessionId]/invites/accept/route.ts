import { NextRequest, NextResponse } from "next/server";
import { verifyPassword, getDummyPasswordHash } from "@/lib/crypto";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  acceptInvite,
  getRoomMeta,
  checkJoinLimit,
  checkRoomJoinLimit,
  clientIpHash,
  assertRoomCode,
  assertUsernameHeader,
} from "@/lib/rooms";

// POST /api/v1/session/{id}/invites/accept — accept a pending invite and
// join the room. Body: { code: string, pubkey: string, password?: string }.
// `code` must equal the path sessionId (the room the invite names); the
// caller is identified by the X-Uplink-Username header.
//
// Ordering mirrors the plain join path exactly: for password-protected
// rooms the password is verified FIRST (401 "Password is required for this
// session" / "Incorrect session password"), so the CLI keeps its two-step
// password modal; only then does the accept run — a full room returns 403
// "Maximum allowance is reached" AND consumes the invite; success joins the
// room and consumes the invite. Missing invites are 404 ("Invite not
// found"). Accepting is a join, so it rides the IP-scoped join budget.
//
// Success response — identical to POST /join:
// { "sessionId", "participants": [...], "roster": [{username, pubkey,
//   online, peerId?, addrs?}], "epoch" }.
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
    await checkJoinLimit(clientIpHash(req)); // accepting IS a join: membership mutation faucet

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { code, pubkey, password } = parsed.body as { code?: unknown; pubkey?: unknown; password?: unknown };
    assertRoomCode(code);
    if (code !== sessionId) return apiError("Session code mismatch", 400);

    // Finding 14: accepting IS a join — the per-room join budget applies
    // (before the password gate, so probes burn the room's budget).
    await checkRoomJoinLimit(sessionId);

    // Password gate FIRST (same verifyPassword path as POST /join): 401s
    // surface before any cap or join logic, keeping the CLI's two-step
    // modal intact. A room with no meta has no invite hash either (they
    // die together), so 404 names the invite, not the session.
    const meta = await getRoomMeta(sessionId);
    if (!meta) {
      return apiError("Invite not found", 404);
    }
    if (meta.passwordHash) {
      if (typeof password !== "string" || password.length === 0) {
        return apiError("Password is required for this session", 401);
      }
      if (!(await verifyPassword(password, meta.passwordHash))) {
        return apiError("Incorrect session password", 401);
      }
    } else {
      // Finding 15 (timing oracle): same equalization as POST /join — verify
      // against a dummy hash so unprotected rooms cost the same as a wrong
      // guess on a protected room. The accept still proceeds.
      await verifyPassword(typeof password === "string" ? password : "", await getDummyPasswordHash());
    }

    const { roster, epoch } = await acceptInvite(sessionId, username, pubkey as string);
    return NextResponse.json({
      sessionId,
      participants: roster.map((m) => m.username),
      roster: roster.map((m) => ({
        username: m.username,
        pubkey: m.pubkey,
        online: m.online,
        ...(m.peerId !== undefined ? { peerId: m.peerId } : {}),
        ...(m.addrs !== undefined ? { addrs: m.addrs } : {}),
      })),
      epoch, // joiner seeds its roster-change tracker (no extra round trip)
    });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/invites/accept:", error);
    return apiError("Internal server error", 500);
  }
}