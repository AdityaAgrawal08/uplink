import { NextRequest } from "next/server";
import crypto from "crypto";

// B31 FIX: Use a constant-time comparison for the admin bearer token.
// `authHeader === \`Bearer ${expected}\`` short-circuits on the first
// differing byte, leaking the key prefix through response-timing
// differences. timingSafeEqual avoids that.
export function validateAdminAuth(req: NextRequest): boolean {
  const expected = process.env.ADMIN_API_KEY;
  if (!expected) {
    console.error("ADMIN_API_KEY environment variable is not set.");
    return false;
  }
  const authHeader = req.headers.get("authorization") || "";
  const prefix = "Bearer ";
  if (!authHeader.startsWith(prefix)) return false;

  const provided = Buffer.from(authHeader.slice(prefix.length));
  const expectedBuf = Buffer.from(expected);
  // Lengths must match for timingSafeEqual; compare lengths in a way that
  // does not itself leak (both branches run a full comparison on equal-length
  // buffers; unequal lengths are rejected without content comparison).
  if (provided.length !== expectedBuf.length) return false;
  return crypto.timingSafeEqual(provided, expectedBuf);
}
