import { NextRequest, NextResponse } from "next/server";
import { apiError } from "@/lib/api-utils";
import { RoomError, heartbeat } from "@/lib/rooms";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const usernameHeader = req.headers.get("X-Uplink-Username");
    if (!usernameHeader) {
      return apiError("X-Uplink-Username header is required", 400);
    }

    const text = await req.text();
    const body = text ? JSON.parse(text) : {};
    const { peerId, addrs } = body;

    const roster = await heartbeat(sessionId, usernameHeader, { peerId, addrs });
    const activeUsers = roster.filter((m) => m.online).map((m) => m.username);
    return NextResponse.json({ ok: true, activeUsers, roster });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/heartbeat:", error);
    return apiError("Internal server error", 500);
  }
}
