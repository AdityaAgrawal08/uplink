import { NextRequest, NextResponse, after } from "next/server";
import crypto from "crypto";
import { getDb } from "@/lib/mongodb";
import { performCleanup } from "../../../cleanup/route";
import { redis } from "@/lib/redis";
import { getPresignedDownloadUrl } from "@/lib/r2";
import { verifyPassword, anonymizeIp } from "@/lib/crypto";
import { consumeClassBQuota } from "@/lib/quota";
import { apiError } from "@/lib/api-utils";

// Safe preview mime-types allowlist
const SAFE_PREVIEW_TYPES = [
  "application/pdf",
  "image/jpeg",
  "image/png",
  "image/gif",
  "image/webp",
];

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await props.params;
    const body = await req.json().catch(() => ({}));
    const { password, preview } = body;

    // B8 FIX: Split x-forwarded-for by comma and take the first entry.
    const rawIp = req.headers.get("x-forwarded-for") || "127.0.0.1";
    const clientIp = rawIp.split(",")[0].trim() || "127.0.0.1";
    const ipHash = anonymizeIp(clientIp);

    const db = await getDb();

    // Fetch share document
    const share = await db.collection("shares").findOne({ $or: [{ shareId: id }, { downloadCode: id }] });
    if (!share) {
      return apiError("Share not found", 404);
    }

    // 1. Rate Limiting check.
    // B42 FIX: password failures and public authorizes use SEPARATE keys.
    // Previously both shared one counter with a threshold of 5, so ~6
    // concurrent legitimate authorizes on a public share tripped the
    // "too many failed attempts" lockout. Failure budget stays tight (5);
    // public throughput gets its own generous window (120 / 5 min).
    const failKey = `rate:download:fail:${ipHash}:${share.shareId}`;
    const failStr = await redis.get(failKey);
    const failures = typeof failStr === "string" ? parseInt(failStr, 10) : 0;
    if (failures > 5) {
      return apiError("Too many failed attempts. Locked out for 5 minutes.", 429);
    }

    if (!share.passwordHash) {
      const pubKey = `rate:download:pub:${ipHash}:${share.shareId}`;
      try {
        const hits = await redis.incr(pubKey);
        if (hits === 1) await redis.expire(pubKey, 300);
        if (hits > 120) {
          return apiError("Too many download requests. Locked out for 5 minutes.", 429);
        }
      } catch (limiterErr) {
        console.warn("public download rate-limiter unavailable; failing open:", limiterErr);
      }
    }

    const now = new Date();

    // 2. Expiry and status check (before password verification so dead links
    //    fail fast without burning a password attempt).
    if (new Date(share.expiresAt) < now || share.status === "EXPIRED") {
      if (share.status !== "EXPIRED" && share.status !== "DELETED" && share.status !== "PENDING_DELETE") {
        await db.collection("shares").updateOne({ shareId: share.shareId }, { $set: { status: "EXPIRED" } });
      }
      return apiError("This share link has expired", 410);
    }

    if (share.status !== "ACTIVE") {
      return apiError(`This share link is not active (${share.status})`, 400);
    }

    // 3. Password Verification (before any quota spend: failed guesses must
    //    not burn Class B operations — otherwise an attacker can exhaust the
    //    daily R2 budget with wrong passwords).
    if (share.passwordHash) {
      if (!password) {
        return NextResponse.json(
          { error: "Password required for this share", passwordRequired: true },
          { status: 401 }
        );
      }
      const isPasswordValid = await verifyPassword(password, share.passwordHash);
      if (!isPasswordValid) {
        const attempts = await redis.incr(failKey);
        if (attempts === 1) {
          await redis.expire(failKey, 300); // 5-minute window
        }
        return NextResponse.json(
          { error: "Incorrect password", passwordRequired: true },
          { status: 401 }
        );
      }
      await redis.del(failKey);
    }

    // 4. Atomic Download Counter and Limit Check
    // Inline preview requests (preview=true for safe types) must NOT burn a
    // download credit — otherwise merely opening the web page kills shares.
    const isSafePreview = SAFE_PREVIEW_TYPES.includes(share.mimeType);
    const wantPreview = preview === true && isSafePreview;

    let result = null;
    if (!wantPreview) {
      result = await db.collection("shares").findOneAndUpdate(
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
        // Limit exceeded or transition conflict. Check state.
        const refreshedShare = await db.collection("shares").findOne({ shareId: share.shareId });
        if (refreshedShare && refreshedShare.downloadsCount >= refreshedShare.downloadLimit) {
          await db.collection("shares").updateOne({ shareId: share.shareId }, { $set: { status: "EXPIRED" } });
          return apiError("Download limit exceeded for this file", 410);
        }
        return apiError("Download authorization failed", 400);
      }
    }

    // 5. Class B Operation Quota check — placed AFTER auth, expiry, and
    //    limit checks so failed guesses, dead links, and exhausted shares
    //    never spend R2 operations budget (B38).
    try {
      const classBApproved = await consumeClassBQuota();
      if (!classBApproved) {
        return apiError("Service is temporarily unavailable due to operations quota limit exhaustion.", 503);
      }
    } catch (quotaErr) {
      console.error("Fail-closed: Class B quota check error:", quotaErr);
      return apiError("Service is temporarily unavailable due to system quota validation failure.", 503);
    }

    // 6. Generate Presigned GET URL
    const downloadUrlExpiry = 3600; // 1h expiry for download link
    const downloadUrl = await getPresignedDownloadUrl(
      share.objectKey,
      downloadUrlExpiry,
      share.storageFilename,
      share.mimeType,
      wantPreview
    );

    // 6. Structured Log Event
    const userAgent = req.headers.get("user-agent") || "Unknown";
    const logEvent = {
      timestamp: now.toISOString(),
      requestId: `req_${crypto.randomUUID().replace(/-/g, "").substring(0, 16)}`,
      event: "DownloadAuthorized",
      shareId: id,
      preview: wantPreview,
      downloadsCount: result?.downloadsCount ?? share.downloadsCount,
      downloadLimit: result?.downloadLimit ?? share.downloadLimit,
      latencyMs: Date.now() - now.getTime(),
      ipHash,
      userAgentParsed: {
        raw: userAgent,
      },
      error: null,
    };
    console.log(JSON.stringify(logEvent));

    // Trigger background cleanup asynchronously to purge any expired uploads
    after(async () => {
      await performCleanup().catch(err => console.error("Background cleanup failed:", err));
    });

    // (No redis.del here: public authorizes consume from their own fixed
    // window, which expires on its own. Deleting on success is what caused
    // the old shared counter to never accumulate — see B42.)

    return NextResponse.json({
      downloadUrl,
      filename: share.filename,
      size: share.size,
      mimeType: share.mimeType,
      hashValue: share.hashValue,
      expiresAt: share.expiresAt,
    });
  } catch (error: unknown) {
    console.error("Error in POST /api/v1/share/[id]/authorize-download:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
