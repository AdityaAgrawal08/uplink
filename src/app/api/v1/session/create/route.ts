import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";
import { getDb, initIndexes } from "@/lib/mongodb";
import { hashPassword } from "@/lib/crypto";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { BloomFilter } from "@/lib/bloom";

export const dynamic = "force-dynamic";
export const maxDuration = 10;

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

    // Indexes: non-blocking — don't stall request on cold start
    initIndexes().catch(() => {});
    const db = await getDb();

    // 2. Generate unique sessionId + hash password in parallel
    const passwordHashPromise = password && typeof password === "string" && password.trim() !== ""
      ? hashPassword(password)
      : Promise.resolve(null);

    let sessionId = "";
    let attempts = 0;
    while (attempts < 10) {
      sessionId = generateSessionId();
      const existing = await db.collection("sessions").findOne({ sessionId }, { projection: { _id: 1 } });
      if (!existing) break;
      attempts++;
    }
    if (attempts === 10) {
      return apiError("Failed to generate a unique session ID", 500);
    }

    // 3. Create Bloom filter and add the creator
    const bloom = new BloomFilter(256, 3);
    bloom.add(username);

    // 4. Hash password (await parallel work)
    const passwordHash = await passwordHashPromise;

    const now = new Date();
    const expiresAt = new Date(now.getTime() + durationNum * 1000);

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

    const participantDoc = {
      sessionId,
      username,
      joinedAt: now,
      lastHeartbeat: now,
      status: "ACTIVE",
    };

    await Promise.all([
      db.collection("sessions").insertOne(sessionDoc),
      db.collection("session_participants").insertOne(participantDoc),
    ]);

    return NextResponse.json({ sessionId }, { status: 201 });
  } catch (error) {
    console.error("Error in POST /api/v1/session/create:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
