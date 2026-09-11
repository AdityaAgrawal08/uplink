import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { getPresignedDownloadUrl } from "@/lib/r2";
import { apiError } from "@/lib/api-utils";
import { consumeClassBQuota } from "@/lib/quota";
import { isPairConv } from "@/lib/sessionChat";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string; fileId: string }> }
) {
  try {
    const { sessionId, fileId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";

    // B28 FIX: Require authentication. Previously `if (username)` skipped
    // the participant check when the header was absent, allowing anonymous
    // download of any session file — including private ones.
    if (!username) {
      return apiError("X-Uplink-Username header is required", 400);
    }

    const db = await getDb();

    // 0. Verify requester is an active session participant
    const participant = await db.collection("session_participants").findOne({
      sessionId,
      username,
      status: "ACTIVE",
    });
    if (!participant) {
      return apiError("You are not an active participant in this session", 403);
    }

    // 1. Find file in session_files
    const sessionFile = await db.collection("session_files").findOne({ sessionId, fileId });
    if (!sessionFile) {
      return apiError("File not found in session", 404);
    }

    if (sessionFile.status !== "UPLOADED") {
      return apiError("File upload is not complete", 400);
    }

    // 1a. Private file access check. `to` may be a raw recipient username
    //     (new docs) or a canonical pair key "a|b" (legacy CLI docs). Only
    //     the sender and the recipient(s) may download.
    const fileTo = typeof sessionFile.to === "string" ? sessionFile.to : "";
    const fileConvId = typeof sessionFile.convId === "string" ? sessionFile.convId : "";
    if (fileTo || (fileConvId && fileConvId !== "general")) {
      const pair = isPairConv(fileConvId) ? fileConvId : (isPairConv(fileTo) ? fileTo : "");
      const allowed = username === sessionFile.username
        || username === fileTo
        || (pair !== "" && (pair.split("|")[0] === username || pair.split("|")[1] === username));
      if (!allowed) {
        return apiError("You do not have access to this private file", 403);
      }
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
