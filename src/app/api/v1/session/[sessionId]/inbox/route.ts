import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import {
  RoomError,
  depositBox,
  fetchBoxes,
  checkSendLimit,
  clientIpHash,
} from "@/lib/rooms";

// Unified offline/fallback inbox. Boxes are ciphertext the server cannot
// read; they live at most 1 hour and die on explicit ACK. Two uses share it:
//   1. Offline delivery — recipient collects on reconnect.
//   2. Serverless fallback relay — when the P2P direct line fails, senders
//      deposit here and recipients fetch on a short poll.
// POST deposits one box (rate-limited per IP). GET fetches without deleting;
// deletion happens only via POST /inbox/ack, so a client crash between fetch
// and processing loses nothing.
export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { to, msgId, kind, payload } = parsed.body as {
      to?: unknown; msgId?: unknown; kind?: unknown; payload?: unknown;
    };

    await checkSendLimit("inbox", clientIpHash(req));
    await depositBox(sessionId, username, to as string, msgId as string, kind as string, payload as string);
    return NextResponse.json({ ok: true }, { status: 201 });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/inbox:", error);
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

    const boxes = await fetchBoxes(sessionId, username);
    return NextResponse.json({ boxes });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in GET /api/v1/session/[sessionId]/inbox:", error);
    return apiError("Internal server error", 500);
  }
}
