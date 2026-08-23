import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const since = req.nextUrl.searchParams.get("since");

    const db = await getDb();

    // 1. Mark stale ANNOUNCED files as UPLOAD_FAILED
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000);
    await db.collection("session_files").updateMany(
      { sessionId, status: "ANNOUNCED", uploadedAt: { $lt: fiveMinAgo } },
      { $set: { status: "UPLOAD_FAILED" } }
    );

    // 2. Fetch session files
    const fileQuery: { sessionId: string; uploadedAt?: { $gt: Date } } = { sessionId };
    if (since) {
      const sinceDate = new Date(since);
      if (!isNaN(sinceDate.getTime())) {
        fileQuery.uploadedAt = { $gt: sinceDate };
      }
    }

    const files = await db
      .collection("session_files")
      .find(fileQuery)
      .sort({ uploadedAt: 1 })
      .toArray();

    // 3. Fetch active participants with P2P discovery info
    const participants = await db
      .collection("session_participants")
      .find({ sessionId, status: "ACTIVE" })
      .project({ username: 1, peerId: 1, addrs: 1 })
      .toArray();

    return NextResponse.json({
      files: files.map((f) => ({
        fileId: f.fileId,
        shareId: f.shareId,
        filename: f.filename,
        username: f.username,
        size: f.size,
        sha256: f.sha256,
        uploadedAt: f.uploadedAt.toISOString(),
        status: f.status,
      })),
      participants: participants.map((p) => ({
        username: p.username,
        peerId: p.peerId || null,
        addrs: p.addrs || [],
      })),
    });
  } catch (error) {
    console.error("Error in GET /api/v1/session/files:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
