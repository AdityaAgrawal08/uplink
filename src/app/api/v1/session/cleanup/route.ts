import { NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";

export async function GET() {
  return performSessionCleanup();
}

export async function POST() {
  return performSessionCleanup();
}

export async function performSessionCleanup() {
  try {
    const db = await getDb();
    const now = new Date();
    const heartbeatTimeout = 45 * 1000; // 45 s — clients beat every 15 s (3 missed = gone)
    const gracePeriod = 120 * 1000; // 120 seconds grace period

    // 1. Find all ACTIVE sessions
    const activeSessions = await db
      .collection("sessions")
      .find({ status: "ACTIVE" })
      .toArray();

    const results = [];

    for (const session of activeSessions) {
      const sessionId = session.sessionId;

      // 2. Query participants who are ACTIVE
      const activeParticipants = await db
        .collection("session_participants")
        .find({ sessionId, status: "ACTIVE" })
        .toArray();

      let leftCount = 0;
      for (const p of activeParticipants) {
        const lastHeartbeat = new Date(p.lastHeartbeat);
        if (now.getTime() - lastHeartbeat.getTime() > heartbeatTimeout) {
          // Participant is stale. Mark as LEFT.
          await db.collection("session_participants").updateOne(
            { _id: p._id },
            { $set: { status: "LEFT" } }
          );
          leftCount++;
        }
      }

      // B5 FIX: Recompute participantCount from actual ACTIVE count instead
      // of arithmetic on potentially-stale document value. This prevents
      // drift from race conditions between join/leave/cleanup.
      const actualActiveCount = await db
        .collection("session_participants")
        .countDocuments({ sessionId, status: "ACTIVE" });

      if (session.participantCount !== actualActiveCount) {
        await db.collection("sessions").updateOne(
          { sessionId },
          { $set: { participantCount: actualActiveCount } }
        );
      }

      let expiresAt = new Date(session.expiresAt);

      // 3. If no active participants, schedule early expiry (min of current expiresAt and now + gracePeriod)
      if (actualActiveCount === 0) {
        const earlyExpiry = new Date(now.getTime() + gracePeriod);
        if (earlyExpiry < expiresAt) {
          expiresAt = earlyExpiry;
          await db.collection("sessions").updateOne(
            { sessionId },
            { $set: { expiresAt } }
          );
        }
      }

      // 4. Check if session has expired
      if (now > expiresAt) {
        // B4 FIX: Mark session as EXPIRED and set cleanupInitiated flag BEFORE
        // deleting messages. This prevents the share cleanup worker from
        // processing this session's shares while messages are being purged.
        await db.collection("sessions").updateOne(
          { sessionId },
          { $set: { status: "EXPIRED", cleanupInitiated: true } }
        );

        // Fetch session files to expire underlying shares
        const sFiles = await db
          .collection("session_files")
          .find({ sessionId })
          .toArray();

        for (const file of sFiles) {
          // Set share expiresAt to now so main cleanup sweeps it
          await db.collection("shares").updateOne(
            { shareId: file.shareId },
            { $set: { expiresAt: now } }
          );
        }

        // Delete session_files references as they are expired
        const deleteFilesResult = await db
          .collection("session_files")
          .deleteMany({ sessionId });

        // Chat transcript dies with the room (privacy: purge-on-end policy)
        await db
          .collection("session_messages")
          .deleteMany({ sessionId });

        results.push({
          sessionId,
          expired: true,
          filesCleaned: deleteFilesResult.deletedCount,
        });
      } else {
        results.push({
          sessionId,
          expired: false,
          activeParticipants: actualActiveCount,
        });
      }
    }

    return NextResponse.json({
      message: "Session cleanup complete",
      processedSessions: activeSessions.length,
      details: results,
    });
  } catch (error) {
    console.error("Error in session cleanup:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
