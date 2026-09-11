import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";
import { isSessionAlive } from "@/lib/sessionChat";
import { deleteObject } from "@/lib/r2";

export const dynamic = "force-dynamic";
export const maxDuration = 10;

export async function DELETE(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string; fileId: string }> }
) {
  try {
    const { sessionId, fileId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);

    const db = await getDb();
    const session = await db.collection("sessions").findOne({ sessionId });
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    if (!session || !isSessionAlive(session as any)) {
      return apiError("Session not found or has ended", 404);
    }

    const participant = await db.collection("session_participants").findOne({
      sessionId,
      username,
      status: "ACTIVE",
    });
    if (!participant) return apiError("Not in this session", 403);

    const file = await db.collection("session_files").findOne({ sessionId, fileId });
    if (!file) return apiError("File not found", 404);
    if (file.username !== username) return apiError("Only the uploader can delete this file", 403);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    if ((file as any).status === "DELETED") return apiError("File already deleted", 410);
    if (file.status !== "UPLOADED") return apiError("File not in deletable state", 400);

    const now = new Date();

    // Delete bytes from R2 (fire and forget, but await for correctness)
    let r2Deleted = true;
    try {
      const share = await db.collection("shares").findOne({ shareId: file.shareId });
      if (share && share.objectKey) {
        r2Deleted = await deleteObject(share.objectKey);
      }
      // Also delete share metadata
      if (share) {
        await db.collection("shares").deleteOne({ shareId: file.shareId });
      }
    } catch (e) {
      console.warn("R2 delete failed for", fileId, e);
      r2Deleted = false;
    }

    await db.collection("session_files").updateOne(
      { sessionId, fileId },
      {
        $set: {
          status: "DELETED",
          deletedAt: now,
          deletedBy: username,
          // keep filename/size for tombstone ordering but clear share link
          shareId: "",
        },
      }
    );

    return NextResponse.json({ ok: true, fileId, deletedAt: now.toISOString(), r2Deleted });
  } catch (error) {
    console.error("Error in DELETE /api/v1/session/[sessionId]/files/[fileId]:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
