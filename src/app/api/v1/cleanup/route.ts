import { NextRequest, NextResponse } from "next/server";
import crypto from "crypto";
import { getDb } from "@/lib/mongodb";
import { deleteObject } from "@/lib/r2";
import { recordDeleteQuota, releaseUploadQuotaWithRetry } from "@/lib/quota";
import { validateAdminAuth } from "@/lib/auth";
import { apiError } from "@/lib/api-utils";

export async function POST(req: NextRequest) {
  if (!validateAdminAuth(req)) {
    return apiError("Unauthorized access", 401);
  }
  return performCleanup();
}

export async function GET(req: NextRequest) {
  if (!validateAdminAuth(req)) {
    return apiError("Unauthorized access", 401);
  }
  return performCleanup();
}

export async function performCleanup() {
  try {
    const db = await getDb();
    const now = new Date();
    const workerId = `worker_${crypto.randomUUID().substring(0, 8)}`;
    const lockDurationMs = 5 * 60 * 1000; // 5 minutes
    const cleanupLockedUntil = new Date(Date.now() + lockDurationMs);

    // 1. Identify and lock expired shares
    // Candidates are:
    // - status in [ACTIVE, EXPIRED, CREATED, DELETE_FAILED] and expired
    // - OR status in PENDING_DELETE and lock has expired (crashed worker)
    const lockQuery = {
      $or: [
        {
          status: { $in: ["ACTIVE", "EXPIRED", "CREATED", "DELETE_FAILED"] },
          expiresAt: { $lt: now },
        },
        {
          status: "PENDING_DELETE",
          cleanupLockedUntil: { $lt: now },
        },
      ],
    };

    const lockUpdate = [
      {
        $set: {
          originalStatus: {
            $cond: {
              if: { $in: ["$status", ["PENDING_DELETE", "DELETE_FAILED"]] },
              then: { $ifNull: ["$originalStatus", "ACTIVE"] },
              else: "$status",
            },
          },
          status: "PENDING_DELETE",
          cleanupLockedUntil: cleanupLockedUntil,
          cleanupWorkerId: workerId,
          lastRetryAt: now,
          retryCount: { $add: [{ $ifNull: ["$retryCount", 0] }, 1] },
        },
      },
    ];

    // 1. Lock all eligible shares atomically
    const lockResult = await db.collection("shares").updateMany(lockQuery, lockUpdate);

    // 2. Fetch locked shares for this worker
    const lockedShares = await db
      .collection("shares")
      .find({
        status: "PENDING_DELETE",
        cleanupWorkerId: workerId,
      })
      .toArray();

    if (lockedShares.length === 0) {
      // 4. Also clean up old upload sessions metadata that are completed or expired
      const uploadSessionsCleanupResult = await db.collection("upload_sessions").deleteMany({
        $or: [
          { status: "COMPLETED", createdAt: { $lt: new Date(Date.now() - 24 * 3600 * 1000) } }, // Keep logs for 24h
          { status: { $in: ["PENDING", "UPLOADING", "VERIFY_FAILED", "EXPIRED"] }, uploadExpiresAt: { $lt: now } }
        ]
      });
      return NextResponse.json({
        message: "No expired shares found for cleanup",
        uploadSessionsCleaned: uploadSessionsCleanupResult.deletedCount
      });
    }

    const results = [];

    // 3. Process deletions
    // B51 FIX: isolate per-share failures so one bad share (R2 error, quota
    // DB error) cannot abort the sweep for every remaining share.
    for (const share of lockedShares) {
      try {
        const originalStatus = share.originalStatus || "ACTIVE";

        const deleteSuccess = await deleteObject(share.objectKey);
        
        if (deleteSuccess) {
          // Deletion succeeded: Update status to DELETED
          await db.collection("shares").updateOne(
            { shareId: share.shareId },
            {
              $set: {
                status: "DELETED",
                cleanupLockedUntil: null,
                cleanupWorkerId: null,
              },
              $unset: {
                downloadCode: "",
                originalStatus: "",
              },
            }
          );

          // Adjust quota system metrics based on original status
          if (originalStatus === "CREATED") {
            // If it was unconfirmed, release the reservation
            const uploadSession = await db.collection("upload_sessions").findOne({ shareId: share.shareId });
            const estimatedOps = uploadSession?.isMultipart ? uploadSession.partsCount + 2 : 1;
            await releaseUploadQuotaWithRetry(share.size, estimatedOps);
          } else {
            // If it was committed, decrement active storage bytes and record delete op
            await recordDeleteQuota(share.size);
          }

          results.push({ shareId: share.shareId, status: "DELETED" });
        } else {
          // Deletion failed: Transition to DELETE_FAILED and release lock
          await db.collection("shares").updateOne(
            { shareId: share.shareId },
            {
              $set: {
                status: "DELETE_FAILED",
                cleanupLockedUntil: null,
                cleanupWorkerId: null,
                lastErrorCode: "R2_DELETE_FAILED",
                lastErrorMessage: "Failed to delete file from R2 object storage",
              },
              $unset: {
                downloadCode: "",
              },
            }
          );

          // If the unconfirmed upload expired and R2 deletion failed, we still release the reservation
          // so storage is not leaked.
          if (originalStatus === "CREATED") {
            const uploadSession = await db.collection("upload_sessions").findOne({ shareId: share.shareId });
            const estimatedOps = uploadSession?.isMultipart ? uploadSession.partsCount + 2 : 1;
            await releaseUploadQuotaWithRetry(share.size, estimatedOps);
          }

          results.push({ shareId: share.shareId, status: "DELETE_FAILED" });
        }
      } catch (shareErr) {
        console.error(`Cleanup failed for share ${share.shareId}, continuing sweep:`, shareErr);
        results.push({ shareId: share.shareId, status: "CLEANUP_ERROR" });
      }
    }

    // 4. Also clean up old upload sessions metadata that are completed or expired
    const uploadSessionsCleanupResult = await db.collection("upload_sessions").deleteMany({
      $or: [
        { status: "COMPLETED", createdAt: { $lt: new Date(Date.now() - 24 * 3600 * 1000) } }, // Keep logs for 24h
        { status: { $in: ["PENDING", "UPLOADING", "VERIFY_FAILED", "EXPIRED"] }, uploadExpiresAt: { $lt: now } }
      ]
    });

    return NextResponse.json({
      message: "Cleanup task completed",
      workerId,
      sharesMatched: lockResult.matchedCount,
      sharesLocked: lockedShares.length,
      actions: results,
      uploadSessionsCleaned: uploadSessionsCleanupResult.deletedCount,
    });
  } catch (error: unknown) {
    console.error("Cleanup job error:", error);
    return apiError("Internal server error", 500);
  }
}
