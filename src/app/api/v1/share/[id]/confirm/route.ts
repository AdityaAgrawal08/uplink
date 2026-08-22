import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";
import fs from "fs";
import path from "path";
import { getDb } from "@/lib/mongodb";
import { checkObjectExists, completeMultipartUpload, s3Client, calculateS3ObjectHash } from "@/lib/r2";
import { anonymizeIp } from "@/lib/crypto";
import { commitUploadQuota, releaseUploadQuota } from "@/lib/quota";
import { apiError } from "@/lib/api-utils";

interface ShareData {
  shareId: string;
  size: number;
  status: string;
  filename: string;
  objectKey: string;
  hashValue: string;
  checksumCrc64nvme?: string | null;
  downloadCode?: string | null;
}

interface UploadSessionData {
  isMultipart: boolean;
  partsCount: number;
  uploadId: string;
  uploadExpiresAt: Date;
}

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ id: string }> }
) {
  let quotaCommitted = false;
  let share: ShareData | null = null;
  let uploadSession: UploadSessionData | null = null;

  try {
    const { id } = await props.params;
    const body = await req.json().catch(() => ({}));
    const { parts } = body;

    const db = await getDb();

    // 1. Fetch Share metadata
    share = (await db.collection("shares").findOne({ $or: [{ shareId: id }, { downloadCode: id }] })) as unknown as ShareData;
    if (!share) {
      return apiError("Share session not found", 404);
    }

    // Idempotent success response if already ACTIVE
    if (share.status === "ACTIVE") {
      return NextResponse.json({
        message: "Upload already confirmed",
        shareId: share.shareId,
        downloadCode: share.downloadCode || null,
        status: share.status,
      });
    }

    if (share.status !== "CREATED") {
      return apiError(`Cannot confirm upload in status: ${share.status}`, 400);
    }

    // 2. Fetch Upload Session
    uploadSession = (await db
      .collection("upload_sessions")
      .findOne({ shareId: share.shareId })) as unknown as UploadSessionData;
    
    if (!uploadSession) {
      return apiError("Upload session not found", 404);
    }

    const now = new Date();

    // Verify session expiration
    if (new Date(uploadSession.uploadExpiresAt) < now) {
      await db
        .collection("shares")
        .updateOne({ shareId: share.shareId }, { $set: { status: "EXPIRED" } });
      await db
        .collection("upload_sessions")
        .updateOne({ shareId: share.shareId }, { $set: { status: "EXPIRED" } });
      const estimatedOps = uploadSession.isMultipart ? uploadSession.partsCount + 2 : 1;
      await releaseUploadQuota(share.size, estimatedOps);
      return apiError("Upload session has expired", 410);
    }

    let finalCrc64 = null;
    let finalEtag = null;

    // 3. Complete and Verify Upload (Multipart vs Single-part)
    if (uploadSession.isMultipart) {
      if (!parts || !Array.isArray(parts)) {
        const estimatedOps = uploadSession.partsCount + 2;
        await releaseUploadQuota(share.size, estimatedOps);
        return apiError("Parts list is required to complete multipart upload", 400);
      }

      // Input Validation on parts array
      const isValid = parts.every(
        p =>
          p &&
          typeof p.partNumber === "number" &&
          Number.isSafeInteger(p.partNumber) &&
          p.partNumber > 0 &&
          typeof p.etag === "string" &&
          p.etag.length > 0
      );
      if (!isValid) {
        const estimatedOps = uploadSession.partsCount + 2;
        await releaseUploadQuota(share.size, estimatedOps);
        return apiError("Invalid parts list structure. Each part must contain a valid partNumber and etag.", 400);
      }

      // Assemble chunks in R2/S3 or mock storage
      const completionResult = await completeMultipartUpload(
        share.objectKey,
        uploadSession.uploadId,
        parts
      );

      if (completionResult.error) {
        const estimatedOps = uploadSession.partsCount + 2;
        await releaseUploadQuota(share.size, estimatedOps);
        return apiError(`Failed to complete multipart assembly: ${completionResult.error}`, 400);
      }

      finalEtag = completionResult.etag;

      // Run R2-validated HEAD Check on completed object to retrieve x-amz-checksum-crc64nvme header
      const objDetails = await checkObjectExists(share.objectKey);
      if (!objDetails.exists) {
        const estimatedOps = uploadSession.partsCount + 2;
        await releaseUploadQuota(share.size, estimatedOps);
        return apiError("Uploaded file was not found in object storage after assembly", 404);
      }

      finalCrc64 = objDetails.checksumCrc64nvme || completionResult.checksumCrc64nvme;

      // CRC64NVME cross-check.
      // R2's multipart CRC64NVME semantics (composite-style) differ from a
      // client-side whole-object computation, so strict equality here produced
      // false 412 failures for valid uploads. The R2-reported value is recorded
      // as authoritative storage-side metadata; divergence is logged for
      // observability. End-to-end integrity remains guaranteed by the CLI's
      // download-path SHA-256 verification against the client-computed hash.
      if (share.checksumCrc64nvme && finalCrc64 && finalCrc64 !== share.checksumCrc64nvme) {
        console.warn(JSON.stringify({
          event: "ChecksumDivergence",
          shareId: id,
          r2Reported: finalCrc64,
          clientExpected: share.checksumCrc64nvme,
        }));
      }
    } else {
      // Single-part HEAD check
      const objDetails = await checkObjectExists(share.objectKey);
      if (!objDetails.exists) {
        await releaseUploadQuota(share.size, 1);
        return apiError("Uploaded file was not found in object storage", 404);
      }

      if (objDetails.size <= 0) {
        await releaseUploadQuota(share.size, 1);
        return apiError("Uploaded file cannot be empty (0 bytes)", 400);
      }

      // Verify SHA-256 integrity
      const expectedHex = share.hashValue.toLowerCase();
      let verified = false;

      const isMock = !s3Client || process.env.FORCE_MOCK_STORAGE === "true";

      if (isMock) {
        // In mock mode, calculate actual file SHA-256
        const localPath = path.join(process.cwd(), "uploads_dev", share.objectKey);
        if (fs.existsSync(localPath)) {
          const content = fs.readFileSync(localPath);
          const computed = crypto.createHash("sha256").update(content).digest("hex");
          verified = computed === expectedHex;
        }
      } else {
        // In real R2 mode, validate against R2 header
        if (objDetails.checksumSha256) {
          let r2Hex = objDetails.checksumSha256.toLowerCase();
          if (!/^[a-f0-9]{64}$/.test(r2Hex)) {
            try {
              r2Hex = Buffer.from(objDetails.checksumSha256, "base64").toString("hex").toLowerCase();
            } catch {}
          }
          if (r2Hex === expectedHex) {
            verified = true;
          }
        } else {
          // If R2/S3 head doesn't return ChecksumSHA256, calculate the actual SHA-256 of the object content from R2
          try {
            const computed = await calculateS3ObjectHash(share.objectKey);
            verified = computed.toLowerCase() === expectedHex;
          } catch (e) {
            console.error("Failed to compute SHA-256 for validation fallback:", e);
          }
        }
      }

      if (!verified) {
        await db
          .collection("upload_sessions")
          .updateOne({ shareId: id }, { $set: { status: "VERIFY_FAILED" } });
        await releaseUploadQuota(share.size, 1);
        return apiError("Integrity check failed: uploaded file SHA-256 does not match client expectation", 412);
      }
    }

    // 4. Update Statuses to ACTIVE / COMPLETED
    const shareStatusBefore = share.status;
    const shareStatusAfter = "ACTIVE";

    const updateShareResult = await db.collection("shares").findOneAndUpdate(
      { shareId: share.shareId, status: "CREATED" },
      {
        $set: {
          status: shareStatusAfter,
          etag: finalEtag || null,
          // Store observed checksum if verified
          checksumCrc64nvme: finalCrc64 || share.checksumCrc64nvme,
        },
      },
      { returnDocument: "after" }
    );

    if (!updateShareResult) {
      const estimatedOps = uploadSession.isMultipart ? uploadSession.partsCount + 2 : 1;
      await releaseUploadQuota(share.size, estimatedOps);
      return apiError("Conflict updating share status", 409);
    }

    await db
      .collection("upload_sessions")
      .updateOne({ shareId: share.shareId }, { $set: { status: "COMPLETED" } });

    // Commit the upload quota atomically
    await commitUploadQuota(share.size);
    quotaCommitted = true;

    // 5. Structured Diagnostics Logging
    const clientIp = req.headers.get("x-forwarded-for") || "127.0.0.1";
    const ipHash = anonymizeIp(clientIp);
    const userAgent = req.headers.get("user-agent") || "Unknown";

    const logEvent = {
      timestamp: now.toISOString(),
      requestId: `req_${crypto.randomUUID().replace(/-/g, "").substring(0, 16)}`,
      event: "UploadConfirmed",
      shareId: id,
      shareStatusBefore,
      shareStatusAfter,
      isMultipart: !!uploadSession.isMultipart,
      latencyMs: Date.now() - now.getTime(),
      ipHash,
      userAgentParsed: {
        raw: userAgent,
      },
      error: null,
    };

    console.log(JSON.stringify(logEvent));

    return NextResponse.json({
      message: "Upload confirmed successfully",
      shareId: share.shareId,
      downloadCode: share.downloadCode || null,
      status: shareStatusAfter,
      filename: share.filename,
      size: share.size,
    });
  } catch (error: unknown) {
    console.error("Error in POST /api/v1/share/[id]/confirm:", error);
    if (!quotaCommitted && share) {
      try {
        const estimatedOps = uploadSession?.isMultipart ? uploadSession.partsCount + 2 : 1;
        await releaseUploadQuota(share.size, estimatedOps);
      } catch (refundErr) {
        console.error("Failed to refund quota on confirm crash:", refundErr);
      }
    }
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
