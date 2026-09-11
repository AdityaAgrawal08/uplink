import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { getObjectText } from "@/lib/r2";
import { verifyPassword, anonymizeIp } from "@/lib/crypto";
import { consumeClassBQuota } from "@/lib/quota";
import { redis } from "@/lib/redis";
import { apiError } from "@/lib/api-utils";

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
    const rawIp = req.headers.get("x-forwarded-for") || "127.0.0.1";
    const ipHash = anonymizeIp(rawIp.split(",")[0].trim() || "127.0.0.1");
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
    if ((share as { isEncrypted?: boolean }).isEncrypted) {
      return apiError("Encrypted shares cannot be previewed as text", 400);
    }

    if (share.passwordHash) {
      if (!password) {
        return NextResponse.json({ error: "Password required" }, { status: 401 });
      }
      const isValid = await verifyPassword(password, share.passwordHash);
      if (!isValid) {
        return NextResponse.json({ error: "Incorrect password" }, { status: 401 });
      }
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

    const text = await getObjectText(share.objectKey);
    return NextResponse.json({ text: text.slice(0, 100000) });
  } catch (err: unknown) {
    const errMsg = err instanceof Error ? err.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
