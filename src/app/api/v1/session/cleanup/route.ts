import { NextResponse } from "next/server";
import { apiError } from "@/lib/api-utils";
import { validateSignalingEnv } from "@/lib/env";
import { sweepRooms } from "@/lib/rooms";

// Cron + opportunistic trigger: prune lapsed heartbeats, destroy emptied
// rooms, drop vanished index entries. Per-room isolation inside sweepRooms;
// this handler only maps the outcome to HTTP.
export async function GET() {
  return performSessionCleanup();
}

export async function POST() {
  return performSessionCleanup();
}

export async function performSessionCleanup() {
  try {
    validateSignalingEnv(); // fail fast without Redis env (no silent MockRedis split-brain)
    const { processed, prunedMembers, destroyed } = await sweepRooms();
    return NextResponse.json({
      message: "Session cleanup complete",
      processedSessions: processed,
      prunedMembers,
      destroyedRooms: destroyed,
    });
  } catch (error) {
    console.error("Error in session cleanup:", error);
    return apiError("Internal server error", 500);
  }
}
