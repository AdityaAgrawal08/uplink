import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { getPresignedDownloadUrl } from "@/lib/r2";
import { apiError } from "@/lib/api-utils";
import { consumeClassBQuota } from "@/lib/quota";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string; fileId: string }> }
) {
  try {
    const { sessionId, fileId } = await props.params;

    const db = await getDb();

    // 1. Find file in session_files
    const sessionFile = await db.collection("session_files").findOne({ sessionId, fileId });
    if (!sessionFile) {
      return apiError("File not found in session", 404);
    }

    if (sessionFile.status !== "UPLOADED") {
      return apiError("File upload is not complete", 400);
    }

    // 2. Find associated share document
    const share = await db.collection("shares").findOne({ shareId: sessionFile.shareId });
    if (!share) {
      return apiError("Associated share metadata not found", 404);
    }

    // 3. Operations Quota check
    try {
      const classBApproved = await consumeClassBQuota();
      if (!classBApproved) {
        return apiError("Service is temporarily unavailable due to operations quota limit exhaustion.", 503);
      }
    } catch (quotaErr) {
      console.error("Fail-closed: Class B quota check error in session download:", quotaErr);
      return apiError("Service is temporarily unavailable due to system quota validation failure.", 503);
    }

    // 4. Generate presigned download URL
    const downloadUrlExpiry = 3600; // 1 hour expiry
    const downloadUrl = await getPresignedDownloadUrl(
      share.objectKey,
      downloadUrlExpiry,
      share.storageFilename,
      share.mimeType,
      false
    );

    // 5. Increment download stats
    await db.collection("shares").updateOne(
      { shareId: share.shareId },
      {
        $inc: { downloadsCount: 1 },
        $set: { lastDownloadedAt: new Date() },
      }
    );

    return NextResponse.json({
      downloadUrl,
      filename: share.filename,
      size: share.size,
      mimeType: share.mimeType,
      hashValue: share.hashValue,
    });
  } catch (error) {
    console.error("Error in POST /api/v1/session/download:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
