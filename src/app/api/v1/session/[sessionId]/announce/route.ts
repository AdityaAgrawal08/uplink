import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";
import { getDb } from "@/lib/mongodb";
import { generateShareId } from "@/lib/crypto";
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
    const { filename, size, sha256 } = body;

    if (!filename || typeof filename !== "string") {
      return apiError("Filename is required", 400);
    }
    if (size === undefined || typeof size !== "number" || size < 0) {
      return apiError("Valid file size is required", 400);
    }
    if (!sha256 || typeof sha256 !== "string" || sha256.length !== 64) {
      return apiError("Valid SHA-256 hash is required", 400);
    }

    const db = await getDb();

    // 1. Verify participant is active in session
    const participant = await db.collection("session_participants").findOne({
      sessionId,
      username: usernameHeader,
      status: "ACTIVE",
    });

    if (!participant) {
      return apiError("Participant is not active in this session", 403);
    }

    const fileId = crypto.randomUUID();
    const shareId = generateShareId();

    const fileDoc = {
      sessionId,
      fileId,
      shareId,
      filename,
      username: usernameHeader,
      size,
      sha256,
      uploadedAt: new Date(),
      status: "ANNOUNCED",
    };

    await db.collection("session_files").insertOne(fileDoc);

    return NextResponse.json({ fileId, shareId }, { status: 201 });
  } catch (error) {
    console.error("Error in POST /api/v1/session/announce:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
