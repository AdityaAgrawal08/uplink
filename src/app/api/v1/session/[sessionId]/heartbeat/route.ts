import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import { RoomError, heartbeat } from "@/lib/rooms";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const { sessionId } = await props.params;
    const usernameHeader = req.headers.get("X-Uplink-Username");
    if (!usernameHeader) {
      return apiError("X-Uplink-Username header is required", 400);
    }

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { peerId, addrs } = parsed.body as { peerId?: unknown; addrs?: unknown };

    // Rooms.heartbeat() validates both fields (400 on garbage), so the
    // narrow-down cast here is safe.
    const roster = await heartbeat(sessionId, usernameHeader, {
      peerId: peerId as string | undefined,
      addrs: addrs as string[] | undefined,
    });
    const activeUsers = roster.filter((m) => m.online).map((m) => m.username);
    return NextResponse.json({ ok: true, activeUsers, roster });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/heartbeat:", error);
    return apiError("Internal server error", 500);
  }
}
