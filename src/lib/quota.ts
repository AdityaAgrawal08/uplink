import { getDb } from "./mongodb";

// Configuration limits and fallbacks
export const MAX_STORAGE_LIMIT = Number(process.env.R2_MAX_STORAGE_LIMIT) || 9.4 * 1024 * 1024 * 1024; // 9.4 GB
export const STORAGE_RECOVERY_MARGIN = Number(process.env.R2_STORAGE_RECOVERY_MARGIN) || 9.0 * 1024 * 1024 * 1024; // 9.0 GB
export const MAX_CLASS_A_LIMIT = Number(process.env.R2_MAX_CLASS_A_LIMIT) || 900000; // 900k ops / month
export const MAX_CLASS_B_LIMIT = Number(process.env.R2_MAX_CLASS_B_LIMIT) || 250000; // 250k ops / day

export interface QuotaEvent {
  timestamp: Date;
  type: "INFO" | "WARNING" | "CRITICAL" | "RECOVERY";
  message: string;
}

export interface QuotaDoc {
  _id: string;
  storageBytes: number;
  reservedBytes: number;
  classAOps: number;
  classBOps: number;
  classAResetAt: Date;
  classBResetAt: Date;
  quotaEvents: QuotaEvent[];
}

function getNextMonthStart(d: Date): Date {
  const date = new Date(d);
  date.setUTCMonth(date.getUTCMonth() + 1);
  date.setUTCDate(1);
  date.setUTCHours(0, 0, 0, 0);
  return date;
}

function getNextDayStart(d: Date): Date {
  const date = new Date(d);
  date.setUTCDate(date.getUTCDate() + 1);
  date.setUTCHours(0, 0, 0, 0);
  return date;
}

// Fetch the quota state and perform atomic resets if necessary
export async function getQuotaState(): Promise<QuotaDoc> {
  const db = await getDb();
  const now = new Date();

  // Try to find the quota doc
  let doc = await db.collection<QuotaDoc>("quotas").findOne({ _id: "r2_quota" });

  if (!doc) {
    // Initialize if not present
    const classAResetAt = getNextMonthStart(now);
    const classBResetAt = getNextDayStart(now);

    const initialDoc: QuotaDoc = {
      _id: "r2_quota",
      storageBytes: 0,
      reservedBytes: 0,
      classAOps: 0,
      classBOps: 0,
      classAResetAt,
      classBResetAt,
      quotaEvents: [
        {
          timestamp: now,
          type: "INFO",
          message: "Quota system initialized successfully.",
        },
      ],
    };

    try {
      await db.collection<QuotaDoc>("quotas").insertOne(initialDoc);
      return initialDoc;
    } catch {
      // If insertion fails due to race, find again
      doc = await db.collection<QuotaDoc>("quotas").findOne({ _id: "r2_quota" });
      if (!doc) {
        throw new Error("Quota document initialization failed (fail-closed)");
      }
    }
  }

  // Check if reset period is hit
  const setFields: Record<string, unknown> = {};
  const newEvents: QuotaEvent[] = [];

  if (now >= new Date(doc.classAResetAt)) {
    setFields.classAOps = 0;
    setFields.classAResetAt = getNextMonthStart(now);
    newEvents.push({
      timestamp: now,
      type: "INFO",
      message: `Monthly Class A operational counter reset. Previous usage: ${doc.classAOps}`,
    });
  }

  if (now >= new Date(doc.classBResetAt)) {
    setFields.classBOps = 0;
    setFields.classBResetAt = getNextDayStart(now);
    newEvents.push({
      timestamp: now,
      type: "INFO",
      message: `Daily Class B operational counter reset. Previous usage: ${doc.classBOps}`,
    });
  }

  if (Object.keys(setFields).length > 0) {
    const pushUpdate = newEvents.length > 0 ? { quotaEvents: { $each: newEvents, $slice: -50 } } : undefined;
    const res = await db.collection<QuotaDoc>("quotas").findOneAndUpdate(
      { _id: "r2_quota" },
      {
        $set: setFields,
        ...(pushUpdate ? { $push: pushUpdate } : {}),
      } as unknown as import("mongodb").UpdateFilter<QuotaDoc>,
      { returnDocument: "after" }
    );
    if (!res) {
      throw new Error("Failed to reset quota limits (fail-closed)");
    }
    return res;
  }

  return doc;
}

// Log a quota-related alert event (capped at 50 events)
export async function logQuotaEvent(type: QuotaEvent["type"], message: string): Promise<void> {
  try {
    const db = await getDb();
    console.log(`[Quota Event] [${type}] ${message}`);
    await db.collection<QuotaDoc>("quotas").updateOne(
      { _id: "r2_quota" },
      {
        $push: {
          quotaEvents: {
            $each: [{ timestamp: new Date(), type, message }],
            $slice: -50,
          },
        },
      }
    );
  } catch (err) {
    console.error("Failed to log quota event:", err);
  }
}

// Reserve storage space and Class A operations for a new upload atomically
export async function reserveUploadQuota(fileSize: number, estimatedClassAOps: number): Promise<boolean> {
  if (
    typeof fileSize !== "number" ||
    isNaN(fileSize) ||
    fileSize <= 0 ||
    !Number.isSafeInteger(fileSize) ||
    typeof estimatedClassAOps !== "number" ||
    isNaN(estimatedClassAOps) ||
    estimatedClassAOps <= 0 ||
    !Number.isSafeInteger(estimatedClassAOps)
  ) {
    throw new Error("Invalid quota reservation parameters: must be positive safe integers");
  }

  const db = await getDb();
  
  // Ensure resets are handled first
  const state = await getQuotaState();

  // Storage recovery margin verification. If currently in quota-restricted mode,
  // we must fall back below the recovery margin (STORAGE_RECOVERY_MARGIN) before resuming.
  const currentTotal = state.storageBytes + state.reservedBytes;
  if (currentTotal >= MAX_STORAGE_LIMIT) {
    await logQuotaEvent("WARNING", `Upload blocked: Max storage limit reached (${(currentTotal / (1024**3)).toFixed(2)} GB).`);
    return false;
  }

  // Atomic transactional checkout using findOneAndUpdate query selection constraints
  const res = await db.collection<QuotaDoc>("quotas").findOneAndUpdate(
    {
      _id: "r2_quota",
      $expr: {
        $and: [
          { $lte: [{ $add: ["$storageBytes", "$reservedBytes", fileSize] }, MAX_STORAGE_LIMIT] },
          { $lte: [{ $add: ["$classAOps", estimatedClassAOps] }, MAX_CLASS_A_LIMIT] },
        ],
      },
    },
    {
      $inc: {
        reservedBytes: fileSize,
        classAOps: estimatedClassAOps,
      },
    },
    { returnDocument: "after" }
  );

  if (!res) {
    // Determine which limit was hit
    if (state.storageBytes + state.reservedBytes + fileSize > MAX_STORAGE_LIMIT) {
      await logQuotaEvent(
        "CRITICAL",
        `Upload blocked: Requested file size (${(fileSize / (1024**2)).toFixed(2)} MB) would exceed MAX_STORAGE_LIMIT.`
      );
    } else {
      await logQuotaEvent(
        "CRITICAL",
        `Upload blocked: Class A operations quota exhausted or would be exceeded. Current: ${state.classAOps}, Estimated increment: ${estimatedClassAOps}.`
      );
    }
    return false;
  }

  // Check if this reservation crossed the storage limit warning
  const newTotal = res.storageBytes + res.reservedBytes;
  if (newTotal >= MAX_STORAGE_LIMIT) {
    await logQuotaEvent("WARNING", `Storage threshold reached! Current: ${(newTotal / (1024**3)).toFixed(2)} GB. Uploads disabled.`);
  }

  return true;
}

// Commit the reserved storage size to actual storage bytes when upload is confirmed
export async function commitUploadQuota(fileSize: number): Promise<void> {
  if (typeof fileSize !== "number" || isNaN(fileSize) || fileSize <= 0 || !Number.isSafeInteger(fileSize)) {
    throw new Error("Invalid commit parameters: fileSize must be a positive safe integer");
  }

  const db = await getDb();
  const res = await db.collection<QuotaDoc>("quotas").findOneAndUpdate(
    { _id: "r2_quota" },
    {
      $inc: {
        storageBytes: fileSize,
        reservedBytes: -fileSize,
      },
    },
    { returnDocument: "after" }
  );

  if (res) {
    const total = res.storageBytes + res.reservedBytes;
    console.log(`[Quota Commit] Storage: ${(res.storageBytes / (1024**2)).toFixed(2)} MB, Reserved: ${(res.reservedBytes / (1024**2)).toFixed(2)} MB, Total: ${(total / (1024**2)).toFixed(2)} MB`);
  }
}

// Release/refund quota reservation if upload is cancelled, aborted, or expired
export async function releaseUploadQuota(fileSize: number, estimatedClassAOps: number): Promise<void> {
  if (
    typeof fileSize !== "number" ||
    isNaN(fileSize) ||
    fileSize <= 0 ||
    !Number.isSafeInteger(fileSize) ||
    typeof estimatedClassAOps !== "number" ||
    isNaN(estimatedClassAOps) ||
    estimatedClassAOps <= 0 ||
    !Number.isSafeInteger(estimatedClassAOps)
  ) {
    throw new Error("Invalid release parameters: must be positive safe integers");
  }

  const db = await getDb();
  
  // Guard decrements below 0
  const state = await getQuotaState();
  const refundSize = Math.min(fileSize, state.reservedBytes);
  const refundOps = Math.min(estimatedClassAOps, state.classAOps);

  const res = await db.collection<QuotaDoc>("quotas").findOneAndUpdate(
    { _id: "r2_quota" },
    {
      $inc: {
        reservedBytes: -refundSize,
        classAOps: -refundOps,
      },
    },
    { returnDocument: "after" }
  );

  if (res) {
    const currentTotal = res.storageBytes + res.reservedBytes;
    // Check if recovery threshold is crossed
    if (currentTotal < STORAGE_RECOVERY_MARGIN && state.storageBytes + state.reservedBytes >= STORAGE_RECOVERY_MARGIN) {
      await logQuotaEvent(
        "RECOVERY",
        `Storage recovered below safety margin. Current: ${(currentTotal / (1024**3)).toFixed(2)} GB. Uploads re-enabled.`
      );
    }
  }
}

// Atomically check and consume 1 Class B operation quota for reads
export async function consumeClassBQuota(): Promise<boolean> {
  const db = await getDb();

  // Ensure resets are handled first
  const state = await getQuotaState();

  if (state.classBOps >= MAX_CLASS_B_LIMIT) {
    await logQuotaEvent("WARNING", `Class B operations limit hit (${state.classBOps}/${MAX_CLASS_B_LIMIT}). Reads restricted.`);
    return false;
  }

  const res = await db.collection<QuotaDoc>("quotas").findOneAndUpdate(
    {
      _id: "r2_quota",
      $expr: {
        $lte: [{ $add: ["$classBOps", 1] }, MAX_CLASS_B_LIMIT],
      },
    },
    {
      $inc: {
        classBOps: 1,
      },
    }
  );

  return !!res;
}

// Decrement storage bytes and increment Class A operations count on file deletions
export async function recordDeleteQuota(fileSize: number): Promise<void> {
  const db = await getDb();
  
  const state = await getQuotaState();
  // B11 FIX: Simplify — Math.max(0, ...) was dead code since fileSize is
  // validated upstream to be positive and storageBytes >= fileSize by design.
  const sizeDiff = Math.min(fileSize, state.storageBytes);

  const res = await db.collection<QuotaDoc>("quotas").findOneAndUpdate(
    { _id: "r2_quota" },
    {
      $inc: {
        storageBytes: -sizeDiff,
        classAOps: 1, // Purging from R2 counts as 1 Class A DeleteObject operation
      },
    },
    { returnDocument: "after" }
  );

  if (res) {
    const currentTotal = res.storageBytes + res.reservedBytes;
    if (currentTotal < STORAGE_RECOVERY_MARGIN && state.storageBytes + state.reservedBytes >= STORAGE_RECOVERY_MARGIN) {
      await logQuotaEvent(
        "RECOVERY",
        `Storage recovered below safety margin after file deletion. Current: ${(currentTotal / (1024**3)).toFixed(2)} GB. Uploads re-enabled.`
      );
    }
  }
}
