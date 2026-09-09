import { NextRequest, NextResponse, after } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const since = req.nextUrl.searchParams.get("since");
    const conv = req.nextUrl.searchParams.get("conv");

    const db = await getDb();

    // 1. Mark stale ANNOUNCED files as UPLOAD_FAILED — non-blocking
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000);
    after(async () => {
      await db.collection("session_files").updateMany(
        { sessionId, status: "ANNOUNCED", uploadedAt: { $lt: fiveMinAgo } },
        { $set: { status: "UPLOAD_FAILED" } }
      ).catch(() => {});
    });

    // 2. Build query — optional conversation scope filter.
    //    conv="general" → public files only (to is empty/absent)
    //    conv="a|b" → private thread files where to matches the pair key
    //    conv omitted → all files (backward compat)
    const fileQuery: Record<string, unknown> = { sessionId };
    if (since) {
      const sinceDate = new Date(since);
      if (!isNaN(sinceDate.getTime())) {
        fileQuery.uploadedAt = { $gt: sinceDate };
      }
    }
    if (conv === "general") {
      // Public files: no `to` field, or `to` is empty
      fileQuery.$or = [{ to: { $exists: false } }, { to: "" }];
    } else if (conv && conv.includes("|")) {
      // Private thread: files addressed to this pair
      fileQuery.to = conv;
    }

    const [files, participants] = await Promise.all([
      db.collection("session_files").find(fileQuery).sort({ uploadedAt: 1 }).toArray(),
      db.collection("session_participants").find({ sessionId, status: "ACTIVE" }).project({ username: 1, peerId: 1, addrs: 1 }).toArray(),
    ]);

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
        to: (typeof f.to === "string" && f.to) || "",
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
