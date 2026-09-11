import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await props.params;

    const db = await getDb();

    // Find the active share
    const share = await db.collection("shares").findOne(
      { $or: [{ shareId: id }, { downloadCode: id }] },
      {
        projection: {
          shareId: 1,
          filename: 1,
          size: 1,
          mimeType: 1,
          hashValue: 1,
          passwordHash: 1,
          isEncrypted: 1,
          createdAt: 1,
          expiresAt: 1,
          status: 1,
          // B32 FIX: these two are returned in the response but were missing
          // from the projection, so they always came back undefined.
          downloadsCount: 1,
          downloadLimit: 1,
        },
      }
    );
    if (!share) {
      return NextResponse.json({ error: "Share not found" }, { status: 404 });
    }

    const now = new Date();
    if (new Date(share.expiresAt) < now || share.status === "EXPIRED" || share.status === "DELETED") {
      if (share.status !== "EXPIRED" && share.status !== "DELETED" && share.status !== "PENDING_DELETE") {
        await db.collection("shares").updateOne({ shareId: share.shareId }, { $set: { status: "EXPIRED" } });
      }
      return NextResponse.json({ error: "This share link has expired" }, { status: 410 });
    }

    if (share.status !== "ACTIVE") {
      return NextResponse.json(
        { error: `This share is not active (${share.status})` },
        { status: 400 }
      );
    }

    // Return non-sensitive metadata only
    return NextResponse.json({
      shareId: share.shareId,
      filename: share.filename,
      size: share.size,
      mimeType: share.mimeType,
      hashValue: share.hashValue,
      expiresAt: share.expiresAt,
      passwordRequired: !!share.passwordHash,
      downloadsCount: share.downloadsCount,
      downloadLimit: share.downloadLimit,
    });
  } catch (error: unknown) {
    console.error("Error in GET /api/v1/share/[id]:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return NextResponse.json({ error: errMsg }, { status: 500 });
  }
}
