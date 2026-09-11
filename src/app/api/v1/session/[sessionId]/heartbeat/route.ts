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

    // B30 FIX: Validate username format (it is used in queries and echoed
    // into the roster). Also required before any DB work.
    if (!/^[a-zA-Z0-9_]{3,20}$/.test(usernameHeader)) {
      return apiError("Invalid X-Uplink-Username", 400);
    }

    const db = await getDb();

    // 1. Fetch Session to ensure it is ACTIVE and not expired
    const session = await db.collection("sessions").findOne({ sessionId });
    if (!session) {
      return apiError("Session not found", 404);
    }
    if (session.status !== "ACTIVE") {
      return apiError("Session is not active", 410);
    }
    // B30 FIX: Also honor expiresAt. Previously a session past its expiry
    // (but not yet swept by the cleanup worker) still accepted heartbeats.
    if (session.expiresAt && new Date(session.expiresAt) <= new Date()) {
      return apiError("Session has expired", 410);
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

    // B30 FIX: Claim the LEFT→ACTIVE transition atomically. Only the request
    // that actually flips the status may increment participantCount. The old
    // code read `status` before the update and incremented afterwards, so
    // two concurrent heartbeats could both observe LEFT and double-increment.
    const revived = await db.collection("session_participants").findOneAndUpdate(
      { sessionId, username: usernameHeader, status: "LEFT" },
      { $set: updateFields },
      { returnDocument: "after" }
    );

    if (revived) {
      // Exactly one caller wins this branch.
      await db.collection("sessions").updateOne(
        { sessionId },
        { $inc: { participantCount: 1 } }
      );
    } else {
      // Already ACTIVE (or missing) — plain heartbeat update.
      const existing = await db.collection("session_participants").findOneAndUpdate(
        { sessionId, username: usernameHeader },
        { $set: updateFields }
      );
      if (!existing) {
        return apiError("Participant not found in session", 404);
      }
    }

    // 4. Live roster so clients can surface joins/leaves without extra calls
    const activeUsers = (
      await db
        .collection("session_participants")
        .find({ sessionId, status: "ACTIVE" })
        .project({ username: 1, _id: 0 })
        .toArray()
    ).map((u) => u.username);

    return NextResponse.json({ ok: true, activeUsers });
  } catch (error) {
    console.error("Error in POST /api/v1/session/heartbeat:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
