import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";
import { getDb, initIndexes } from "@/lib/mongodb";
import { hashPassword } from "@/lib/crypto";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { BloomFilter } from "@/lib/bloom";

// 6-digit numeric key (e.g. "042917") — the shareable chat room code.
function generateSessionId(): string {
  return String(crypto.randomInt(0, 1000000)).padStart(6, "0");
}

export async function POST(req: NextRequest) {
  try {
    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { username, password, duration } = parsed.body;

    // 1. Username validation
    if (!username || typeof username !== "string") {
      return apiError("Username is required", 400);
    }
    const usernameRegex = /^[a-zA-Z0-9_]{3,20}$/;
    if (!usernameRegex.test(username)) {
      return apiError("Username must be 3-20 characters and contain only alphanumeric characters and underscores", 400);
    }

    const durationNum = duration ? Number(duration) : 600;
    if (isNaN(durationNum) || durationNum < 60 || durationNum > 3600) {
      return apiError("Duration must be between 60 and 3600 seconds", 400);
    }

    // Ensure MongoDB indexes are initialized
    await initIndexes();
    const db = await getDb();

    // 3. Create Bloom filter with the creator
    const bloom = new BloomFilter(256, 3);
    bloom.add(username);

    // 4. Hash password if provided
    let passwordHash = null;
    if (password && typeof password === "string" && password.trim() !== "") {
      passwordHash = await hashPassword(password);
    }

    const now = new Date();
    const expiresAt = new Date(now.getTime() + durationNum * 1000);

    // B34 FIX: Insert atomically with retry on duplicate sessionId.
    // The old check-then-insert was a TOCTOU race: two concurrent creates
    // could pick the same 6-digit id, and the second insertOne threw a
    // duplicate-key error that surfaced as a 500.
    let sessionId = "";
    let inserted = false;
    for (let attempt = 0; attempt < 10; attempt++) {
      sessionId = generateSessionId();
      const sessionDoc = {
        sessionId,
        passwordHash,
        createdAt: now,
        expiresAt,
        sessionDuration: durationNum,
        bloomFilter: bloom.toJSON(),
        participantCount: 1,
        status: "ACTIVE",
      };
      try {
        await db.collection("sessions").insertOne(sessionDoc);
        inserted = true;
        break;
      } catch (dbErr) {
        if ((dbErr as { code?: number }).code === 11000) {
          continue; // id collision — regenerate
        }
        throw dbErr;
      }
    }
    if (!inserted) {
      return apiError("Failed to generate a unique session ID", 500);
    }

    const participantDoc = {
      sessionId,
      username,
      joinedAt: now,
      lastHeartbeat: now,
      status: "ACTIVE",
    };

    // B34 FIX: If the participant insert fails, roll back the session so we
    // don't leave an orphaned ACTIVE session with participantCount=1 and no
    // members (which the cleanup worker would keep alive).
    try {
      await db.collection("session_participants").insertOne(participantDoc);
    } catch (participantErr) {
      await db.collection("sessions").deleteOne({ sessionId }).catch(() => {});
      throw participantErr;
    }

    return NextResponse.json({ sessionId }, { status: 201 });
  } catch (error) {
    console.error("Error in POST /api/v1/session/create:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
