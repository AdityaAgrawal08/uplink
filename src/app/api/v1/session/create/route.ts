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
  assertRoomCode,
} from "@/lib/rooms";

// Rooms live until the last member leaves — there is no duration/expiry
// parameter. Codes are 6 digits; membership carries each device's X25519
// public key so peers can E2E-encrypt with no global key directory.
// Optional group-room meta rides the same create call: groupName (1-64),
// groupDesc (0-256), maxMembers (int >= 2, or null = unlimited) and
// parentCode (the main room this group branches from) — all validated in
// createRoom, so lib and HTTP agree on the exact contract.
export async function POST(req: NextRequest) {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const parsed = await parseJsonBody(req);
    if (!parsed.ok) return apiError("Request body must be a JSON object", 400);
    const { username, pubkey, password, groupName, groupDesc, maxMembers, parentCode } = parsed.body;

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
      passwordHash,
      {
        groupName: groupName as string | undefined,
        groupDesc: groupDesc as string | undefined,
        maxMembers: maxMembers as number | null | undefined,
        parentCode: parentCode as string | undefined,
      }
    );
    return NextResponse.json({ sessionId }, { status: 201 });
  } catch (error) {
    if (error instanceof RoomError) return apiError(error.message, error.status);
    console.error("Error in POST /api/v1/session/create:", error);
    return apiError("Internal server error", 500);
  }
}
