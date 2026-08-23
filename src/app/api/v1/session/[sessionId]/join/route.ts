import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { verifyPassword } from "@/lib/crypto";
import { apiError } from "@/lib/api-utils";
import { BloomFilter } from "@/lib/bloom";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const text = await req.text();
    const body = text ? JSON.parse(text) : {};
    const { username, password } = body;

    // 1. Validations
    if (!username || typeof username !== "string") {
      return apiError("Username is required", 400);
    }
    const usernameRegex = /^[a-zA-Z0-9_]{3,20}$/;
    if (!usernameRegex.test(username)) {
      return apiError("Username must be 3-20 characters and contain only alphanumeric characters and underscores", 400);
    }

    const db = await getDb();

    // 2. Fetch Session
    const session = await db.collection("sessions").findOne({ sessionId });
    if (!session) {
      return apiError("Session not found", 404);
    }

    if (session.status !== "ACTIVE" || new Date(session.expiresAt) < new Date()) {
      return apiError("Session has expired or is inactive", 410);
    }

    // 3. Verify Password if required
    if (session.passwordHash) {
      if (!password || typeof password !== "string") {
        return apiError("Password is required for this session", 401);
      }
      const isPwdValid = await verifyPassword(password, session.passwordHash);
      if (!isPwdValid) {
        return apiError("Incorrect session password", 401);
      }
    }

    // 4. Check Bloom filter for username uniqueness
    const bloom = BloomFilter.fromJSON(session.bloomFilter);
    let isTaken = false;

    if (bloom.contains(username)) {
      // Possible match or false positive. authorative check in DB:
      const existingParticipant = await db.collection("session_participants").findOne({
        sessionId,
        username,
      });
      if (existingParticipant) {
        isTaken = true;
      }
    }

    if (isTaken) {
      return apiError("Username already taken", 409);
    }

    // 5. Add username to Bloom Filter
    bloom.add(username);

    const now = new Date();
    const participantDoc = {
      sessionId,
      username,
      joinedAt: now,
      lastHeartbeat: now,
      status: "ACTIVE",
    };

    // Use atomic transaction-like updates or safe updates
    // In case of race conditions, unique index on { sessionId, username } will prevent duplicate insert
    try {
      await db.collection("session_participants").insertOne(participantDoc);
    } catch (dbErr) {
      const error = dbErr as { code?: number };
      if (error.code === 11000) {
        return apiError("Username already taken", 409);
      }
      throw dbErr;
    }

    // Update session document
    await db.collection("sessions").updateOne(
      { sessionId },
      {
        $set: { bloomFilter: bloom.toJSON() },
        $inc: { participantCount: 1 },
      }
    );

    // Fetch active participants to return
    const activeParticipants = await db
      .collection("session_participants")
      .find({ sessionId, status: "ACTIVE" })
      .toArray();

    return NextResponse.json({
      sessionId,
      participants: activeParticipants.map((p) => p.username),
    });
  } catch (error) {
    console.error("Error in POST /api/v1/session/join:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
