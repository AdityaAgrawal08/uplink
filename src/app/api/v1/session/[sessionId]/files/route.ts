import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { apiError } from "@/lib/api-utils";
import { GENERAL_CONV, isPairConv, conversationKey } from "@/lib/sessionChat";

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ sessionId: string }> }
) {
  try {
    const { sessionId } = await props.params;
    const username = req.headers.get("X-Uplink-Username") || "";
    if (!username) return apiError("X-Uplink-Username header is required", 400);
    // B28 FIX: Validate username format. It is interpolated into a $regex
    // below; unvalidated input could inject regex metacharacters (ReDoS /
    // unintended matches).
    if (!/^[a-zA-Z0-9_]{3,20}$/.test(username)) {
      return apiError("Invalid X-Uplink-Username", 400);
    }

    const since = req.nextUrl.searchParams.get("since");
    const conv = req.nextUrl.searchParams.get("conv");

    const db = await getDb();

    // 1. Mark stale ANNOUNCED files as UPLOAD_FAILED
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000);
    await db.collection("session_files").updateMany(
      { sessionId, status: "ANNOUNCED", uploadedAt: { $lt: fiveMinAgo } },
      { $set: { status: "UPLOAD_FAILED" } }
    );

    // 2. Build query.
    //    B28 CRITICAL FIX: Always apply a visibility filter so a caller can
    //    never see another user's private files. Previously, omitting `conv`
    //    returned EVERY file in the room, leaking private threads.
    //    Visibility: public files + files I sent privately + files sent to me.
    //    Supports both legacy (`to` = raw username) and new (`convId`) docs.
    const visibilityOr: Record<string, unknown>[] = [
      { convId: GENERAL_CONV },
      { convId: { $exists: false }, to: { $exists: false } },
      { convId: { $exists: false }, to: "" },
      { username },                                  // files I sent
      { to: username },                              // legacy: files sent to me
      // Legacy pair-key docs: convId exists and contains me as a token.
      { convId: { $regex: `(^|\\|)${username}(\\||$)` } },
      // Legacy pair-key docs stored in `to` ("a|b") where I am a token.
      { convId: { $exists: false }, to: { $regex: `(^|\\|)${username}(\\||$)` } },
    ];

    const fileQuery: Record<string, unknown> = {
      sessionId,
      $or: visibilityOr,
    };

    if (since) {
      const sinceDate = new Date(since);
      if (!isNaN(sinceDate.getTime())) {
        fileQuery.uploadedAt = { $gt: sinceDate };
      }
    }

    // 3. Optional conversation scope narrowing.
    if (conv === GENERAL_CONV) {
      // Public files only.
      fileQuery.$and = [
        { $or: [{ convId: GENERAL_CONV }, { convId: { $exists: false }, to: { $exists: false } }, { convId: { $exists: false }, to: "" }] },
      ];
      delete fileQuery.$or;
    } else if (conv && isPairConv(conv)) {
      const [u1, u2] = conv.split("|");
      if (username !== u1 && username !== u2) {
        return apiError("not a participant of this conversation", 403);
      }
      // Match canonical convId OR legacy pair-key `to` OR legacy raw
      // recipient `to` for either party.
      delete fileQuery.$or;
      fileQuery.$and = [
        { $or: [{ convId: conv }, { to: conv }, { to: u1 }, { to: u2 }] },
      ];
    }

    const files = await db
      .collection("session_files")
      .find(fileQuery)
      .sort({ uploadedAt: 1 })
      .toArray();

    // 4. Fetch active participants with P2P discovery info
    const participants = await db
      .collection("session_participants")
      .find({ sessionId, status: "ACTIVE" })
      .project({ username: 1, peerId: 1, addrs: 1 })
      .toArray();

    return NextResponse.json({
      files: files.map((f) => {
        // Resolve the canonical conversation bucket so clients don't have to
        // re-derive it (and to bridge legacy docs).
        const rawTo = typeof f.to === "string" ? f.to : "";
        const convId = typeof f.convId === "string" && f.convId
          ? f.convId
          : rawTo
            ? (isPairConv(rawTo) ? rawTo : conversationKey(f.username as string, rawTo))
            : GENERAL_CONV;
        return {
          fileId: f.fileId,
          shareId: f.shareId,
          filename: f.filename,
          username: f.username,
          size: f.size,
          sha256: f.sha256,
          uploadedAt: f.uploadedAt.toISOString(),
          status: f.status,
          to: rawTo,
          convId,
        };
      }),
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
