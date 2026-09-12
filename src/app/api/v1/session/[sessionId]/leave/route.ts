import { NextRequest, NextResponse } from "next/server";
import { apiError } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import { RoomError, leaveRoom } from "@/lib/rooms";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    // Idempotent: leaving twice reports the true count; the last member out
    // destroys the room instantly (rooms live till empty).
    const { remaining, ended } = await leaveRoom(sessionId, username);
    return NextResponse.json({ ok: true, remaining, ended });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/leave:", error);
    return apiError("Internal server error", 500);
  }
}
