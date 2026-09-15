import { NextRequest, NextResponse } from "next/server";
import { hashPassword } from "@/lib/crypto";
import { apiError, parseJsonBody } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";

export const dynamic = "force-dynamic";
export const maxDuration = 10;
import {
  RoomError,
  createRoom,
  checkCreateLimit,
  clientIpHash,
} from "@/lib/rooms";

// Rooms live until the last member leaves — there is no duration/expiry
// parameter. Codes are 6 digits; membership carries each device's X25519
// public key so peers can E2E-encrypt with no global key directory.
export async function POST(req: NextRequest) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { username, pubkey, password } = parsed.body;

    // Per-IP room-creation budget (no accounts to throttle instead).
    await checkCreateLimit(clientIpHash(req));

    let passwordHash: string | null = null;
    if (password !== undefined && password !== null && password !== "") {
      if (typeof password !== "string" || password.length > 256) {
        return apiError("Password must be a string of at most 256 characters", 400);
      }
      passwordHash = await hashPassword(password);
    }

    const { sessionId } = await createRoom(
      username as string,
      pubkey as string,
      passwordHash
    );
    return NextResponse.json({ sessionId }, { status: 201 });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/create:", error);
    return apiError("Internal server error", 500);
  }
}
