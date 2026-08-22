import { randomFillSync } from "crypto";

export async function GET() {
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
