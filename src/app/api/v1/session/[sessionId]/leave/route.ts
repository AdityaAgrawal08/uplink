import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";
import { appendMessage, isSessionAlive } from "@/lib/sessionChat";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    const db = await getDb();

    const session = (await db.collection("sessions").findOne({ sessionId })) as unknown as {
      status: string;
      expiresAt: Date | string;
    } | null;
    if (!isSessionAlive(session)) {
      return apiError("Session has ended", 410);
    }

    // Idempotent leave: only an ACTIVE member transitions to LEFT.
    const left = await db.collection("session_participants").findOneAndUpdate(
      { sessionId, username, status: "ACTIVE" },
      { $set: { status: "LEFT", leftAt: new Date() } }
    );

    if (!left) {
      return NextResponse.json({ ok: true, remaining: -1 });
    }

    await appendMessage(db, sessionId, "system", "system", `${username} left`);

    const remaining = await db.collection("session_participants").countDocuments({
      sessionId,
      status: "ACTIVE",
    });

    let ended = false;
    if (remaining === 0) {
      // Last member out — end the room instantly and purge its transcript.
      await db
        .collection("sessions")
        .updateOne(
          { sessionId, status: "ACTIVE" },
          { $set: { status: "ENDED", endedAt: new Date() } }
        );
      await db.collection("session_messages").deleteMany({ sessionId });
      ended = true;
    }

    return NextResponse.json({ ok: true, remaining, ended });
  } catch (error) {
    console.error("Error in POST /api/v1/session/[sessionId]/leave:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
