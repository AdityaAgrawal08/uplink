import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";
import { isSessionAlive } from "@/lib/sessionChat";

export const dynamic = "force-dynamic";
export const maxDuration = 10;

export async function DELETE(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string; seq: string }> }
) {
  try {
    const { sessionId, seq: seqStr } = await props.params;
    const seq = Number(seqStr);
    if (!Number.isSafeInteger(seq)) {
      return apiError("seq must be an integer", 400);
    }
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    const db = await getDb();
    const session = await db.collection("sessions").findOne({ sessionId });
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    if (!session || !isSessionAlive(session as any)) {
      return apiError("Session not found or has ended", 404);
    }

    const participant = await db.collection("session_participants").findOne({
      sessionId,
      username,
      status: "ACTIVE",
    });
    if (!participant) return apiError("Not in this session", 403);

    const msg = await db.collection("session_messages").findOne({ sessionId, seq });
    if (!msg) return apiError("Message not found", 404);

    // Only sender can delete own messages, no admin override, no system messages
    if (msg.kind === "system") return apiError("Cannot delete system message", 403);
    if (msg.username !== username) return apiError("Only the sender can delete this message", 403);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    if ((msg as any).status === "DELETED") return apiError("Message already deleted", 410);

    const now = new Date();
    await db.collection("session_messages").updateOne(
      { sessionId, seq },
      {
        $set: {
          status: "DELETED",
          text: "",
          deletedAt: now,
          deletedBy: username,
        },
      }
    );

    return NextResponse.json({ ok: true, seq, deletedAt: now.toISOString() });
  } catch (error) {
    console.error("Error in DELETE /api/v1/session/[sessionId]/messages/[seq]:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
