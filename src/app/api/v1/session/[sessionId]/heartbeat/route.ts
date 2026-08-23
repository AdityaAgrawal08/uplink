import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";

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

    const db = await getDb();

    // 1. Fetch Session to ensure it is ACTIVE
    const session = await db.collection("sessions").findOne({ sessionId });
    if (!session) {
      return apiError("Session not found", 404);
    }
    if (session.status !== "ACTIVE") {
      return apiError("Session is not active", 410);
    }

    const now = new Date();

    // 2. Update participant heartbeat
    const updateFields: {
      lastHeartbeat: Date;
      status: string;
      peerId?: string;
      addrs?: string[];
    } = {
      lastHeartbeat: now,
      status: "ACTIVE",
    };
    if (peerId !== undefined) {
      updateFields.peerId = peerId;
    }
    if (addrs !== undefined) {
      updateFields.addrs = addrs;
    }

    const result = await db.collection("session_participants").findOneAndUpdate(
      { sessionId, username: usernameHeader },
      { $set: updateFields },
      { returnDocument: "before" }
    );

    if (!result) {
      return apiError("Participant not found in session", 404);
    }

    // 3. Self-heal if uploader was marked LEFT
    if (result.status === "LEFT") {
      await db.collection("sessions").updateOne(
        { sessionId },
        { $inc: { participantCount: 1 } }
      );
    }

    return NextResponse.json({ ok: true });
  } catch (error) {
    console.error("Error in POST /api/v1/session/heartbeat:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
