import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { checkObjectExists } from "@/lib/r2";
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
    const { fileId, shareId } = body;

    if (!fileId || !shareId) {
      return apiError("fileId and shareId are required", 400);
    }

    const db = await getDb();

    // 1. Fetch the announced file to verify ownership and session matching
    const file = await db.collection("session_files").findOne({
      sessionId,
      fileId,
      shareId,
    });

    if (!file) {
      return apiError("File registration not found", 404);
    }

    if (file.username !== usernameHeader) {
      return apiError("You do not have permission to modify this file status", 403);
    }

    // B46 FIX: verify the bytes actually landed before advertising the file
    // as UPLOADED. Previously a client could announce + complete without
    // ever PUTting, creating phantom file cards whose downloads 404 at R2.
    // Idempotent: an already-UPLOADED file re-verifies cheaply via HEAD.
    const share = await db.collection("shares").findOne(
      { shareId },
      { projection: { objectKey: 1 } }
    );
    if (!share) {
      return apiError("Associated share metadata not found", 404);
    }
    const objDetails = await checkObjectExists(share.objectKey as string);
    if (!objDetails.exists) {
      return apiError("Uploaded file was not found in object storage", 404);
    }

    // 2. Update status to UPLOADED
    await db.collection("session_files").updateOne(
      { sessionId, fileId, shareId },
      { $set: { status: "UPLOADED", uploadedAt: new Date() } }
    );

    return NextResponse.json({ success: true });
  } catch (error) {
    console.error("Error in POST /api/v1/session/upload-complete:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
