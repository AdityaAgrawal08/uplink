import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";
import { getDb, initIndexes } from "@/lib/mongodb";
import { hashPassword } from "@/lib/crypto";
import { apiError } from "@/lib/api-utils";
import { BloomFilter } from "@/lib/bloom";

function generateSessionId(): string {
  const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  let result = "";
  for (let i = 0; i < 8; i++) {
    result += chars.charAt(crypto.randomInt(0, chars.length));
  }
  return result;
}

export async function POST(req: NextRequest) {
  try {
    const text = await req.text();
    const body = text ? JSON.parse(text) : {};
    const { username, password, duration } = body;

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

    // 2. Generate unique sessionId
    let sessionId = "";
    let attempts = 0;
    while (attempts < 10) {
      sessionId = generateSessionId();
      const existing = await db.collection("sessions").findOne({ sessionId });
      if (!existing) break;
      attempts++;
    }
    if (attempts === 10) {
      return apiError("Failed to generate a unique session ID", 500);
    }

    // 3. Create Bloom filter and add the creator
    const bloom = new BloomFilter(256, 3);
    bloom.add(username);

    // 4. Hash password if provided
    let passwordHash = null;
    if (password && typeof password === "string" && password.trim() !== "") {
      passwordHash = await hashPassword(password);
    }

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

    await db.collection("sessions").insertOne(sessionDoc);
    await db.collection("session_participants").insertOne(participantDoc);

    return NextResponse.json({ sessionId }, { status: 201 });
  } catch (error) {
    console.error("Error in POST /api/v1/session/create:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
