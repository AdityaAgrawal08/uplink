import { NextRequest, NextResponse } from "next/server";
import { verifyPassword, getDummyPasswordHash } from "@/lib/crypto";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import { RoomError, joinRoom, getRoomMeta, checkJoinLimit, checkRoomJoinLimit, clientIpHash, assertRoomCode } from "@/lib/rooms";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    await checkJoinLimit(clientIpHash(req)); // membership mutations are Redis-op faucets
    const { sessionId } = await props.params;
    assertRoomCode(sessionId); // malformed codes are 400, not "not found"
    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { username, pubkey, password } = parsed.body;

    // Finding 14: per-ROOM join budget (anti-brute-force for 6-digit codes),
    // counted per well-formed attempt — before the password gate, so failed
    // guesses (the attacker's probes) burn the room's budget too.
    await checkRoomJoinLimit(sessionId);

    const meta = await getRoomMeta(sessionId);
    if (!meta) {
      return apiError("Session not found", 404);
    }

    if (meta.passwordHash) {
      if (typeof password !== "string" || password.length === 0) {
        return apiError("Password is required for this session", 401);
      }
      if (!(await verifyPassword(password, meta.passwordHash))) {
        return apiError("Incorrect session password", 401);
      }
    } else {
      // Finding 15 (timing oracle): burn the same argon2 work a protected
      // room burns per wrong guess — against a dummy hash — so response
      // timing cannot reveal which rooms are protected. The join still
      // succeeds; only the side channel is closed.
      await verifyPassword(typeof password === "string" ? password : "", await getDummyPasswordHash());
    }

    const { roster, epoch } = await joinRoom(sessionId, username as string, pubkey as string);
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
    console.error("Error in POST /api/v1/session/join:", error);
    return apiError("Internal server error", 500);
  }
}
