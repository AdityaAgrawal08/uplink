import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import {
  RoomError,
  depositSignal,
  drainSignals,
  checkSendLimit,
  checkReadLimit,
  scopedBudgetKey,
} from "@/lib/rooms";

// WebRTC rendezvous: members exchange SDP offers/answers and ICE candidates
// through per-user queues. POST deposits one note (rate-limited per IP);
// GET drains the caller's queue. Notes expire in minutes; polls during call
// setup only — steady-state chat never touches this endpoint.
export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { to, type, payload } = parsed.body as { to?: unknown; type?: unknown; payload?: unknown };

    await checkSendLimit("sig", scopedBudgetKey(req, username));
    await depositSignal(sessionId, username, to as string, type as string, payload as string);
    return NextResponse.json({ ok: true }, { status: 201 });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/signal:", error);
    return apiError("Internal server error", 500);
  }
}

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);
    await checkReadLimit(scopedBudgetKey(req, username)); // drains are the hot poll path

    const notes = await drainSignals(sessionId, username);
    return NextResponse.json({ notes });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in GET /api/v1/session/[sessionId]/signal:", error);
    return apiError("Internal server error", 500);
  }
}
