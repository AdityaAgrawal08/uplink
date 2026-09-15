import { NextRequest, NextResponse } from "next/server";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import { RoomError, ackBoxes, checkReadLimit, scopedBudgetKey, assertUsernameHeader } from "@/lib/rooms";

// Explicit acknowledgement: deletes exactly the listed boxes. A message is
// forgotten by the server only after the recipient confirms receipt.
export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);
    assertUsernameHeader(username);
    await checkReadLimit(scopedBudgetKey(req, username)); // high-frequency acks share the read budget

    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);

    const { removed, skipped } = await ackBoxes(sessionId, username, (parsed.body as { ids?: unknown }).ids);
    return NextResponse.json({ ok: true, removed, skipped });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/[sessionId]/inbox/ack:", error);
    return apiError("Internal server error", 500);
  }
}
