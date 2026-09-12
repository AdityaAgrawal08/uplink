import { NextRequest, NextResponse } from "next/server";
import { verifyPassword } from "@/lib/crypto";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import { RoomError, joinRoom, getRoomMeta } from "@/lib/rooms";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const { sessionId } = await props.params;
    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { username, pubkey, password } = parsed.body;

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
    }

    const roster = await joinRoom(sessionId, username as string, pubkey as string);
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
    });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/join:", error);
    return apiError("Internal server error", 500);
  }
}
