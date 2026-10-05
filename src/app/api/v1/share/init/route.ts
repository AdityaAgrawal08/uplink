import { NextRequest, NextResponse, after } from "next/server";
import crypto from "crypto";
import { getDb, initIndexes } from "@/lib/mongodb";
import { performCleanup } from "../../cleanup/route";
import { redis } from "@/lib/redis";
import { getPresignedUploadUrl, getPresignedMultipartUrls } from "@/lib/r2";
import {
  generateShareId,
  sanitizeFilename,
  hashPassword,
} from "@/lib/crypto";
import { reserveUploadQuota, releaseUploadQuotaWithRetry } from "@/lib/quota";
import { chargeIpReservation, releaseIpReservation } from "@/lib/reservation";
import { clientIpHash } from "@/lib/rooms";
import { apiError } from "@/lib/api-utils";

export async function POST(req: NextRequest) {
  let size = 0;
  let partsCount = 0;
  let isMultipart = false;
  let quotaReserved = false;
  let success = false;
  let redisIdempotencyKey: string | null = null;
  let ipHash = "";

  try {
    // Rate Limiting check
    // B8 FIX: split x-forwarded-for by comma and take the first entry
    // (client's real IP). clientIpHash additionally honors TRUST_PROXY
    // (finding 10): when no trusted proxy is in front, XFF is
    // attacker-controlled and is ignored for budgeting.
    ipHash = clientIpHash(req);
    const rateLimitKey = `rate:init:${ipHash}`;
    const attempts = await redis.incr(rateLimitKey);
    if (attempts === 1) {
      await redis.expire(rateLimitKey, 300); // 5-minute window
    }
    if (attempts > 10) {
      return apiError("Too many upload initialization requests. Locked out for 5 minutes.", 429);
    }

    const text = await req.text();
    if (text.length > 1024 * 100) { // 100 KB max for init metadata
      return apiError("Request body too large", 413);
    }
    let body: Record<string, unknown>;
    try {
      body = text ? JSON.parse(text) : {};
    } catch {
      return apiError("Request body must be valid JSON", 400);
    }

    const {
      filename,
      mimeType,
      hashValue,
      password,
      expiresInSeconds,
      downloadLimit,
      checksumCrc64nvme,
      isEncrypted,
    } = body as {
      filename?: unknown;
      mimeType?: unknown;
      hashValue?: unknown;
      password?: unknown;
      expiresInSeconds?: unknown;
      downloadLimit?: unknown;
      checksumCrc64nvme?: unknown;
      isEncrypted?: unknown;
    };
    size = Number(body.size as unknown);
    partsCount = Number(body.partsCount as unknown) || 0;

    // 1. Basic Validations
    if (!filename || typeof filename !== "string") {
      return apiError("Filename is required", 400);
    }
    if (typeof size !== "number" || isNaN(size) || size <= 0 || !Number.isSafeInteger(size)) {
      return apiError("File size must be a valid positive integer", 400);
    }
    if (typeof partsCount !== "number" || isNaN(partsCount) || partsCount < 0 || !Number.isSafeInteger(partsCount)) {
      return apiError("partsCount must be a valid non-negative integer", 400);
    }
    if (partsCount > 100) {
      return apiError("partsCount cannot exceed 100", 400);
    }
    if (!hashValue || typeof hashValue !== "string" || hashValue.length !== 64) {
      return apiError("Valid SHA-256 hashValue (64 chars hex) is required", 400);
    }

    isMultipart = partsCount > 1;

    // Enforce limits: Max 500 MB for multipart directories, 200 MB for guest single-part files
    const MAX_SIZE = isMultipart ? 500 * 1024 * 1024 : 200 * 1024 * 1024;
    if (size > MAX_SIZE) {
      return apiError(`File size exceeds ${isMultipart ? "500 MB directory" : "200 MB file"} limit`, 400);
    }

    // Expiry verification
    const DEFAULT_EXPIRY = 86400; // 24 hours
    const maxExpiry = 86400;
    let expirySec = expiresInSeconds !== undefined ? Number(expiresInSeconds) : DEFAULT_EXPIRY;
    if (isNaN(expirySec) || expirySec <= 0 || expirySec > maxExpiry) {
      expirySec = DEFAULT_EXPIRY;
    }

    // Download limit verification
    const defaultDownloadLimit = 10;
    let dlLimit = downloadLimit !== undefined ? Number(downloadLimit) : defaultDownloadLimit;
    if (isNaN(dlLimit) || dlLimit <= 0) {
      dlLimit = defaultDownloadLimit;
    }

    // 2. Idempotency Check
    const idempotencyKey = req.headers.get("idempotency-key");
    redisIdempotencyKey = idempotencyKey ? `idempotency:${idempotencyKey}` : null;

    if (redisIdempotencyKey) {
      const lockAcquired = await redis.set(redisIdempotencyKey, "PROCESSING", {
        nx: true,
        ex: 30,
      });

      if (lockAcquired === null) {
        const status = await redis.get(redisIdempotencyKey);
        if (status === "PROCESSING") {
          return apiError("A request with this Idempotency-Key is currently processing", 409);
        }
        if (status && typeof status === "object") {
          // Only replay the cached init if the referenced share is still
          // alive. The cache outlives share expiry (2h TTL vs 1h default),
          // so a blind replay could resurrect a DELETED/EXPIRED share and
          // hand the user a dead link on re-send.
          const cachedShareId = (status as { shareId?: string }).shareId;
          const live = cachedShareId
            ? await (await getDb()).collection("shares").findOne(
                { shareId: cachedShareId },
                { projection: { status: 1, expiresAt: 1 } }
              )
            : null;
          const terminal = ["EXPIRED", "DELETED", "PENDING_DELETE", "DELETE_FAILED"];
          const notExpired = !!live?.expiresAt && new Date(live.expiresAt as unknown as string) > new Date();
          if (live && !terminal.includes(live.status) && notExpired) {
            success = true;
            return NextResponse.json(status);
          }
          // Stale cache for a dead share — drop it and fall through to a
          // fresh initialization below.
          await redis.del(redisIdempotencyKey);
        }
      }
    }

    // Initialize MongoDB index checks on startup/first request
    if (process.env.NODE_ENV === "production") {
      await initIndexes();
    } else {
      initIndexes().catch(err => console.error("Background index initialization failed in dev:", err));
    }

    const db = await getDb();

    // 3. Quota Enforcement check (Milestone 5)
    // Estimate Class A operations:
    // If multipart: partsCount + 2 (Initiate + UploadParts + Complete)
    // If singlepart: 1 (PutObject)
    const estimatedClassAOps = isMultipart ? Number(partsCount) + 2 : 1;

    try {
      const quotaApproved = await reserveUploadQuota(size, estimatedClassAOps);
      if (!quotaApproved) {
        return apiError(
          "Storage capacity has been reached. New uploads are temporarily unavailable. Please wait while older files are removed automatically to free space.",
          503
        );
      }
      quotaReserved = true;
    } catch (quotaErr) {
      console.error("Fail-closed: Quota check error:", quotaErr);
      return apiError("Uploads are temporarily unavailable due to system quota validation failure.", 503);
    }

    // 4. Per-IP in-flight reservation budget (finding 4): a single IP cannot
    // hold more than ~1GB of pending upload reservations at once, so an
    // attacker cannot carpet-bomb init with large sizes to pin the storage
    // reservation ledger. Refused → refund the Mongo reservation and answer
    // 429 before any R2 presigning work.
    const ipReservationCharged = await chargeIpReservation(ipHash, size);
    if (!ipReservationCharged) {
      await releaseUploadQuotaWithRetry(size, estimatedClassAOps).catch(() => {});
      quotaReserved = false;
      return apiError("Too many uploads in progress from this IP. Try again later.", 429);
    }

    // 5. Share ID and Key construction
    // B59 FIX (finding 12): a client-supplied shareId is honored (the CLI's
    // resume flow re-sends the previous session's server-generated id), but
    // it must match the server's own id charset or the request is rejected:
    // the shareId is embedded in the R2 object key, and an arbitrary value
    // would smuggle path separators / control characters into object-storage
    // paths. Server-generated ids (22-char base64url) always match.
    const SHARE_ID_RE = /^[A-Za-z0-9_-]{10,128}$/;
    const rawShareId = typeof body.shareId === "string" ? body.shareId : "";
    if (rawShareId !== "" && !SHARE_ID_RE.test(rawShareId)) {
      return apiError("shareId must be 10-128 characters of A-Za-z0-9_-", 400);
    }
    const shareId = rawShareId || generateShareId();
    const storageFilename = sanitizeFilename(filename);
    const date = new Date();
    const year = date.getUTCFullYear();
    const month = String(date.getUTCMonth() + 1).padStart(2, "0");
    const objectKey = `uploads/${year}/${month}/${shareId}/${storageFilename}`;

    // Hash password if supplied
    let passwordHash = null;
    if (password && typeof password === "string") {
      passwordHash = await hashPassword(password);
    }

    const expiresAt = new Date(Date.now() + expirySec * 1000);
    const uploadExpiresAt = new Date(Date.now() + 2 * 3600 * 1000); // Upload session valid for 2h
    const uploadUrlExpiry = 7200; // Presigned URL valid for 2h (aligned with session expiry)

    let uploadId = "";
    let uploadUrl = null;
    let uploadUrls = null;

    // 4. Generate Presigned R2 Upload URLs (Single-part vs Multipart)
    if (isMultipart) {
      const parts = Number(partsCount);
      const mpDetails = await getPresignedMultipartUrls(objectKey, parts);
      uploadId = mpDetails.uploadId;
      uploadUrls = mpDetails.urls;
    } else {
      uploadId = `upload_${crypto.randomUUID().replace(/-/g, "")}`;
      uploadUrl = await getPresignedUploadUrl(
        objectKey,
        uploadUrlExpiry,
        (mimeType as string | undefined) || "application/octet-stream",
        hashValue
      );
    }

    // 5. Database Insert (Share & Upload Session)
    // B20 FIX: The old check-then-insert was a TOCTOU race — two concurrent
    // requests could both pass the findOne uniqueness check, then the second
    // insertOne would throw duplicate-key (11000) and return a 500. Now the
    // insert is retried with a freshly generated code on collision.
    const shareBase = {
      shareId,
      filename,
      storageFilename,
      size,
      mimeType: mimeType || "application/octet-stream",
      observedMimeType: null,
      etag: null,
      objectKey,
      hashAlgorithm: "SHA-256",
      hashValue,
      checksumCrc64nvme: checksumCrc64nvme || null,
      passwordHash,
      isEncrypted: isEncrypted === true,
      status: "CREATED",
      createdAt: date,
      expiresAt,
      downloadLimit: dlLimit,
      downloadsCount: 0,
      firstDownloadedAt: null,
      lastDownloadedAt: null,
      // Finding 4: the initiator's anonymized IP hash rides the share doc so
      // confirm/cleanup can refund the per-IP reservation counter without a
      // request context.
      initIpHash: ipHash,
      schemaVersion: 1,
      cleanupLockedUntil: null,
      cleanupWorkerId: null,
      retryCount: 0,
      lastRetryAt: null,
      nextRetryAt: null,
      lastErrorCode: null,
      lastErrorMessage: null,
    };

    const uploadSessionDoc = {
      uploadId,
      shareId,
      uploadExpiresAt,
      uploadUrlExpiresAt: new Date(Date.now() + uploadUrlExpiry * 1000),
      status: "PENDING",
      createdAt: date,
      isMultipart,
      partsCount: isMultipart ? Number(partsCount) : 1,
    };

    let shareInserted = false;
    let codeAttempts = 0;
    let downloadCode = "";
    while (!shareInserted && codeAttempts < 20) {
      downloadCode = "";
      for (let i = 0; i < 10; i++) {
        downloadCode += crypto.randomInt(0, 10).toString();
      }
      try {
        await db.collection("shares").insertOne({ ...shareBase, downloadCode });
        shareInserted = true;
      } catch (dbErr) {
        const err = dbErr as { code?: number };
        if (err.code === 11000) {
          // Duplicate downloadCode (or shareId) — retry with a new code.
          codeAttempts++;
          await new Promise(r => setTimeout(r, 50));
          continue;
        }
        throw dbErr;
      }
    }

    if (!shareInserted) {
      return apiError("Unique download code generation failed due to collision limits", 500);
    }

    await db.collection("upload_sessions").insertOne(uploadSessionDoc);

    const responseData = {
      shareId,
      uploadId,
      uploadUrl,
      uploadUrls,
      objectKey,
      filename,
      storageFilename,
      expiresAt: expiresAt.toISOString(),
      uploadExpiresAt: uploadExpiresAt.toISOString(),
    };

    // Cache final response in Redis with a TTL matching presigned URL expiration (2h) if idempotency key is used
    if (redisIdempotencyKey) {
      await redis.set(redisIdempotencyKey, responseData, { ex: uploadUrlExpiry });
    }

    // Trigger background cleanup asynchronously to purge any expired uploads
    after(async () => {
      await performCleanup().catch(err => console.error("Background cleanup failed:", err));
    });

    success = true;
    return NextResponse.json(responseData, { status: 201 });
  } catch (error: unknown) {
    console.error("Error in POST /api/v1/share/init:", error);
    if (quotaReserved) {
      // B3 FIX: Retry quota release so a transient failure cannot leak quota.
      const estimatedClassAOps = isMultipart ? partsCount + 2 : 1;
      await releaseUploadQuotaWithRetry(size, estimatedClassAOps);
      // Finding 4: refund the per-IP reservation counter too, or a failed
      // init would hold the IP's bytes hostage until the 2h TTL.
      await releaseIpReservation(ipHash, size).catch(err => console.error("Failed to release IP reservation:", err));
    }
    return apiError("Internal server error", 500);
  } finally {
    if (redisIdempotencyKey && !success) {
      await redis.del(redisIdempotencyKey).catch(err => console.error("Failed to clean up idempotency key on error:", err));
    }
  }
}
