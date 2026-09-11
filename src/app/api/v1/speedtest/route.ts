import { randomFillSync } from "crypto";
import { redis } from "@/lib/redis";
import { anonymizeIp } from "@/lib/crypto";

export async function GET(req: Request) {
  // B19 FIX: Add rate limiting to speedtest endpoint. Without this,
  // abusers could generate unlimited random data, consuming server CPU
  // and bandwidth. Limit: 10 requests per 5 minutes per IP.
  const rawIp = req.headers.get("x-forwarded-for") || "127.0.0.1";
  const ipHash = anonymizeIp(rawIp.split(",")[0].trim() || "127.0.0.1");
  const rateKey = `rate:speed:${ipHash}`;
  try {
    const hits = await redis.incr(rateKey);
    if (hits === 1) await redis.expire(rateKey, 300);
    if (hits > 10) {
      return new Response(JSON.stringify({ error: "Rate limited" }), {
        status: 429,
        headers: { "Content-Type": "application/json" },
      });
    }
  } catch {
    // Fail open if rate limiter is unavailable.
  }

  const buf = new Uint8Array(262144); // 256 KB
  // Note: crypto.getRandomValues is capped at 65,536 bytes by the Web Crypto spec
  // and throws QuotaExceededError beyond it. randomFillSync has no such limit.
  randomFillSync(buf);
  return new Response(buf, {
    headers: {
      "Content-Type": "application/octet-stream",
      "Content-Length": "262144",
      "Cache-Control": "no-store, no-cache, must-revalidate",
    },
  });
}
