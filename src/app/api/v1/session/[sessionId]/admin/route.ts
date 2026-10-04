import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  setRole,
  checkJoinLimit,
  clientIpHash,
  assertRoomCode,
  assertUsername,
  assertUsernameHeader,
} from "@/lib/rooms";

// POST /api/v1/session/{id}/admin — grant/revoke admin (creator only).
// Body: { target: string, admin: boolean }. Returns the fresh roster +
// epoch so the actor's client refreshes without waiting for the next beat.
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
    await checkJoinLimit(clientIpHash(req)); // membership mutation faucet

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { target, admin } = parsed.body as { target?: unknown; admin?: unknown };
    assertUsername(target);
    if (typeof admin !== "boolean") return apiError("admin must be a boolean", 400);

    const { roster, epoch } = await setRole(sessionId, actor, target as string, admin ? "admin" : "member");
    return NextResponse.json({ ok: true, roster, epoch });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/admin:", error);
    return apiError("Internal server error", 500);
  }
}
