import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { getObjectText, PREVIEW_TEXT_MAX_BYTES } from "@/lib/r2";
import { verifyPassword } from "@/lib/crypto";
import { consumeClassBQuota } from "@/lib/quota";
import { redis } from "@/lib/redis";
import { clientIpHash } from "@/lib/rooms";
import { apiError } from "@/lib/api-utils";

// B56 FIX (finding 7): preview-text previously (a) skipped the share's
// download counter entirely — an attacker could preview a limited share an
// unbounded number of times — (b) buffered the whole object into memory
// (getObjectText read the full file), and (c) had no password lockout, so
// the endpoint doubled the brute-force surface of authorize-download. Now:
//  1. Preview credits count against downloadsCount/downloadLimit with the
//     same atomic findOneAndUpdate as authorize-download (limit reached →
//     410).
//  2. getObjectText streams/truncates at ~100KB+1 (Range header on R2,
//     bounded read on mock storage) — memory is capped regardless of file
//     size.
//  3. Wrong passwords trip the SAME failKey counter authorize-download uses
//     (incr-first, bounded at 5 per IP+share per 5 min), so the two
//     endpoints cannot double the guess budget.
export async function POST(
  req: NextRequest,
  props: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await props.params;
    const body = await req.json().catch(() => ({}));
    const { password } = body;

    // B33 FIX: rate limit preview requests per IP. Previously an attacker
    // could brute-force a share password here without any limiter, and could
    // force unbounded GetObject calls.
    const ipHash = clientIpHash(req);
    const rateKey = `rate:preview:${ipHash}`;
    try {
      const hits = await redis.incr(rateKey);
      if (hits === 1) await redis.expire(rateKey, 300);
      if (hits > 30) {
        return apiError("Too many preview requests. Locked out for 5 minutes.", 429);
      }
    } catch (limiterErr) {
      console.warn("preview rate-limiter unavailable; failing open:", limiterErr);
    }

    const db = await getDb();
    const share = (await db.collection("shares").findOne({ $or: [{ shareId: id }, { downloadCode: id }] })) as {
      shareId: string;
      status: string;
      expiresAt: Date | string;
      passwordHash?: string;
      objectKey: string;
      isEncrypted?: boolean;
    } | null;

    if (!share) {
      return apiError("Share not found", 404);
    }

    // B33 FIX: honor expiresAt. Previously only status was checked, so a
    // share past its expiry (but not yet swept) could still be previewed.
    if (new Date(share.expiresAt) < new Date()) {
      return apiError("This share link has expired", 410);
    }

    if (share.status !== "ACTIVE") {
      return apiError("Share is not active", 400);
    }

    // Encrypted shares hold ciphertext — text preview would only leak noise.
    if (share.isEncrypted) {
      return apiError("Encrypted shares cannot be previewed as text", 400);
    }

    // Finding 7: password lockout shared with authorize-download, with the
    // same incr-FIRST discipline (the count is bumped before the argon2
    // verify, so parallel wrong guesses cannot race past the check — each
    // concurrent attempt still verifies at most once) and del on success.
    // The lockout also reuses the /[id]/authorize-download failKey, so the
    // two password surfaces share ONE budget instead of doubling it.
    const failKey = `rate:download:fail:${ipHash}:${share.shareId}`;
    if (share.passwordHash) {
      if (!password) {
        return NextResponse.json({ error: "Password required" }, { status: 401 });
      }
      const attempts = await redis.incr(failKey);
      if (attempts === 1) await redis.expire(failKey, 300);
      if (attempts > 5) {
        return apiError("Too many failed attempts. Locked out for 5 minutes.", 429);
      }
      const isValid = await verifyPassword(password, share.passwordHash);
      if (!isValid) {
        return NextResponse.json({ error: "Incorrect password" }, { status: 401 });
      }
      await redis.del(failKey);
    }

    // Finding 7: previews consume download credits atomically — same
    // findOneAndUpdate pattern as authorize-download, so a preview flood
    // exhausts the share's downloadLimit exactly like downloads do.
    const now = new Date();
    const result = await db.collection("shares").findOneAndUpdate(
      {
        shareId: share.shareId,
        status: "ACTIVE",
        $expr: { $lt: ["$downloadsCount", "$downloadLimit"] },
      },
      [
        {
          $set: {
            downloadsCount: { $add: ["$downloadsCount", 1] },
            lastDownloadedAt: now,
            firstDownloadedAt: { $ifNull: ["$firstDownloadedAt", now] },
          },
        },
      ],
      { returnDocument: "after" }
    );
    if (!result) {
      const refreshed = await db.collection("shares").findOne({ shareId: share.shareId });
      if (refreshed && refreshed.downloadsCount >= refreshed.downloadLimit) {
        return apiError("Download limit exceeded for this file", 410);
      }
      return apiError("Preview authorization failed", 400);
    }

    // B33 FIX: consuming an object counts as a Class B operation. Account for
    // it so preview traffic cannot silently exhaust the daily R2 budget.
    try {
      const classBApproved = await consumeClassBQuota();
      if (!classBApproved) {
        return apiError("Service is temporarily unavailable due to operations quota limit exhaustion.", 503);
      }
    } catch (quotaErr) {
      console.error("Fail-closed: Class B quota check error in preview-text:", quotaErr);
      return apiError("Service is temporarily unavailable due to system quota validation failure.", 503);
    }

    // Finding 7: getObjectText caps memory at ~100KB+1 (Range/truncation);
    // the response is additionally sliced to 100KB.
    const text = await getObjectText(share.objectKey);
    return NextResponse.json({ text: text.slice(0, PREVIEW_TEXT_MAX_BYTES) });
  } catch (err: unknown) {
    console.error("Error in POST /api/v1/share/[id]/preview-text:", err);
    return apiError("Internal server error", 500);
  }
}