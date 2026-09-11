import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";
import { getDb } from "@/lib/mongodb";
import { generateShareId } from "@/lib/crypto";
import { apiError } from "@/lib/api-utils";
import { conversationKey } from "@/lib/sessionChat";

const USERNAME_RE = /^[a-zA-Z0-9_]{3,20}$/;
const MAX_FILENAME_LEN = 255;

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
    const { filename, size, sha256, to } = body;

    if (!filename || typeof filename !== "string") {
      return apiError("Filename is required", 400);
    }
    // B28 FIX: Bound filename length to prevent unbounded DB growth.
    if (filename.length > MAX_FILENAME_LEN) {
      return apiError(`Filename must be at most ${MAX_FILENAME_LEN} characters`, 400);
    }
    // B28 FIX: size must be strictly positive (previously size===0 was allowed).
    if (typeof size !== "number" || !Number.isSafeInteger(size) || size <= 0) {
      return apiError("Valid positive file size is required", 400);
    }
    if (!sha256 || typeof sha256 !== "string" || sha256.length !== 64) {
      return apiError("Valid SHA-256 hash is required", 400);
    }

    // Optional: private conversation target. Empty/undefined = public (general).
    const toUser = typeof to === "string" && to.trim() !== "" ? to.trim() : "";
    // B28 FIX: Validate the recipient username format and reject self-targeting.
    if (toUser) {
      if (!USERNAME_RE.test(toUser)) {
        return apiError("to must be a valid username", 400);
      }
      if (toUser === usernameHeader) {
        return apiError("cannot target a private file to yourself", 400);
      }
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

    // B28 FIX: Verify the private recipient is actually an active member.
    if (toUser) {
      const recipient = await db.collection("session_participants").findOne({
        sessionId,
        username: toUser,
        status: "ACTIVE",
      });
      if (!recipient) {
        return apiError("recipient is not in this session", 404);
      }
    }

    const fileId = crypto.randomUUID();
    const shareId = generateShareId();

    const fileDoc: Record<string, unknown> = {
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
    if (toUser) {
      // B28 CRITICAL FIX: Store BOTH the raw recipient (`to`) and the
      // canonical conversation key (`convId`). Previously only the raw
      // recipient was stored, so the files route (which filters by the
      // canonical "a|b" pair key) never matched — private files were
      // unroutable and leaked to everyone when conv was omitted.
      fileDoc.to = toUser;
      fileDoc.convId = conversationKey(usernameHeader, toUser);
    } else {
      fileDoc.convId = "general";
    }

    await db.collection("session_files").insertOne(fileDoc);

    return NextResponse.json({ fileId, shareId }, { status: 201 });
  } catch (error) {
    console.error("Error in POST /api/v1/session/announce:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
