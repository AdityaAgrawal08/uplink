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
    //    For deleted tombstones, also return files where deletedAt > since
    const baseQuery: Record<string, unknown> = { sessionId };
    if (conv === "general") {
      baseQuery.$or = [{ to: { $exists: false } }, { to: "" }];
    } else if (conv && conv.includes("|")) {
      baseQuery.to = conv;
    }

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let files: any[] = [];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let participants: any[] = [];
    if (since) {
      const sinceDate = new Date(since);
      if (!isNaN(sinceDate.getTime())) {
        const [newFiles, deletedFiles, parts] = await Promise.all([
          db.collection("session_files").find({ ...baseQuery, uploadedAt: { $gt: sinceDate } }).sort({ uploadedAt: 1 }).toArray(),
          db.collection("session_files").find({ ...baseQuery, status: "DELETED", deletedAt: { $gt: sinceDate } }).sort({ uploadedAt: 1 }).toArray(),
          db.collection("session_participants").find({ sessionId, status: "ACTIVE" }).project({ username: 1, peerId: 1, addrs: 1 }).toArray(),
        ]);
        // Merge and dedupe by fileId
        const seen = new Set<string>();
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        files = [...newFiles, ...deletedFiles].filter((f: any) => {
          if (seen.has(f.fileId)) return false;
          seen.add(f.fileId);
          return true;
        });
        participants = parts;
      } else {
        [files, participants] = await Promise.all([
          db.collection("session_files").find(baseQuery).sort({ uploadedAt: 1 }).toArray(),
          db.collection("session_participants").find({ sessionId, status: "ACTIVE" }).project({ username: 1, peerId: 1, addrs: 1 }).toArray(),
        ]);
      }
    } else {
      [files, participants] = await Promise.all([
        db.collection("session_files").find(baseQuery).sort({ uploadedAt: 1 }).toArray(),
        db.collection("session_participants").find({ sessionId, status: "ACTIVE" }).project({ username: 1, peerId: 1, addrs: 1 }).toArray(),
      ]);
    }

    return NextResponse.json({
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      files: files.map((f: any) => ({
        fileId: f.fileId,
        shareId: f.shareId,
        filename: f.filename,
        username: f.username,
        size: f.size,
        sha256: f.sha256,
        uploadedAt: f.uploadedAt.toISOString(),
        status: f.status,
        to: (typeof f.to === "string" && f.to) || "",
        deletedAt: f.deletedAt ? (f.deletedAt as Date).toISOString() : undefined,
        deletedBy: f.deletedBy || undefined,
      })),
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      participants: participants.map((p: any) => ({
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
